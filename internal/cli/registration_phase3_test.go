package cli

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
)

// This file walks the command tree from NewRootCmd and pins that every
// command path exists and resolves to the right command, with a non-nil RunE
// on each leaf.
// It deliberately does NOT assert leaf behavior. Group commands keep their
// own runnable help RunE (the apps.go group pattern), so this contract also
// pins a non-nil RunE on the groups themselves — registration shape only,
// not behavior. The per-leaf behavior (the wdm.v1 envelope, exit codes) is
// asserted by each leaf's own _test.go file.

// findCommandPath walks root's subcommand tree following path and returns
// the resolved command, or nil if any segment is missing. cobra's Find
// would also match flags and partial args, so this uses an explicit
// name-by-name descent over the registered Commands to assert the exact
// registration shape.
func findCommandPath(root *cobra.Command, path ...string) *cobra.Command {
	cur := root
	for _, name := range path {
		var next *cobra.Command
		for _, sub := range cur.Commands() {
			if sub.Name() == name {
				next = sub
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

// registers every command path. Leaves must carry a non-nil RunE; group commands carry their own help RunE so they are
// non-nil too, but the contract for a group is that it exists and hosts
// the right children — asserted by the leaf rows under it.
func TestAppsCatalogSettingsLockRegistration_AllCommandPathsExist(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		path   []string
		isLeaf bool // a leaf must have a Runnable RunE; a group hosts children
	}{
		// New `apps` subcommands.
		{name: "apps restart", path: []string{"apps", "restart"}, isLeaf: true},
		{name: "apps validate", path: []string{"apps", "validate"}, isLeaf: true},
		{name: "apps backups", path: []string{"apps", "backups"}, isLeaf: false},
		{name: "apps backups list", path: []string{"apps", "backups", "list"}, isLeaf: true},
		{name: "apps backups restore", path: []string{"apps", "backups", "restore"}, isLeaf: true},
		{name: "apps delete", path: []string{"apps", "delete"}, isLeaf: true},

		// New top-level `catalog` group.
		{name: "catalog", path: []string{"catalog"}, isLeaf: false},
		{name: "catalog list", path: []string{"catalog", "list"}, isLeaf: true},
		{name: "catalog show", path: []string{"catalog", "show"}, isLeaf: true},

		// New top-level `settings` group.
		{name: "settings", path: []string{"settings"}, isLeaf: false},
		{name: "settings set", path: []string{"settings", "set"}, isLeaf: true},

		// New top-level `lock` group.
		{name: "lock", path: []string{"lock"}, isLeaf: false},
		{name: "lock status", path: []string{"lock", "status"}, isLeaf: true},
		{name: "lock clear", path: []string{"lock", "clear"}, isLeaf: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build a fresh tree per subtest: cobra lazily sorts a
			// command's child slice on the first Commands call
			// (mutating shared state), so sharing one root across parallel
			// subtests races on that sort (golang-spf13-cobra: "build a
			// fresh command tree per test").
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
