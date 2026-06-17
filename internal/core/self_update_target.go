package core

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/wnstify/wdm/pkg/types"
)

// This file is the binary self-update writability gate: can wdm replace the
// current binary without privilege escalation? That question is answered before
// any download or staging is worth attempting.
// The gate lives in internal/core (not internal/release) because it is the
// layer that knows os.Executable and the XDG on-disk layout (PRD §14: prefer
// a user-local binary path such as ~/.local/bin/wdm). internal/release stays
// the network+verify+stage owner and carries no executable-path or XDG
// knowledge; the two halves compose in ApplySelfUpdate, which runs the gate
// before staging.
// PRD §11 / §14 invariant: wdm NEVER invokes sudo or any privilege
// escalation. When the resolved executable lives in a directory wdm cannot
// write to, the gate refuses with a typed usage error (exit 2) carrying a
// manual-install hint pointing at the user-local path — it does not try to
// elevate, and it does not stage.

// manualInstallHint is the operator guidance attached when self-update is
// refused because the install target is not user-writable. It names the
// PRD §14 user-local path and the manual download/verify/install flow so
// the user has a clear next action that never needs sudo from wdm.
const manualInstallHint = "wdm cannot replace its binary at this location without elevated " +
	"privileges; install wdm to a user-writable path such as ~/.local/bin/wdm " +
	"and re-run, or update it manually following SECURITY.md"

// selfUpdateTarget describes the resolved binary install target and whether
// wdm may replace it without privilege escalation. It is the gate's
// structured outcome so the apply path can stage into a sibling of Dir and
// promote into Path.
type selfUpdateTarget struct {
	// Path is the symlink-resolved absolute path of the running
	// executable — the file a self-update would replace.
	Path string

	// Dir is filepath.Dir(Path): the directory the replacement happens in
	// and the directory the staging sibling is promoted within (an atomic
	// rename requires the staged file and the live binary share a
	// filesystem, which a same-directory staging guarantees).
	Dir string
}

// resolveSelfUpdateTarget resolves the running executable to its real path,
// confirms wdm can write to the directory holding it, and returns the target.
// It is the gate: it fails closed with a typed
// [types.ErrCodeUsageValidation] error (exit 2) and the [manualInstallHint]
// when the target directory is not user-writable, NEVER attempting any
// privilege escalation (PRD §11, §14).
// executablePath and resolveSymlinks are seams so tests drive the gate
// without depending on the test binary's own install location. Production
// passes [os.Executable] and [filepath.EvalSymlinks].
func resolveSelfUpdateTarget(
	executablePath func() (string, error),
	resolveSymlinks func(string) (string, error),
) (selfUpdateTarget, error) {
	exe, err := executablePath()
	if err != nil {
		return selfUpdateTarget{}, types.WrapError(
			types.ErrCodeGeneric,
			"could not determine the wdm executable path",
			"",
			err,
		)
	}

	// Resolve symlinks so a ~/.local/bin/wdm symlink pointing at a read-only
	// managed location (or vice versa) is judged by the real file's directory,
	// not the link's (PRD §12/§13 resolve-symlinks-before-write posture,
	// applied to the binary itself).
	resolved, err := resolveSymlinks(exe)
	if err != nil {
		return selfUpdateTarget{}, types.WrapError(
			types.ErrCodeGeneric,
			"could not resolve the wdm executable path",
			"",
			err,
		)
	}

	dir := filepath.Dir(resolved)
	if err := requireWritableDir(dir); err != nil {
		return selfUpdateTarget{}, err
	}

	return selfUpdateTarget{Path: resolved, Dir: dir}, nil
}

// requireWritableDir reports whether wdm can create a file in dir by probing
// it — a create-then-remove of a uniquely named temp file. A real probe is
// the honest signal for "can I replace the binary here without sudo?": it
// accounts for ownership, mode, mounts, and read-only filesystems that a
// path-prefix heuristic would miss.
// A permission failure (EACCES / EROFS surfaced as fs.ErrPermission) is the
// gate's fail-closed refusal: a typed usage-validation error (exit 2) with
// the [manualInstallHint], NEVER an attempt to elevate. The probe file is
// removed on every success path so the directory is left untouched.
func requireWritableDir(dir string) error {
	probe, err := os.CreateTemp(dir, ".wdm-selfupdate-probe-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return types.WrapError(
				types.ErrCodeUsageValidation,
				"the wdm install location is not user-writable",
				manualInstallHint,
				err,
			)
		}
		if errors.Is(err, os.ErrNotExist) {
			return types.WrapError(
				types.ErrCodeUsageValidation,
				"the wdm install location does not exist",
				manualInstallHint,
				err,
			)
		}
		// Any other probe failure (e.g. read-only filesystem reported as a
		// non-permission errno) still means wdm cannot stage a replacement
		// here; refuse fail-closed rather than proceed toward a doomed replace.
		return types.WrapError(
			types.ErrCodeUsageValidation,
			"the wdm install location cannot be written",
			manualInstallHint,
			err,
		)
	}

	name := probe.Name()
	_ = probe.Close()   //nolint:errcheck // probe file is removed next; close error is not actionable.
	_ = os.Remove(name) //nolint:errcheck // best-effort cleanup of the probe file.
	return nil
}
