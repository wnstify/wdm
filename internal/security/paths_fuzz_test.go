package security_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnstify/wdm/internal/security"
)

// fuzzSafeJoinSeeds is the starting corpus for the path-safety join. It
// pairs benign relative subpaths with the escape-shaped inputs SafeJoin
// must refuse: parent traversal chains, absolute paths, NUL bytes, long
// components, unicode normalization tricks, and backslashes.
var fuzzSafeJoinSeeds = []struct {
	root string
	sub  string
}{
	{root: "/home/user/docker", sub: "app"},
	{root: "/home/user/docker", sub: "nested/app/config"},
	{root: "/home/user/docker", sub: "."},
	{root: "/home/user/docker", sub: ".."},
	{root: "/home/user/docker", sub: "../etc/passwd"},
	{root: "/home/user/docker", sub: "a/../../b"},
	{root: "/home/user/docker", sub: "/etc/passwd"},
	{root: "/home/user/docker", sub: ""},
	{root: "/home/user/docker", sub: "app\x00name"},
	{root: "/home/user/docker", sub: strings.Repeat("a", 300)},
	{root: "/home/user/docker", sub: "café"},
	{root: "/home/user/docker", sub: "app\\..\\..\\etc"},
	{root: "/home/user/docker", sub: "./app/."},
	{root: "", sub: "app"},
	{root: "relative/root", sub: "app"},
}

// FuzzSafeJoin drives the real path-containment join
// [security.SafeJoin] against arbitrary (root, subpath) pairs. It is the
// security-critical surface of 's "path safety" fuzz fence —
// the guard behind PRD §12's "no writes outside the selected stack
// directory" invariant — so the body asserts the containment property
// independently of SafeJoin's own internal check:
//   - Never panics.
//   - On rejection, the returned path is empty and the error wraps
//     [security.ErrUnsafePath] or [security.ErrPathEscape].
//   - On acceptance, the joined path is absolute and lexically inside
//     the cleaned root, re-verified here with filepath.Rel — the
//     relative path from root to the result must stay local (never begin
//     with ".." or be rooted), so a join that escaped its sandbox is
//     caught even if SafeJoin's own EnsureWithinRoot guard regressed.
func FuzzSafeJoin(f *testing.F) {
	for _, seed := range fuzzSafeJoinSeeds {
		f.Add(seed.root, seed.sub)
	}

	f.Fuzz(func(t *testing.T, root, sub string) {
		joined, err := security.SafeJoin(root, sub)

		if err != nil {
			if joined != "" {
				t.Fatalf("SafeJoin(%q, %q) returned both a path %q and an error", root, sub, joined)
			}
			if !errors.Is(err, security.ErrUnsafePath) && !errors.Is(err, security.ErrPathEscape) {
				t.Fatalf("SafeJoin(%q, %q) rejection wraps neither sentinel: %v", root, sub, err)
			}
			return
		}

		if !filepath.IsAbs(joined) {
			t.Fatalf("SafeJoin(%q, %q) accepted non-absolute path %q", root, sub, joined)
		}

		// Independent containment proof: the lexical relative path from
		// the cleaned root to the result must be local — no ".."
		// climb-out, no absolute re-injection.
		cleanedRoot := filepath.Clean(root)
		rel, relErr := filepath.Rel(cleanedRoot, joined)
		if relErr != nil {
			t.Fatalf("SafeJoin(%q, %q) accepted %q but Rel failed: %v", root, sub, joined, relErr)
		}
		if !filepath.IsLocal(rel) {
			t.Fatalf("SafeJoin(%q, %q) accepted %q which escapes root %q (rel=%q)", root, sub, joined, cleanedRoot, rel)
		}

		// EnsureWithinRoot is the public containment check SafeJoin
		// composes; an accepted join must satisfy it too.
		if err := security.EnsureWithinRoot(root, joined); err != nil {
			t.Fatalf("SafeJoin(%q, %q) accepted %q but EnsureWithinRoot rejects it: %v", root, sub, joined, err)
		}
	})
}
