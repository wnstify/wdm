package security

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wnstify/wdm/pkg/types"
)

// SecretFileMode is the canonical Unix mode bits for secret-bearing
// files — `.env` and any catalog `additional_files` entry that carries
// rendered secret values. The mode is `0o600` — owner read+write, no group,
// no world. It is exported so callers and tests reference the canonical
// constant rather than a hard-coded octal literal that would drift independently.
// `docker-compose.yml` is NOT a secret-bearing file (Compose templates
// carry `${VAR}` references, not literal secret values per PRD §11 /
// non-secret-bearing permissions.
const SecretFileMode os.FileMode = 0o600

// ValidateSecretFileMode returns a `*types.Error` with
// [types.ErrCodePermissionDenied] when mode does not match
// [SecretFileMode] exactly. The check is strict equality, not "no broader
// than": a stricter-than-0o600 mode (e.g. `0o400` read-only) is also invalid
// for a writable secret artifact that must be rewritten on update.
// Callers SHOULD pass `info.Mode.Perm` so non-permission bits
// (setuid, setgid, sticky, [os.ModeDir]) are masked off before the
// comparison — a full [os.FileMode] with type bits fails this check
// even when the permission bits are `0o600`.
// Used by tests asserting post-create and post-rename file modes and by
// `internal/state`'s atomic write composition to re-verify that POSIX
// `rename(2)` preserved the inode's mode bits.
func ValidateSecretFileMode(mode os.FileMode) error {
	if mode != SecretFileMode {
		return types.WrapError(
			types.ErrCodePermissionDenied,
			"secret file mode is not 0o600",
			"secret-bearing files must be mode 0o600; verify the file's permissions",
			fmt.Errorf("got mode %#o, want %#o", mode, SecretFileMode),
		)
	}
	return nil
}

// CreateSecretFile opens path with `O_CREATE|O_WRONLY|O_EXCL` at
// [SecretFileMode], then post-open `fchmod(2)`s the result to
// [SecretFileMode]. The second step defends against an exotic process
// umask (e.g. `0o400` masking owner-read) that would otherwise strip
// bits from the kernel-applied `mode & ~umask` during `open(2)`. The
// returned `*os.File` is open for writing with zero bytes written; the
// caller owns every subsequent step (write, fsync, close).
// PACKAGE INVARIANT — NO ONE-SHOT WRITE HELPER:
// This package deliberately exposes no one-shot
// `WriteSecretFile(path, data) error` helper, and `CreateSecretFile`
// deliberately does not write, fsync, or close the file it returns.
// Every secret-bearing write MUST compose through `internal/state`'s
// atomic `tmp+fsync+rename+parent-fsync` sequence per
// so a crash mid-write never leaves a partially-written secret-bearing
// file on disk. A one-shot helper would let future callers ship
// secrets to disk without that crash-safety contract — the absence of
// the helper is itself the contract.
// Path safety is caller-owned. The caller MUST pre-validate path via
// [SafeJoin] / [EnsureWithinRoot] before calling this helper;
// `CreateSecretFile` does NOT re-validate parent-chain symlinks,
// system-directory rejection, or stack-root containment. `O_EXCL`
// structurally rejects a pre-existing file or symlink at the target
// itself, but an intermediate symlinked directory in path is NOT
// caught here — that is [SafeJoin]'s and [EnsureWithinRoot]'s domain
// upstream.
// Error surface (all returns are `*types.Error`):
//   - `fs.ErrPermission` from `open(2)` → [types.ErrCodePermissionDenied]
//   - `fs.ErrExist` (`O_EXCL` conflict) → [types.ErrCodeGeneric] with
//     a hint to remove the leftover file and retry
//   - any other open failure → [types.ErrCodeGeneric]
//   - `f.Chmod` failure → [types.ErrCodeGeneric]; the partial file is
//     best-effort `Close`d and `Remove`d before returning so the caller
//     never observes an on-disk artifact with broader-than-`0o600`
//     permissions
//
// Underlying causes (`fs.ErrPermission`, `fs.ErrExist`, etc.) stay
// reachable via [errors.Is] through the `*types.Error` wrap, so
// diagnostic chains and structured-logging redactors see the original
// syscall error.
func CreateSecretFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, SecretFileMode) //nolint:gosec // G304: caller pre-validates path via SafeJoin / EnsureWithinRoot per the package contract pinned in the godoc above
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrPermission):
			return nil, types.WrapError(
				types.ErrCodePermissionDenied,
				"could not create secret file",
				"ensure the parent directory exists and the current user has write permission",
				err,
			)
		case errors.Is(err, fs.ErrExist):
			return nil, types.WrapError(
				types.ErrCodeGeneric,
				"secret file already exists",
				"remove the leftover file and retry (a previous crash may have left a partial artifact)",
				err,
			)
		default:
			return nil, types.WrapError(
				types.ErrCodeGeneric,
				"could not create secret file",
				"verify the path's parent directory exists and is on a writable filesystem",
				err,
			)
		}
	}
	if chmodErr := f.Chmod(SecretFileMode); chmodErr != nil {
		// The on-disk artifact may have broader-than-0o600 perms if
		// open(2) applied `mode & ~umask` and Chmod could not narrow
		// it. Close and remove best-effort before returning so the
		// caller never sees a leaky file.
		_ = f.Close()       //nolint:errcheck // best-effort cleanup after Chmod failure; primary error is chmodErr
		_ = os.Remove(path) //nolint:errcheck // best-effort cleanup after Chmod failure; primary error is chmodErr
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"could not enforce secret file mode",
			"ensure the underlying filesystem supports chmod (some FUSE mounts may not)",
			chmodErr,
		)
	}
	return f, nil
}

