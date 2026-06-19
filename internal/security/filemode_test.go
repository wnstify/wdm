package security_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// TestSecretFileMode_Is0600 pins the canonical secret-bearing file mode at
// `0o600`. A change to the constant would silently weaken the contract for
// every caller — the test catches such a change at the package boundary.
func TestSecretFileMode_Is0600(t *testing.T) {
	t.Parallel()

	assert.Equal(t, os.FileMode(0o600), security.SecretFileMode,
		"SecretFileMode must be 0o600")
}

// TestValidateSecretFileMode_Accepts0600 covers the happy path:
// exactly `0o600` passes validation.
func TestValidateSecretFileMode_Accepts0600(t *testing.T) {
	t.Parallel()

	assert.NoError(t, security.ValidateSecretFileMode(0o600),
		"ValidateSecretFileMode must accept exactly 0o600")
}

// TestValidateSecretFileMode_RejectsNon0600 covers every other mode
// — broader (`0o644`, `0o755`, `0o777`) AND stricter (`0o000`,
// `0o400`, `0o500`). The contract is strict equality, not "no
// broader than"; there is no stricter-acceptable carve-out.
func TestValidateSecretFileMode_RejectsNon0600(t *testing.T) {
	t.Parallel()

	cases := []os.FileMode{
		0o000, 0o400, 0o500, // stricter than 0o600
		0o644, 0o666, 0o700, 0o755, 0o777, // broader than 0o600
	}
	for _, mode := range cases {
		t.Run(fmt.Sprintf("reject_%#o", mode), func(t *testing.T) {
			t.Parallel()

			err := security.ValidateSecretFileMode(mode)
			require.Error(t, err)
			assert.True(t, types.IsCode(err, types.ErrCodePermissionDenied),
				"non-0o600 mode must surface as ErrCodePermissionDenied")
		})
	}
}

// TestCreateSecretFile_CreatesWithMode0600 is the primary happy
// path: the helper produces a file whose post-create mode is
// exactly `SecretFileMode`. Verifies via [os.Stat] (the standalone
// `info.Mode.Perm` read) so this test is the canonical evidence
// for the secret-file mode contract at the security-helper layer.
func TestCreateSecretFile_CreatesWithMode0600(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "secret.env")

	f, err := security.CreateSecretFile(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, security.SecretFileMode, info.Mode().Perm(),
		"CreateSecretFile must produce a file with mode 0o600")
	assert.False(t, info.IsDir(), "CreateSecretFile must produce a regular file")
}

// TestCreateSecretFile_RejectsExistingFile asserts the `O_EXCL`
// semantics: a second `CreateSecretFile` against the same path
// fails. This prevents a TOCTOU where two install attempts race
// to create the same `.env.tmp` — the second one MUST fail rather
// than truncate a half-written secret file from the first.
// The failure surfaces as [types.ErrCodeGeneric] (NOT
// [types.ErrCodePermissionDenied] — existence-conflict is not a
// permission issue), with a hint pointing at the leftover-file
// recovery and the underlying [fs.ErrExist] preserved in the chain.
func TestCreateSecretFile_RejectsExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "secret.env")

	f1, err := security.CreateSecretFile(path)
	require.NoError(t, err)
	require.NoError(t, f1.Close())

	f2, err := security.CreateSecretFile(path)
	require.Error(t, err, "O_EXCL must reject existing files")
	assert.Nil(t, f2, "no file handle should be returned on EEXIST")
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"O_EXCL conflict surfaces as ErrCodeGeneric")
	assert.True(t, errors.Is(err, fs.ErrExist),
		"underlying fs.ErrExist must remain reachable for diagnostics")
}

// TestCreateSecretFile_ChmodFailureCleansUpPartialFile drives the
// chmod-failure cleanup arm: when narrowing the freshly-opened file to
// [security.SecretFileMode] fails, the function MUST close and remove
// the partial artifact before returning so no leaky >0o600 file
// survives on disk. The seam swap forces the failure deterministically;
// the post-condition stat asserts the file is gone.
// Not parallel: the swap mutates a process-global seam.
func TestCreateSecretFile_ChmodFailureCleansUpPartialFile(t *testing.T) {
	sentinel := errors.New("chmod denied by seam")
	security.SwapChmodSecretFileForTest(t, func(*os.File, os.FileMode) error {
		return sentinel
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "secret.env")

	f, err := security.CreateSecretFile(path)
	require.Error(t, err, "chmod failure must surface as an error")
	assert.Nil(t, f, "no file handle should be returned on chmod failure")
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"chmod failure surfaces as ErrCodeGeneric")
	assert.True(t, errors.Is(err, sentinel),
		"underlying chmod error must remain reachable for diagnostics")

	_, statErr := os.Stat(path)
	assert.True(t, errors.Is(statErr, fs.ErrNotExist),
		"partial file must be removed so no leaky >0o600 artifact survives")
}

