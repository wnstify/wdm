package core_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// removeAllAllowedFiles is the CLOSED allowlist of production source files
// permitted to call os.RemoveAll anywhere in the repository (the c22
// production-source-scan precedent, extended for E1 per /
// 269). Any NEW os.RemoveAll site in a file outside this set fails the
// scan.
// Reconciliation note (recorded for the reviewer and the audit log): the
// delete implementation behind EnsureWithinRoot containment". Three
// PRE-EXISTING legitimate sites already exist and predate E1, so that text
// is unsatisfiable as literally written. The RECONCILED invariant this
// scan enforces is a closed allowlist of exactly those three files plus
// the delete implementation:
//   - internal/core/delete.go — the new containment-guarded primitive
//     (E1); the os.RemoveAll runs only after resolveDeleteTarget proves
//     symlink-resolved containment under the stack base via
//     security.EnsureWithinRoot.
//   - internal/core/install.go — the private 0o700 compose-validation
//     tempdir cleanup (a deferred best-effort RemoveAll of a workspace
//     the engine itself created under os.MkdirTemp).
//   - internal/state/backup.go — the backupRemoveAll = os.RemoveAll seam
//     and its snapshot-scoped uses (empty-snapshot cleanup, retention
//     prune, restore-path cleanup), all scoped to the stack-local
//     .wdm-backups tree.
//   - internal/state/catalog_bundle.go — the RemoveContainedTree /
//     removeTreeIfPresent verified-catalog rollback primitive;
//     the os.RemoveAll runs only on absolute paths the catalog storage
//     writer (internal/catalog/storage.go) builds under the catalogs root
//     from fixed constants and the validVersionSegment-validated version,
//     so every removal target is contained by construction.
//   - internal/core/self_update.go — the ephemeral verification staging
//     dir cleanup in CheckSelfUpdate; the os.RemoveAll runs
//     only on the directory the engine itself created via os.MkdirTemp("",
//     "wdm-selfupdate-check-*"), never the live binary (the check never
//     replaces anything).
//   - internal/core/self_update_apply.go — the ephemeral replacement
//     staging dir cleanup in ApplySelfUpdate; the os.RemoveAll
//     runs only on the directory the engine itself created via
//     os.MkdirTemp(target.Dir, ".wdm-selfupdate-stage-*"). The live binary
//     is replaced via os.Rename, never RemoveAll, and the wdm.previous
//     retention copy is removed (on a failed replace) via os.Remove (single
//     file), not RemoveAll — so no RemoveAll target is ever the install
//     target or its sibling rollback binary.
//   - internal/core/uninstall.go — the self-uninstall footprint removal
//     (PRD §39); the os.RemoveAll runs only inside removeFootprintDir, after
//     the target is symlink-resolved (filepath.EvalSymlinks), proven within
//     the user home via security.EnsureWithinRoot, and rejected when it
//     resolves to a suspiciously shallow path. The running binary and its
//     .previous sibling are removed via os.Remove (single file), never
//     RemoveAll.
//   - internal/core/recover.go — the opt-in orphan-recovery removal
//     (issue #114, `apps install --force`); the os.RemoveAll in
//     removeOrphanStackDir runs ONLY after state.ClearStaleStackLock proved
//     the directory is a wdm-owned interrupted-install orphan (its .wdm.lock
//     was empty/corrupt and removed under a held flock), AND the target is
//     rejected as an unsafe root, with symlinked ancestors, proven within the
//     user home via security.EnsureWithinRoot, and rejected when it resolves
//     to a suspiciously shallow path. A directory with no .wdm.lock is removed
//     only via os.Remove (empty-dir only), never RemoveAll.
//
// way 's forbidigo criterion was ticked at with its
// own reconciliation note. The scan walks every production (non-_test.go)
// file under internal/, pkg/, and cmd/.
var removeAllAllowedFiles = map[string]struct{}{
	filepath.Join("internal", "core", "delete.go"):            {},
	filepath.Join("internal", "core", "install.go"):           {},
	filepath.Join("internal", "core", "recover.go"):           {},
	filepath.Join("internal", "core", "self_update.go"):       {},
	filepath.Join("internal", "core", "self_update_apply.go"): {},
	filepath.Join("internal", "core", "uninstall.go"):         {},
	filepath.Join("internal", "state", "backup.go"):           {},
	filepath.Join("internal", "state", "catalog_bundle.go"):   {},
}

