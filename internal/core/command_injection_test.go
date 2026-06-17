package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

// coreInjectionPayloads is the command-injection payload set adapted for
// the core-side string inputs that flow toward Docker argv (app IDs,
// domains, stack paths). internal/core validates each before render or
// any exec, so these must surface as typed usage-validation refusals —
// the argv-only boundary in internal/docker is the second line of
// defense (PRD §12, §31).
var coreInjectionPayloads = []struct {
	name    string
	payload string
}{
	{name: "semicolon separator", payload: "app; ls"},
	{name: "double ampersand", payload: "app && whoami"},
	{name: "pipe to passwd", payload: "app | cat /etc/passwd"},
	{name: "backtick substitution", payload: "app`whoami`"},
	{name: "dollar paren substitution", payload: "app$(id)"},
	{name: "newline splice", payload: "app\nrm -rf /"},
	{name: "embedded space", payload: "app rm"},
	{name: "redirection", payload: "app > /tmp/owned"},
	{name: "glob", payload: "app*"},
	{name: "subshell parens", payload: "$(reboot)"},
}

// TestCommandInjection_HostileAppIDRefusedBeforeDocker proves that a
// hostile app id carrying shell metacharacters is refused with a typed
// ErrCodeUsageValidation BEFORE any Docker command runs: catalog
// selection only matches the curated app's clean id (and path-separator
// shapes are caught by the stack-path SafeJoin), so an injection
// payload can never select a catalog entry, render a stack, or reach an
// exec. The fake docker client's zero call count is the structural
// proof that the refusal precedes client construction.
func TestCommandInjection_HostileAppIDRefusedBeforeDocker(t *testing.T) {
	t.Parallel()

	for _, tc := range coreInjectionPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Catalog carries a single clean app; the hostile id below
			// can never equal it.
			eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, appFixture("clean-app", 18080))))
			core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
				return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
			})
			fake := &fakeDockerClient{}
			core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

			res, err := eng.Install(
				t.Context(),
				types.InstallRequest{AppID: tc.payload},
				nil,
				&fakeConfirmer{},
			)
			require.Error(t, err)
			assert.Nil(t, res)
			assertUsageValidation(t, err)
			assert.NotErrorIs(t, err, types.ErrNotImplemented)
			assert.Zero(t, fake.calls,
				"a hostile app id must be refused before any docker command")
		})
	}
}

// TestCommandInjection_HostileDomainRefused drives the OWASP payload set
// through the catalog domain placeholder. normalizeDomain enforces a
// strict RFC-1123 ASCII character class, so every
// metacharacter payload — separators, substitutions, redirection,
// whitespace — is refused as a typed ErrCodeUsageValidation during
// planning, before render or any exec.
func TestCommandInjection_HostileDomainRefused(t *testing.T) {
	t.Parallel()

	app := appFixture("domain-injection-app", freeLocalTCPPort(t))
	app.Placeholders = []catalog.Placeholder{{
		Name:     "DOMAIN",
		Type:     "domain",
		Required: true,
	}}
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	host := system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}

	for _, tc := range coreInjectionPayloads {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := core.PlanInstallForTest(
				eng,
				t.Context(),
				types.InstallRequest{AppID: app.AppID, Domain: tc.payload},
				host,
				nil,
			)
			require.Error(t, err)

			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code,
				"hostile domain %q must be refused as usage validation", tc.payload)
		})
	}
}

// TestCommandInjection_HostileStackPathRefused drives injection-shaped
// --stack-path values through planning. A stack path must be an
// absolute, traversal-free path under a safe root; payloads carrying
// parent traversal, command separators, or substitution shapes are
// refused as typed ErrCodeUsageValidation before render or any exec
// (PRD §12, §13).
func TestCommandInjection_HostileStackPathRefused(t *testing.T) {
	t.Parallel()

	app := appFixture("stackpath-injection-app", freeLocalTCPPort(t))
	host := system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}

	// Each payload is genuinely refused by the stack-path validator:
	// traversal segments are rejected outright, relative shapes fail the
	// absolute-path requirement, and system-directory prefixes are
	// denied. A metacharacter-bearing path that is nonetheless absolute
	// AND outside every reserved root is intentionally NOT listed here —
	// such a path is accepted as a literal directory name and is
	// structurally safe because internal/docker passes it as a single
	// argv element (proven in internal/docker's command-injection test).
	cases := []struct {
		name      string
		stackPath string
	}{
		{name: "traversal with command", stackPath: "../stack; rm -rf /"},
		{name: "home traversal with command", stackPath: "~/../stack && whoami"},
		{name: "relative with substitution", stackPath: "stack/$(whoami)"},
		{name: "relative with pipe", stackPath: "stack | cat /etc/passwd"},
		{name: "relative with backtick", stackPath: "stack/`id`"},
		{name: "system dir with chain", stackPath: "/etc/wdm; ls"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
			_, err := core.PlanInstallForTest(
				eng,
				t.Context(),
				types.InstallRequest{AppID: app.AppID, StackPath: tc.stackPath},
				host,
				nil,
			)
			require.Error(t, err)
			assertUsageValidation(t, err)
		})
	}
}
