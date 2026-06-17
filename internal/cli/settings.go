package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// settableSettingKeys maps each settable config.toml key (the snake_case TOML
// tag on [types.Settings]) to a setter that writes value onto a loaded settings
// struct. This is the leaf's ONLY own validation surface (the invariant's
// thin-leaf rule): the CLI maps a key name to a field, and the engine owns
// every value check — the update-check-preference enum, the locked "stable"
// catalog channel, base-stack-path safety, the Docker network-name schema, and
// the timezone. schema_version is deliberately ABSENT: it
// is managed by wdm's schema migrations (internal/state), not the user, so
// `settings set schema_version...` refuses below with a usage error.
// The keys are the on-disk vocabulary a user already sees in config.toml, so
// `settings set base_stack_path ~/srv` edits exactly the line they would edit
// by hand. The map value is a setter rather than a reflect-based field lookup
// so the settable surface stays explicit and a non-settable field
// (schema_version) cannot be reached by accident.
var settableSettingKeys = map[string]func(s *types.Settings, value string){
	"base_stack_path":         func(s *types.Settings, v string) { s.BaseStackPath = v },
	"timezone":                func(s *types.Settings, v string) { s.Timezone = v },
	"default_docker_network":  func(s *types.Settings, v string) { s.DefaultDockerNetwork = v },
	"catalog_channel":         func(s *types.Settings, v string) { s.CatalogChannel = v },
	"update_check_preference": func(s *types.Settings, v string) { s.UpdateCheckPreference = v },
}

// newSettingsCmd builds the top-level `settings` group and registers its
// `set` leaf. The factory flows down to the leaf so it can be wired inside
// RunE following the install/status precedent.
func newSettingsCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	settings := &cobra.Command{
		Use:           "settings",
		Short:         "View and change wdm user settings",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	settings.AddCommand(newSettingsSetCmd(newEngine))
	return settings
}