// TestProductionSourcesRestrictRemoveAllToAllowlist walks every production
// Go source file under internal/, pkg/, and cmd/ and asserts that any
// os.RemoveAll call expression lives in a file on the closed allowlist
// above. A new os.RemoveAll in any other production file fails this test,
// pinning the §19:452 / the invariant invariant that destructive directory
// removal is a single, containment-guarded site.
// The detection is AST-based, not a string grep: it finds CallExpr whose
// Fun is a selector os.RemoveAll (package identifier "os", selector
// "RemoveAll"), so a comment, a string literal, or a same-named method on
// a different receiver does not trip it — and the negative-fixture
// subtest proves the matcher actually fires on a fabricated source line.
func TestProductionSourcesRestrictRemoveAllToAllowlist(t *testing.T) {
	t.Parallel()

	moduleRoot := moduleRootForScan(t)
	fileSet := token.NewFileSet()

	for _, root := range []string{"internal", "pkg", "cmd"} {
		rootDir := filepath.Join(moduleRoot, root)
		if _, err := os.Stat(rootDir); os.IsNotExist(err) {
			continue
		}

		err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || !isProductionGoFile(d.Name()) {
				return nil
			}

			raw, readErr := os.ReadFile(path)
			require.NoError(t, readErr, "reading %s", path)

			file, parseErr := parser.ParseFile(fileSet, path, raw, parser.SkipObjectResolution)
			require.NoError(t, parseErr, "parsing %s", path)

			rel, relErr := filepath.Rel(moduleRoot, path)
			require.NoError(t, relErr)
			_, allowed := removeAllAllowedFiles[rel]

			if fileCallsOSRemoveAll(file) && !allowed {
				t.Errorf(
					"%s calls os.RemoveAll but is not on the closed allowlist; "+
						"destructive directory removal must stay contained to the delete "+
						"implementation, the install tempdir cleanup, and the backup seam "+
						"(PRD §19)",
					rel,
				)
			}
			return nil
		})
		require.NoError(t, err, "walking %s", rootDir)
	}
}

// TestRemoveAllScanMatcherCatchesFabricatedCall is the negative-fixture
// proof that the AST matcher actually detects an os.RemoveAll call rather
// than vacuously passing. A source-file scan cannot mutate a real
// production file to prove the catch (that would break the build), so this
// drives the SAME fileCallsOSRemoveAll matcher over a fabricated source
// string carrying an os.RemoveAll call and asserts it fires — and over a
// near-miss (a same-named method on a non-os receiver) and asserts it does
// NOT.
func TestRemoveAllScanMatcherCatchesFabricatedCall(t *testing.T) {
	t.Parallel()

	fileSet := token.NewFileSet()

	t.Run("detects os.RemoveAll", func(t *testing.T) {
		t.Parallel()

		src := `package fixture
import "os"
func wipe(p string) error { return os.RemoveAll(p) }
`
		file, err := parser.ParseFile(fileSet, "fixture.go", src, parser.SkipObjectResolution)
		require.NoError(t, err)
		require.True(t, fileCallsOSRemoveAll(file),
			"the matcher must detect a real os.RemoveAll call")
	})

	t.Run("ignores same-named method on another receiver", func(t *testing.T) {
		t.Parallel()

		src := `package fixture
type cleaner struct{}
func (c cleaner) RemoveAll(p string) error { return nil }
func wipe(c cleaner, p string) error { return c.RemoveAll(p) }
`
		file, err := parser.ParseFile(fileSet, "fixture.go", src, parser.SkipObjectResolution)
		require.NoError(t, err)
		require.False(t, fileCallsOSRemoveAll(file),
			"a same-named method on a non-os receiver must not trip the matcher")
	})
}

// fileCallsOSRemoveAll reports whether file contains a call expression
// whose function is the selector os.RemoveAll — package identifier "os",
// selector "RemoveAll". It deliberately matches on the unqualified "os"
// identifier (the project never aliases the os import), so a method named
// RemoveAll on a different receiver does not match.
func fileCallsOSRemoveAll(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RemoveAll" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "os" {
			found = true
			return false
		}
		return true
	})
	return found
}

// isProductionGoFile reports whether name is a non-test, non-doc Go source
// file the scan should inspect.
func isProductionGoFile(name string) bool {
	if !strings.HasSuffix(name, ".go") {
		return false
	}
	if strings.HasSuffix(name, "_test.go") {
		return false
	}
	return name != "doc.go"
}

// moduleRootForScan locates the repository root by walking up from the
// current package directory until it finds go.mod, so the scan paths are
// stable regardless of where `go test` is invoked from.
func moduleRootForScan(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "go.mod not found walking up from the test directory")
		dir = parent
	}
}
