package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
)

// This file walks the command tree from NewRootCmd and pins that every
// command path exists and resolves to the right command, with a non-nil RunE
// on each leaf. It reuses findCommandPath from the registration tests.
// It deliberately does NOT assert leaf behavior. The per-leaf behavior (the
// wdm.v1 envelope, exit codes) is asserted by each leaf's own _test.go file.
// Group commands keep their own runnable help RunE (the apps.go group
// pattern), so this contract also pins a non-nil RunE on the new
// `self-update` group itself — registration shape only, not behavior.

// registers every command path: `catalog check` and
// `catalog update` under the existing `catalog` group, and the new top-level
// `self-update` group with its `check` and `apply` leaves. There is no registry command — the registry image check
// folds into the existing `apps update` — so none is pinned.
func TestCatalogUpdateAndSelfUpdateRegistration_AllCommandPathsExist(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		path   []string
		isLeaf bool // a leaf must have a Runnable RunE; a group hosts children
	}{
		// New leaves under the existing top-level `catalog` group.
		{name: "catalog check", path: []string{"catalog", "check"}, isLeaf: true},
		{name: "catalog update", path: []string{"catalog", "update"}, isLeaf: true},

		// New top-level `self-update` group.
		{name: "self-update", path: []string{"self-update"}, isLeaf: false},
		{name: "self-update check", path: []string{"self-update", "check"}, isLeaf: true},
		{name: "self-update apply", path: []string{"self-update", "apply"}, isLeaf: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build a fresh tree per subtest: cobra lazily sorts a
			// command's child slice on the first Commands call (mutating
			// shared state), so sharing one root across parallel subtests
			// races on that sort (golang-spf13-cobra: "build a fresh
			// command tree per test").
			root := NewRootCmd("test", func() (engine.Engine, error) {
				return &fakeEngine{}, nil
			})

			cmd := findCommandPath(root, tc.path...)
			require.NotNilf(t, cmd, "command path %q is not registered", tc.name)

			if tc.isLeaf {
				assert.NotNilf(t, cmd.RunE, "leaf %q must have a non-nil RunE", tc.name)
			}
		})
	}
}
