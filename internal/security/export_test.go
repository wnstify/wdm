package security

import (
	"io"
	"os"
	"testing"
)

// SwapEntropyForTest replaces the package-private [entropy] reader with
// r for the lifetime of the calling test and restores the prior value
// through [testing.T.Cleanup]. This is the only legitimate path that
// mutates [entropy]; production code never touches it.
// The helper lives in this file because Go forbids production code
// from importing [testing] — the `_test.go` suffix on the filename
// scopes both the [testing] import and this symbol to test
// compilations only. The function is exported (capital S) so external
// tests (`package security_test`) can call it without having to live
// inside the security package themselves; the unexported [entropy]
// variable it closes over remains inaccessible to those external
// tests except through this seam.
// Concurrency: callers MUST NOT use `t.Parallel` in any test that
// invokes this helper. The seam is a process-global package variable,
// so a parallel test that swaps the seam can race with another
// parallel test that reads from it (or with [GenerateSecret] running
// inside a different parallel test that drew from the previously
// installed reader). Tests that only assert on the production
// `crypto/rand.Reader`-backed path (no swap) can still call
// `t.Parallel`.
func SwapEntropyForTest(t *testing.T, r io.Reader) {
	t.Helper()
	prev := entropy
	entropy = r
	t.Cleanup(func() { entropy = prev })
}

// SwapChmodSecretFileForTest replaces the package-private
// [chmodSecretFile] seam with fn for the lifetime of the calling test
// and restores the prior value through [testing.T.Cleanup]. It lets
// external tests drive [CreateSecretFile]'s chmod-failure cleanup arm
// without touching production behavior, which keeps the real method.
// Concurrency: callers MUST NOT use `t.Parallel` while the seam is
// swapped — it is a process-global package variable.
func SwapChmodSecretFileForTest(t *testing.T, fn func(*os.File, os.FileMode) error) {
	t.Helper()
	prev := chmodSecretFile
	chmodSecretFile = fn
	t.Cleanup(func() { chmodSecretFile = prev })
}