// TestCreateSecretFile_LeavesCallerResponsibleForWriteAndClose
// pins the PACKAGE INVARIANT: `CreateSecretFile` opens but does not
// write, fsync, or close. The caller ('s `internal/state`
// atomic write helper) owns every subsequent step. The test
// verifies all three properties by inspection:
//   - the returned `*os.File` is open and writable (`f.Write`
//     succeeds before any caller-driven close)
//   - the on-disk file is empty immediately after
//     `CreateSecretFile` returns (helper did not write bytes)
//   - the caller's subsequent `Close` flushes the caller-written
//     bytes to the named path with the enforced mode preserved
func TestCreateSecretFile_LeavesCallerResponsibleForWriteAndClose(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "secret.env")

	f, err := security.CreateSecretFile(path)
	require.NoError(t, err)

	// Helper wrote zero bytes — confirmed via fd-level stat
	// before any caller-driven write.
	preInfo, err := f.Stat()
	require.NoError(t, err)
	assert.Equal(t, int64(0), preInfo.Size(),
		"CreateSecretFile must not write bytes; caller owns writes")

	// File is open and writable by the caller (helper did not
	// close).
	payload := []byte("SECRET_VAR=value")
	n, err := f.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, len(payload), n)

	// Caller's Close commits the write. CreateSecretFile did not
	// pre-close the fd.
	require.NoError(t, f.Close())

	diskInfo, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), diskInfo.Size(),
		"caller's bytes are on disk after caller-driven Close")
	assert.Equal(t, security.SecretFileMode, diskInfo.Mode().Perm(),
		"mode survives the caller-driven write+close cycle")
}

// TestCreateSecretFile_DefendsAgainstExoticUmask exercises the
// post-open `f.Chmod(SecretFileMode)` defense. Process umask
// `0o400` strips the owner-read bit during `open(2)` — without the
// post-open Chmod, the on-disk mode would be
// `0o600 & ^0o400 = 0o200` (write-only), making the file
// unreadable even by its own owner.
// MUST NOT call `t.Parallel` — `syscall.Umask` is process-global
// and racing it against other tests would yield non-deterministic
// results. The `t.Cleanup` restores the prior umask; Go's test
// scheduler serializes non-parallel tests before parallel ones, so
// the polluted-umask window is bounded to this test's body.
func TestCreateSecretFile_DefendsAgainstExoticUmask(t *testing.T) {
	// Capture t.TempDir BEFORE setting umask — otherwise the
	// kernel applies `0o700 & ~0o400 = 0o300` to the tempdir's
	// own mode bits and the test cleanup's RemoveAll cannot
	// traverse a directory missing owner-read.
	dir := t.TempDir()

	prev := syscall.Umask(0o400)
	t.Cleanup(func() { _ = syscall.Umask(prev) })

	path := filepath.Join(dir, "secret.env")

	f, err := security.CreateSecretFile(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, security.SecretFileMode, info.Mode().Perm(),
		"post-open Chmod must defeat exotic umask (0o400 would otherwise strip owner-read)")
}

// TestCreateSecretFile_PermissionDeniedSurfacesAsTypedError covers
// the EACCES branch: a parent directory at mode `0o500`
// (r-x------) denies the owner write access on it, so `open(2)`
// with `O_CREATE` fails. The helper surfaces this as
// [types.ErrCodePermissionDenied] with [fs.ErrPermission]
// preserved in the cause chain.
// Skipped when running as root because root bypasses POSIX DAC —
// the kernel would happily create the file regardless of the
// `0o500` parent and the assertion would fail. Per the personal
// "never run sudo internally" rule, wdm tests MUST run as a
// non-root user, but the skip guards against accidental CI
// misconfiguration.
func TestCreateSecretFile_PermissionDeniedSurfacesAsTypedError(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("EACCES test is meaningless when running as root (DAC bypass)")
	}

	dir := t.TempDir()
	sub := filepath.Join(dir, "readonly")
	require.NoError(t, os.Mkdir(sub, 0o500))
	// Restore writable perms BEFORE t.TempDir's RemoveAll
	// cleanup runs — LIFO order means this t.Cleanup fires
	// first, then t.TempDir's, so the directory tree is
	// removable.
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })

	path := filepath.Join(sub, "secret.env")
	f, err := security.CreateSecretFile(path)
	require.Error(t, err)
	assert.Nil(t, f)
	assert.True(t, types.IsCode(err, types.ErrCodePermissionDenied),
		"EACCES must surface as ErrCodePermissionDenied")
	assert.True(t, errors.Is(err, fs.ErrPermission),
		"underlying fs.ErrPermission must remain reachable")
}

