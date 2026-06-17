package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// appsListPayload is the inner shape of the wdm.v1 envelope emitted by
// `wdm apps list --json` (PRD §32). The slice lives under the stable "apps"
// key, not at the top of envelope.data, because PRD §32 mandates an object,
// and the keyed shape leaves room for sibling fields (warnings, scan_errors,
// next_cursor) without breaking parsers.
// nil slices serialize to JSON "null". The leaf RunE normalizes a nil
// [engine.Engine.List] result to an empty slice so the wire contract emits
// "apps": [] for the no-stacks case.
type appsListPayload struct {
	Apps []types.AppInfo `json:"apps"`
}

// newAppsCmd builds the `apps` command group (PRD §17–§20). Its leaves are
// `list`, `install`, the read-only `status` / `logs`, `update`, the safe
// `remove`, `restart`, `validate`, the `backups` subgroup (`list` /
// `restore`), and the destructive `delete`.
// Like the root command, Args:NoArgs plus a RunE that prints help is
// load-bearing: without Args:NoArgs, `wdm apps foo` silently exits 0 because
// no subcommand matched; without a Runnable RunE, --help skips the Usage and
// Flags sections. The golang-spf13-cobra skill names this the canonical
// pattern for a group command with no behavior of its own.
// The newEngine factory flows to each leaf RunE rather than a shared
// PersistentPreRunE: a Pre hook could build the engine even on `wdm apps
// --help`, and a Pre/Post pair splits Close handling where PostRunE does not
// run on RunE failure. Inlining open plus defer Close in the leaf keeps the
// lifecycle uniform across success and error paths.
func newAppsCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	apps := &cobra.Command{
		Use:           "apps",
		Short:         "Manage curated Docker Compose application stacks",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	apps.AddCommand(newAppsListCmd(newEngine))
	apps.AddCommand(newAppsInstallCmd(newEngine))
	apps.AddCommand(newAppsStatusCmd(newEngine))
	apps.AddCommand(newAppsLogsCmd(newEngine))
	apps.AddCommand(newAppsUpdateCmd(newEngine))
	apps.AddCommand(newAppsRemoveCmd(newEngine))
	apps.AddCommand(newAppsRestartCmd(newEngine))
	apps.AddCommand(newAppsValidateCmd(newEngine))
	apps.AddCommand(newAppsBackupsCmd(newEngine))
	apps.AddCommand(newAppsDeleteCmd(newEngine))
	return apps
}

// newAppsListCmd builds the `apps list` leaf (PRD §9). It calls
// [engine.Engine.List] and emits one of two forms based on the root's --json
// persistent flag:
//   - Plain mode: one stack per line as "<app_id>\t<stack_path>", tab-
//     separated so cut(1) and awk(1) parse without quoting. An empty result
//     output on a fresh system").
//   - JSON mode: the wdm.v1 envelope wraps an object whose "apps" key holds
//     the slice (PRD §32). A nil result is normalized to an empty slice.
//
// The engine factory is invoked here, and only here, so `wdm --version`,
// `wdm --help`, `wdm apps --help`, and `wdm apps list --help` never reach
// [core.New]. PRD §14's self-update smoke check requires --version to exit 0
// even when the installed config.toml is malformed.
// [engine.Engine.Close] runs via defer regardless of outcome. The errcheck
// silencer is intentional: Close's failure mode is an already-released flock,
// not actionable from a CLI handler.
func newAppsListCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	return &cobra.Command{
		Use:           "list",
		Short:         "List managed Docker Compose stacks",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close is a no-op flip today and will release resources in a later phase

			apps, err := eng.List(cmd.Context())
			if err != nil {
				return err
			}

			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps list: reading --json: %w", err)
			}
			if useJSON {
				if apps == nil {
					apps = []types.AppInfo{}
				}
				return EmitJSON(cmd.OutOrStdout(), appsListPayload{Apps: apps})
			}

			for _, a := range apps {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", a.AppID, a.StackPath); err != nil {
					return fmt.Errorf("apps list: writing output: %w", err)
				}
			}
			return nil
		},
	}
}
