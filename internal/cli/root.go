package cli

import (
	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
)

// NewRootCmd constructs the wdm root [cobra.Command] with the version string
// and engine factory.
// newEngine is a lazy constructor for [engine.Engine]. cmd/wdm builds the
// closure — the only package allowed to import internal/core per the depguard
// "cli-uses-engine" rule. Subcommand leaves invoke the factory on their
// execution path, never here at construction time, so `wdm --version` and
// `wdm --help` short-circuit before any engine code runs. PRD §14's
// self-update smoke check depends on this: a malformed config.toml must not
// break the version path.
// version flows from cmd/wdm so the build-time ldflag value (PRD §14) has a
// single path. Cobra's default version template emits "<command> version
// <version>"; the override below produces just the bare version string plus a
// newline so the smoke check can compare it directly against the downloaded
// release.
// SilenceUsage and SilenceErrors keep cmd/wdm in control of stderr output and
// PRD §27 exit-code mapping. Without SilenceErrors, Cobra prints "Error:..."
// to stderr before returning, racing with main's own "wdm: <err>" line;
// without SilenceUsage, every error prints the full help banner (golang-cli:
// save full usage for --help).
// Args is cobra.NoArgs and RunE prints help, which makes root the
// non-TTY/no-subcommand fallback: `wdm` with no subcommand prints help, and
// `wdm foo` (unknown subcommand) exits 2 with "unknown command". Leaf dispatch
// such as `wdm apps list` bypasses root's RunE.
func NewRootCmd(version string, newEngine func() (engine.Engine, error)) *cobra.Command {
	root := &cobra.Command{
		Use:   "wdm",
		Short: "Webnestify Docker Manager — curated Docker Compose self-hosting templates",
		Long: `wdm installs, updates, and checks a curated set of Docker
Compose self-hosting templates with strong security defaults.
The tool refuses root and sudo execution by design; run it as
a normal user who belongs to the docker group.`,
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	root.SetVersionTemplate("{{.Version}}\n")

	// Shell completion is deferred to release packaging; keep the
	root.CompletionOptions.DisableDefaultCmd = true

	root.PersistentFlags().Bool("json", false,
		"emit machine-readable JSON via the wdm.v1 envelope (PRD §32)")

	// --debug raises the file log to debug level (PRD §24). It is consumed
	// by cmd/wdm before Cobra parses, since the engine is constructed there;
	// registering it here keeps Cobra from rejecting the flag and surfaces it
	// in --help.
	root.PersistentFlags().Bool("debug", false,
		"write verbose debug logs to the wdm log file (still redacts secrets, PRD §24)")

	root.AddCommand(newAppsCmd(newEngine))

	// registration toucher): `catalog` (, the the confirmation rules
	// catalog-browse form), `settings`, and `lock`.
	root.AddCommand(newCatalogCmd(newEngine))
	root.AddCommand(newSettingsCmd(newEngine))
	root.AddCommand(newLockCmd(newEngine))

	// registration toucher; the `catalog check` / `catalog update` leaves
	// register inside newCatalogCmd). Leaves: `self-update check` (read-only)
	// and `self-update apply` (binary self-update, PRD §14). Names settled by
	root.AddCommand(newSelfUpdateCmd(newEngine))

	// Top-level destructive system command (PRD §39, issue #29): `uninstall`
	// tears down every managed app and removes wdm's own footprint. It sits
	// at the root, not under `apps`, because it acts on wdm itself.
	root.AddCommand(newUninstallCmd(newEngine))

	// Top-level per-app resource management (issue #28): `resources` views
	// or changes a managed app's resource limits. It sits at the root,
	// alongside the other per-app verbs, and acts on one installed stack.
	root.AddCommand(newResourcesCmd(newEngine))

	return root
}