// RejectInsecureParent stats the parent directory of path and returns
// a `*types.Error` if that parent is group- or world-writable. A
// group/world-writable parent lets any user with write access to that
// directory rename, replace, or hardlink a secret-bearing file out
// from under wdm between create and rename (TOCTOU on the directory
// entry), so even a `0o600`-mode child file is not actually protected.
// check.
// path itself need not exist; only the parent is stat'd. path SHOULD
// be absolute — `filepath.Dir` of a relative path returns `"."` (the
// current working directory), which is rarely intended; the
// caller-owned path-safety contract (see [CreateSecretFile]) implies
// absolute paths resolved through [SafeJoin] upstream.
// The check is strict: `mode & 0o022 != 0` rejects any group-write
// (`0o020`) or world-write (`0o002`) bit. Sticky-on-world-writable
// (e.g. `/tmp` at `1777`) is still rejected — wdm secret files must
// not live directly under `/tmp` regardless of the sticky bit
// preventing cross-user rename, because timing-side-channel and
// hardlink-target observation remain practical.
// Error surface (all returns are `*types.Error`):
//   - `fs.ErrPermission` from the parent stat →
//     [types.ErrCodePermissionDenied]
//   - any other stat failure (parent missing, parent is a regular
//     file, etc.) → [types.ErrCodeGeneric]
//   - parent exists, is a directory, and has group/world write bits
//     set → [types.ErrCodePermissionDenied] with a hint suggesting
//     `chmod 700 <parent>`
func RejectInsecureParent(path string) error {
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return types.WrapError(
				types.ErrCodePermissionDenied,
				"could not stat parent directory of secret file",
				"ensure the current user can read the parent directory",
				err,
			)
		}
		return types.WrapError(
			types.ErrCodeGeneric,
			"could not stat parent directory of secret file",
			"verify the parent directory exists and is reachable",
			err,
		)
	}
	if !info.IsDir() {
		return types.WrapError(
			types.ErrCodeGeneric,
			"parent of secret file is not a directory",
			"verify the path is laid out as <dir>/<file>",
			fmt.Errorf("stat %q: not a directory", parent),
		)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return types.WrapError(
			types.ErrCodePermissionDenied,
			"parent directory of secret file is group- or world-writable",
			fmt.Sprintf("set the parent directory to a mode without group/world write bits (e.g. `chmod 700 %s`)", parent),
			fmt.Errorf("parent %q has mode %#o; group/world write bits forbidden for secret-bearing files", parent, info.Mode().Perm()),
		)
	}
	return nil
}