// newSettingsSetCmd builds the live `settings set <key> <value>` leaf (PRD
// §34). It is a THIN caller of the engine's settings surface: it loads the
// current settings via [engine.Engine.Settings], replaces exactly the named
// field, persists the merged struct via [engine.Engine.UpdateSettings], and
// renders that same merged struct.
// The rendered struct is byte-faithful to what was persisted:
// [engine.Engine.UpdateSettings] marshals the caller's struct verbatim (it
// expands "~/" only as a validation step, never rewriting the stored value), so
// the merged struct the leaf built IS the on-disk truth. The engine's
// in-memory cache is deliberately NOT re-read: [engine.Engine.Settings] returns
// a copy loaded once at [engine.New], which UpdateSettings does not refresh, so
// a post-write re-read would render the PRE-edit value. (Refreshing that cache
// is a recorded for the TUI settings screen, which holds a
// long-lived engine.)
// The leaf owns no business logic. Its only own check is the key-name mapping
// (see [settableSettingKeys]): an unknown key, or schema_version (managed by
// wdm's schema migrations, not the user), refuses with a usage error. The
// engine owns every value validation — the update-check-preference enum, the
// locked "stable" catalog channel, base-stack-path safety, the Docker
// network-name schema, and the timezone — and surfaces a typed
// [types.ErrCodeUsageValidation] error for an invalid value, which cmd/wdm maps
// to exit 2.
// The key-name refusal happens BEFORE the engine factory runs (mirroring the
// install --set precedent): a malformed invocation never constructs the engine,
// acquires runtime.lock, or reads config.toml.
// Output discipline (PRD §32):
//   - Plain mode: a short confirmation on stdout naming the key and the merged
//     value that was persisted.
//   - JSON mode (the root's --json persistent flag): ONLY the merged
//     [types.Settings] wrapped in the wdm.v1 envelope on stdout. Settings
//     marshals to a JSON object, so it is the envelope.data object directly.
//
// The error path leaves stdout empty: a refused key, a failed engine
// validation, or a failed save all return before any stdout write.
// No [types.Confirmer] and no [types.ProgressFn]: UpdateSettings is a
// config-only write that reconciles no deployed apps (PRD §34). The engine
// factory is invoked inside RunE, and only there, so `wdm settings set --help`
// never reaches [engine.New] (PRD §14 self-update smoke-check invariant,
// mirrored from `apps list`).
// Exit codes (mapped from the returned error by cmd/wdm, PRD §27):
//   - 0 success (the merged struct validated and persisted).
//   - 2 usage validation: a CLI key refusal (unknown or schema_version) or an
//     engine value rejection (bad enum, non-"stable" channel, unsafe
//     base path, malformed network name, bad timezone).
//   - 4 the global runtime.lock is held by another wdm process.
//   - 6 permission denied: the secret-mode config write refuses a group- or
//     world-writable config parent directory.
//   - 1 a generic fault writing config.toml (e.g. a marshal or atomic write
//     failure).
//
// Exit codes 5 (Docker unavailable) and 7 (user canceled) are NOT reachable:
// the path makes no Docker contact and takes no Confirmer.
func newSettingsSetCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a wdm user setting",
		Long: `Set persists one user setting to ~/.config/wdm/config.toml.

The settable keys are the snake_case config.toml keys:

  base_stack_path          directory under which managed stacks are written
  timezone                 IANA timezone name (empty = detect at install)
  default_docker_network   external Docker network attached to managed stacks
  catalog_channel          catalog channel ("stable" in v1)
  update_check_preference  one of: manual, daily-on-launch, disabled

schema_version is managed by wdm and cannot be set. The value is validated
by wdm before it is saved; an invalid value is rejected and config.toml is
left unchanged.`,
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]

			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("settings set: reading --json: %w", err)
			}

			// Resolve the key to a field setter before constructing the
			// engine: an unknown or non-settable key is a usage error, and
			// refusing it here keeps the engine and runtime.lock untouched on a
			// bad invocation, mirroring `apps install --set`.
			setField, err := resolveSettingKey(key)
			if err != nil {
				return err
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			// Read-modify-write through the facade: load the current (or
			// default) settings, replace exactly the named field, and persist the
			// merged struct so every untouched field round-trips unchanged.
			settings, err := eng.Settings(cmd.Context())
			if err != nil {
				return err
			}
			setField(settings, value)

			if err := eng.UpdateSettings(cmd.Context(), *settings); err != nil {
				return err
			}

			// Render the merged struct just persisted. UpdateSettings marshals
			// it verbatim, so it is byte-faithful to config.toml; the engine's
			// in-memory cache is deliberately not re-read, since UpdateSettings
			// does not refresh it.
			if useJSON {
				return EmitJSON(cmd.OutOrStdout(), settings)
			}
			return writeSettingsSetConfirmation(cmd.OutOrStdout(), key, value)
		},
	}

	return cmd
}

// resolveSettingKey looks up the setter for a settable config key. An unknown
// key, or schema_version (managed by wdm's schema migrations, not the user),
// returns a usage error naming the settable keys so the user can correct the
// invocation. The error is plain (non-typed), so cmd/wdm's exitCodeFor default
// arm maps it to exit 2 (usage).
func resolveSettingKey(key string) (func(s *types.Settings, value string), error) {
	if setField, ok := settableSettingKeys[key]; ok {
		return setField, nil
	}

	if key == "schema_version" {
		return nil, fmt.Errorf(
			"settings set: schema_version is managed by wdm and cannot be set (settable keys: %s)",
			settableKeyList(),
		)
	}

	return nil, fmt.Errorf(
		"settings set: unknown setting %q (settable keys: %s)",
		key, settableKeyList(),
	)
}

// settableKeyList returns the settable config keys as a sorted,
// comma-separated string for refusal guidance. Sorting keeps the list
// deterministic (map iteration order is randomized) so the help text and
// error messages are stable across runs.
func settableKeyList() string {
	keys := make([]string, 0, len(settableSettingKeys))
	for k := range settableSettingKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// writeSettingsSetConfirmation renders the plain-mode confirmation for a
// successful save to w (stdout): the key and the persisted value. The value is
// the input verbatim — [engine.Engine.UpdateSettings] marshals the merged
// struct without rewriting any field — so echoing it is byte-faithful to
// config.toml.
func writeSettingsSetConfirmation(w io.Writer, key, value string) error {
	if _, err := fmt.Fprintf(w, "%s = %s\n", key, value); err != nil {
		return fmt.Errorf("settings set: writing confirmation: %w", err)
	}
	return nil
}