// TestRejectInsecureParent_AcceptsSafeParents asserts that a
// parent directory without group/world write bits passes the
// check. `0o700` is the strictest practical mode (owner rwx only)
// and `0o755` is the most common world-readable-but-not-writable
// mode used for user-visible directories like `~/docker`.
func TestRejectInsecureParent_AcceptsSafeParents(t *testing.T) {
	t.Parallel()

	cases := []os.FileMode{0o700, 0o750, 0o755}
	for _, mode := range cases {
		t.Run(fmt.Sprintf("accept_%#o", mode), func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			parent := filepath.Join(base, "p")
			require.NoError(t, os.Mkdir(parent, mode))
			// Explicit chmod defeats process umask narrowing
			// during Mkdir — guarantees the test sees the
			// declared mode regardless of test-runner umask.
			require.NoError(t, os.Chmod(parent, mode))
			t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

			path := filepath.Join(parent, "secret.env")
			assert.NoError(t, security.RejectInsecureParent(path),
				"RejectInsecureParent must accept parent mode %#o", mode)
		})
	}
}

// TestRejectInsecureParent_RejectsGroupAndWorldWritable asserts
// the rejection branch: any parent with group-write (`0o020`) or
// world-write (`0o002`) bits fails the check with
// [types.ErrCodePermissionDenied].
// `0o770` exercises group-write alone; `0o775` exercises
// group-write + world-execute; `0o777` exercises both write bits.
// Each case is structurally distinct in the `mode & 0o022` AND
// result so a regression that only catches one bit would still be
// surfaced.
func TestRejectInsecureParent_RejectsGroupAndWorldWritable(t *testing.T) {
	t.Parallel()

	cases := []os.FileMode{0o770, 0o775, 0o777}
	for _, mode := range cases {
		t.Run(fmt.Sprintf("reject_%#o", mode), func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			parent := filepath.Join(base, "p")
			require.NoError(t, os.Mkdir(parent, mode))
			require.NoError(t, os.Chmod(parent, mode))
			t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

			path := filepath.Join(parent, "secret.env")
			err := security.RejectInsecureParent(path)
			require.Error(t, err,
				"RejectInsecureParent must reject parent mode %#o", mode)
			assert.True(t, types.IsCode(err, types.ErrCodePermissionDenied),
				"insecure-parent rejection must surface as ErrCodePermissionDenied")
		})
	}
}

// TestRejectInsecureParent_RejectsNonexistentParent covers the
// stat-failure branch: a path whose parent does not exist surfaces
// as [types.ErrCodeGeneric] (not a permission issue — the parent
// simply isn't there).
func TestRejectInsecureParent_RejectsNonexistentParent(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	path := filepath.Join(base, "does-not-exist", "secret.env")

	err := security.RejectInsecureParent(path)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"missing parent surfaces as ErrCodeGeneric")
	assert.True(t, errors.Is(err, fs.ErrNotExist),
		"underlying fs.ErrNotExist must remain reachable")
}

// TestRejectInsecureParent_RejectsNonDirectoryParent covers the
// "parent is a regular file" branch. Surfaces as
// [types.ErrCodeGeneric] with a hint pointing at the malformed
// path shape — the caller is supposed to pass `<dir>/<file>`.
func TestRejectInsecureParent_RejectsNonDirectoryParent(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	regular := filepath.Join(base, "not-a-dir")
	require.NoError(t, os.WriteFile(regular, []byte("placeholder"), 0o600))

	path := filepath.Join(regular, "secret.env")
	err := security.RejectInsecureParent(path)
	require.Error(t, err)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"non-directory parent surfaces as ErrCodeGeneric")
}

// TestNoWriteSecretFileSymbol is the negative-surface contract test
// codifying the PACKAGE INVARIANT pinned in `CreateSecretFile`'s
// godoc — no top-level `WriteSecretFile` function may exist in
// `internal/security`. AST-based (not grep-based) because the
// `CreateSecretFile` godoc EXPLICITLY names "WriteSecretFile" as
// the forbidden symbol, and a substring grep would false-match
// that documentation.
// Scans every non-`_test.go` source file under the package
// directory (cwd at test time). Test files are excluded so a
// future test could legitimately reference the name (e.g. another
// negative-surface assertion against a different package).
func TestNoWriteSecretFileSymbol(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err, "parsing %s", name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Recv != nil {
				continue // methods on other types are out of scope
			}
			if fn.Name.Name == "WriteSecretFile" {
				t.Errorf(
					"forbidden top-level symbol WriteSecretFile declared in %s — "+
						"this package deliberately does NOT expose a one-shot write helper; "+
						"every secret-bearing write must compose through internal/state's "+
						"atomic tmp+fsync+rename",
					name,
				)
			}
		}
	}
}
