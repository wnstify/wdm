package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// This file holds the top-level `catalog` group and its leaf bodies —
// `catalog list`, `catalog show <app-id>`, `catalog check`, and
// `catalog update` (PRD §7 "Install an app", §8 first-run wizard, §15
// eligibility; §22 verified catalog network self-update). The browse leaves
// are thin callers of the engine's read-only browse surface
// ([engine.Engine.AvailableApps] / [engine.Engine.AvailableApp]); the
// check/update leaves call its catalog-update surface
// ([engine.Engine.CheckCatalogUpdate] / [engine.Engine.ApplyCatalogUpdate]).
// Group form: a top-level
// `catalog` group with `wdm catalog list` / `wdm catalog show <app-id>` was
// chosen over `wdm apps available`. The vocabulary mirrors PRD §32's
// `wdm catalog status` example, gives the self-update commands a
// natural home, and avoids conflating installable catalog entries with the
// managed-only `apps` group under one verb. Registration tests pin the
// command paths (`catalog list`, `catalog show`, `catalog check`,
// `catalog update`), so the constructors keep their registered names.
// `catalog` is a group command, mirroring the `apps` group (apps.go):
// Args:NoArgs plus a Runnable RunE that prints help, the golang-spf13-cobra
// canonical group pattern. Without Args:NoArgs an unknown subcommand would
// silently exit 0; without a Runnable RunE, `catalog --help` would skip the
// Usage and Flags sections.
// The browse leaves are read-only: the engine reads the
// verified catalog straight off the local FS with no network call, no Docker
// contact, no runtime.lock, no per-stack flock, and no [types.Confirmer] or
// [types.ProgressFn]. `catalog check` is also read-only and mirrors that
// posture, surfacing the current/latest version, whether an update is
// available, the change summary, and the verification state. `catalog update`
// is state-changing and mirrors `apps update`: the engine holds the global
// runtime.lock, emits progress, and gates the verified download/write on the
// shared [cliConfirmer] under the catalog_update confirmation kind (a SAFE
// kind: --yes accepts, a TTY prompts y/N, a non-TTY without --yes refuses →
// [types.ErrCodeUserCanceled], exit 7). Neither leaf modifies any deployed app
// or per-stack .wdm.lock. Output follows PRD §32 — plain text on
// stdout by default, only the wdm.v1 envelope under --json — and the engine
// factory is invoked inside RunE so `--help` never reaches [engine.New] (PRD
// §14 self-update smoke-check invariant).

// catalogListPayload is the inner shape of the wdm.v1 envelope emitted by
// `wdm catalog list --json` (PRD §32). The slice lives under the stable "apps"
// key, not at the top of envelope.data, because PRD §32 mandates an object, and
// the keyed shape leaves room for sibling fields (a channel echo, scan
// warnings) without breaking parsers. This mirrors [appsListPayload] so both
// list views share one envelope shape.
// A nil [engine.Engine.AvailableApps] result is normalized to an empty slice
// so the wire contract emits "apps": [] for the empty-catalog case.
type catalogListPayload struct {
	Apps []types.CatalogApp `json:"apps"`
}

// newCatalogCmd builds the top-level `catalog` group and registers its
// `list` and `show` leaves. The factory flows down to each leaf so it is
// wired inside RunE following the install/status precedent.
func newCatalogCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	catalog := &cobra.Command{
		Use:           "catalog",
		Short:         "Browse the installable catalog of curated apps",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	catalog.AddCommand(newCatalogListCmd(newEngine))
	catalog.AddCommand(newCatalogShowCmd(newEngine))
	catalog.AddCommand(newCatalogCheckCmd(newEngine))
	catalog.AddCommand(newCatalogUpdateCmd(newEngine))
	return catalog
}

// newCatalogCheckCmd builds the `catalog check` leaf (PRD §22 — the read-only
// "is a newer verified catalog available?" probe). It calls
// [engine.Engine.CheckCatalogUpdate] through the injected factory and renders
// [types.CatalogUpdateStatus] in one of two forms based on the root's --json
// persistent flag:
//   - Plain mode: a scannable, table-art-free status block on stdout —
//     channel, current and latest version ("(none)" for an empty current
//     version, i.e. no local catalog installed yet), whether an update is
//     available (yes/no), whether the latest is verified (yes/no), and the
//     per-app change list when an update is available (one
//     "<app_id>\t<kind>\t<summary>" per line). Line-oriented so cut(1) and
//     awk(1) stay usable.
//   - JSON mode: the CatalogUpdateStatus wrapped DIRECTLY in the wdm.v1
//     envelope on stdout and nothing else (PRD §32). It marshals to a JSON
//     object, so it is the envelope.data object directly.
//
// The --channel flag selects the catalog channel; an empty value (the default)
// selects the configured default channel ("stable" in v1), as
// [types.CatalogUpdateQuery] documents.
// Read-only path: no runtime.lock, no [types.Confirmer], no
// [types.ProgressFn] — a pure read like Status. The check still reaches the
// network and verifies the latest bundle fail-closed, so transport and trust
// faults stay distinct exit codes. The engine factory is invoked inside RunE,
// and only there, so `wdm catalog check --help` never reaches [engine.New]
// (PRD §14 self-update smoke-check invariant).
// Exit codes (mapped from the returned error by cmd/wdm, PRD §27; the invariant
// keeps network and verification failures distinct):
//   - 0 success (including "no update available" and "no local catalog").
//   - 2 usage validation: an invalid or non-"stable" catalog channel (refused
//     before any network call).
//   - 3 verification failure: the latest bundle failed checksum, signature, or
//     attestation, or a present local catalog is corrupt (fail closed — never
//     an unverified "update available").
//   - 8 network failure: the catalog release metadata or asset download could
//     not be reached.
//   - 1 generic: a local read fault (EACCES / I/O) on an installed catalog —
//     neither a network nor a trust failure.
//
// Exit codes 4 (runtime.lock held), 5 (Docker unavailable), 6 (permission
// denied), and 7 (user canceled) are NOT reachable: the read-only check takes
// no lock, makes no Docker call, and passes no Confirmer.
func newCatalogCheckCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var channel string

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check whether a newer verified catalog is available",
		Long: `Check reports whether a newer verified catalog is available from
the configured catalog endpoint: the current local version, the latest
verified version, whether an update is available, the per-app change
summary, and whether the latest passed checksum, signature, and
attestation verification.

Check is read-only — it downloads and verifies the latest catalog to
report on it but never writes anything. Use 'wdm catalog update' to apply
an available update.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("catalog check: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			status, err := eng.CheckCatalogUpdate(cmd.Context(), types.CatalogUpdateQuery{Channel: channel})
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, status, writeCatalogUpdateStatus)
		},
	}

	cmd.Flags().StringVar(&channel, "channel", "",
		`catalog channel to check (empty = configured default, "stable" in v1)`)

	return cmd
}

// newCatalogUpdateCmd builds the `catalog update` leaf (PRD §22 — the
// state-changing "download, verify, and install the newer catalog" apply
// path). It calls [engine.Engine.ApplyCatalogUpdate] through the injected
// factory and renders [types.CatalogUpdateResult] in one of two forms based on
// the root's --json persistent flag:
//   - Plain mode: a finish block on stdout — channel, the version transition
//     (previous → applied, "(none)" when no local catalog existed), the
//     verification detail when present, and the per-app change list when
//     present. The engine's progress lines stream to stderr (download / verify
//     / write).
//   - JSON mode: the CatalogUpdateResult wrapped DIRECTLY in the wdm.v1
//     envelope on stdout, and nothing else (PRD §32). Progress is suppressed.
//
// The apply downloads and verifies the catalog fail-closed (checksum,
// signature, attestation) BEFORE any write, refuses a signed rollback, confirms
// via the catalog_update [types.Confirmer] kind, then writes the verified
// catalog atomically under the local catalogs directory. It never modifies any
// deployed app or per-stack .wdm.lock.
// Flags map onto [types.CatalogUpdateRequest] plus one authorization gate:
//   - --channel: the catalog channel to update; empty (the default) selects
//     the configured default channel ("stable" in v1).
//   - --target-version: pin the exact catalog version to apply (the value
//     `wdm catalog check` surfaces). The engine refuses with
//     [types.ErrCodeUsageValidation] when the verified latest differs, so a
//     check→apply TOCTOU never silently installs an unexpected version. Empty
//     (the default) accepts whatever the verified latest is.
//   - --yes: accept the SAFE catalog_update confirmation without prompting. On
//     a TTY without --yes the leaf prompts y/N; on a non-TTY without --yes it
//     refuses (the shared [cliConfirmer] fail-closed posture) and the engine
//     maps that to [types.ErrCodeUserCanceled] (exit 7). There is no
//     database-risk gate — a catalog write is non-destructive config, so the
//     confirmer is wired acceptDBRisk=false.
//
// The engine factory is invoked inside RunE, and only there, so
// `wdm catalog update --help` never reaches [engine.New] (PRD §14 self-update
// smoke-check invariant).
// Exit codes (mapped from the returned error by cmd/wdm, PRD §27; the invariant
// keeps network and verification failures distinct):
//   - 0 success (the verified catalog was written).
//   - 2 usage validation: an invalid or non-"stable" channel, a
//     --target-version the verified latest does not match, a refused signed
//     rollback (the latest is not newer than the installed catalog), or a nil
//     confirmer.
//   - 3 verification failure: the latest bundle failed checksum, signature, or
//     attestation, or a present local catalog is corrupt.
//   - 4 runtime.lock held: another wdm operation is in progress.
//   - 7 user canceled: the catalog_update confirmation was declined, or
//     auto-refused on a non-TTY without --yes.
//   - 8 network failure: the catalog release metadata or asset download could
//     not be reached.
//   - 1 generic: a local read fault on an installed catalog, or a verified
//     catalog write/store fault.
//
// Exit codes 5 (Docker unavailable) and 6 (permission denied) are NOT
// reachable: the apply makes no Docker call and the leaf does not re-check the
// root/sudo posture (cmd/wdm did that before any subcommand).
func newCatalogUpdateCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		channel       string
		targetVersion string
		assumeYes     bool
	)

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Download, verify, and install a newer catalog",
		Long: `Update downloads the latest catalog, verifies it (checksum,
signature, attestation) before writing anything, confirms the action, and
then installs the verified catalog under the local catalogs directory.

It refuses a downgrade (a catalog older than or equal to the installed
one) and never changes any deployed app. Use --yes to accept the
confirmation without prompting, or --target-version to require a specific
catalog version (the value 'wdm catalog check' reports).`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("catalog update: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			req := types.CatalogUpdateRequest{
				Channel:       channel,
				TargetVersion: targetVersion,
			}

			// A catalog write is non-destructive config (no app is touched), so
			// the catalog_update confirmation is SAFE; acceptDBRisk is false
			// because this flow never produces a database-risk kind.
			confirmer, onProgress := stateChangeIO(cmd, assumeYes, false, useJSON)

			result, err := eng.ApplyCatalogUpdate(cmd.Context(), req, onProgress, confirmer)
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, result, writeCatalogUpdateResult)
		},
	}

	cmd.Flags().StringVar(&channel, "channel", "",
		`catalog channel to update (empty = configured default, "stable" in v1)`)
	cmd.Flags().StringVar(&targetVersion, "target-version", "",
		"require this catalog version (refuses if the verified latest is another)")
	cmd.Flags().BoolVar(&assumeYes, "yes", false,
		"accept the catalog-update confirmation without prompting")

	return cmd
}

// newCatalogListCmd builds the `catalog list` leaf (PRD §7, §8 step 3, §15). It
// calls [engine.Engine.AvailableApps] through the injected factory and emits
// one of two forms based on the root's --json persistent flag:
//   - Plain mode: one installable app per line as
//     "<app_id>\t<name>\t<template_version>\t<summary>" — tab-separated with
//     the free-text summary last. The schema pattern-constrains app_id to be
//     tab- and newline-free; name, template_version, and summary are
//     unconstrained, so tab separation keeps the leading fields parseable by
//     cut(1)/awk(1) for the curated catalog (whose maintainer-authored fields
//     carry no tabs or newlines, though the schema does not foreclose them for
//     arbitrary content). An empty catalog emits no output and exits 0.
//   - JSON mode: the wdm.v1 envelope wraps an object whose "apps" key holds the
//     slice (PRD §32 forbids a top-level array). A nil result is normalized to
//     an empty slice.
//
// The browse list shows installable catalog ENTRIES, not managed stacks (that
// is `apps list`), so it carries no stack path or runtime status — only the
// catalog identity a picker needs.
// The --channel flag selects the catalog channel; an empty value (the default)
// selects the configured default channel ("stable" in v1), as
// [types.CatalogQuery] documents.
// Read-only browse path: no lock, no Docker, no network, no
// [types.Confirmer], no [types.ProgressFn]. The engine factory is invoked
// inside RunE, and only there, so `wdm catalog list --help` never reaches
// [engine.New] (PRD §14 self-update smoke-check invariant).
// Exit codes (mapped from the returned error by cmd/wdm, PRD §27):
//   - 0 success (including an empty catalog).
//   - 2 usage validation: an invalid catalog channel (a non-"stable" or
//     malformed channel).
//   - 3 verification failure: the catalog could not be read or schema-verified
//     off the local FS.
//
// Exit codes 1 (generic), 4 (runtime.lock held), 5 (Docker unavailable), 6
// (permission denied), and 7 (user canceled) are NOT reachable: the browse path
// takes no lock, makes no Docker or network call, writes nothing, takes no
// Confirmer, and emits no generic typed error — its only typed errors are
// usage-validation (2) and verification (3) above. Even a hypothetical
// non-typed error maps to exit 2 via cmd/wdm's exitCodeFor default arm, never
// to 1.
func newCatalogListCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var channel string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the installable apps in the catalog",
		Long: `List shows the curated apps available to install from the catalog,
one per line as app_id, name, template version, and a one-line summary.

These are installable catalog entries, not managed stacks — use
'wdm apps list' to see the stacks you have already installed. An empty
catalog exits 0 with no output.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("catalog list: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			apps, err := eng.AvailableApps(cmd.Context(), types.CatalogQuery{Channel: channel})
			if err != nil {
				return err
			}

			if useJSON {
				if apps == nil {
					apps = []types.CatalogApp{}
				}
				return EmitJSON(cmd.OutOrStdout(), catalogListPayload{Apps: apps})
			}
			return writeCatalogList(cmd.OutOrStdout(), apps)
		},
	}

	cmd.Flags().StringVar(&channel, "channel", "",
		`catalog channel to browse (empty = configured default, "stable" in v1)`)

	return cmd
}

// newCatalogShowCmd builds the `catalog show <app-id>` leaf (PRD §7, §8, §15 —
// the app-detail projection). It calls [engine.Engine.AvailableApp] through the
// injected factory and renders the [types.CatalogApp] detail in one of two
// forms based on the root's --json persistent flag:
//   - Plain mode: a scannable detail block on stdout — an identity header (app
//     id, name, template, channel), the summary and description, the
//     placeholders with type/required/secret markers (a secret placeholder is
//     marked engine-generated and is NEVER prompted; wdm generates it at
//     install time), the ports, the pinned images, the recommended resource
//     bands, and the risk classification. Line-oriented and free of table-art
//     so cut(1) and awk(1) stay usable.
//   - JSON mode: the CatalogApp wrapped DIRECTLY in the wdm.v1 envelope on
//     stdout and nothing else (PRD §32). It marshals to a JSON object, so it is
//     the envelope.data object directly.
//
// The --channel flag selects the catalog channel; an empty value (the default)
// selects the configured default channel ("stable" in v1), as
// [types.CatalogAppQuery] documents.
// Read-only browse path: no lock, no Docker, no network, no
// [types.Confirmer], no [types.ProgressFn]. The engine factory is invoked
// inside RunE, and only there, so `wdm catalog show --help` never reaches
// [engine.New] (PRD §14 self-update smoke-check invariant).
// Exit codes (mapped from the returned error by cmd/wdm, PRD §27):
//   - 0 success.
//   - 2 usage validation: an unknown app id, or an invalid catalog channel.
//   - 3 verification failure: the catalog could not be read or schema-verified
//     off the local FS (a duplicate app id refuses here too).
//
// Exit codes 1 (generic), 4 (runtime.lock held), 5 (Docker unavailable), 6
// (permission denied), and 7 (user canceled) are NOT reachable: the browse path
// takes no lock, makes no Docker or network call, writes nothing, takes no
// Confirmer, and emits no generic typed error — its only typed errors are
// usage-validation (2) and verification (3) above. Even a hypothetical
// non-typed error maps to exit 2 via cmd/wdm's exitCodeFor default arm, never
// to 1.
func newCatalogShowCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var channel string

	cmd := &cobra.Command{
		Use:   "show <app-id>",
		Short: "Show full details for one catalog app",
		Long: `Show prints the full detail of one installable catalog app: its
identity, template and channel, summary and description, the placeholders
the install form collects (secret placeholders are generated by wdm and
never prompted), the ports it binds, the pinned images, the recommended
resource bands, and its risk classification.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("catalog show: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			app, err := eng.AvailableApp(cmd.Context(), types.CatalogAppQuery{
				AppID:   args[0],
				Channel: channel,
			})
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, app, writeCatalogApp)
		},
	}

	cmd.Flags().StringVar(&channel, "channel", "",
		`catalog channel to look the app up in (empty = configured default, "stable" in v1)`)

	return cmd
}

// writeCatalogList renders the plain-mode `catalog list` block to w (stdout):
// one installable app per line as
// "<app_id>\t<name>\t<template_version>\t<summary>", tab-separated and free of
// table-art, with the free-text summary last. The schema pattern-constrains
// app_id to be tab- and newline-free; name, template_version, and summary are
// unconstrained, so tab separation keeps the leading fields parseable by
// cut(1)/awk(1) for the curated catalog (whose maintainer-authored fields
// contain no tabs or newlines, though the schema does not foreclose them for
// arbitrary content). An empty slice writes nothing (exit 0).
func writeCatalogList(w io.Writer, apps []types.CatalogApp) error {
	for _, app := range apps {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			app.AppID, app.Name, app.TemplateVersion, app.Summary); err != nil {
			return fmt.Errorf("catalog list: writing output: %w", err)
		}
	}
	return nil
}

// writeCatalogApp renders the plain-mode `catalog show` detail block to w
// (stdout). The layout is line-oriented and free of table-art so cut(1) and
// awk(1) stay usable: an identity header, optional summary/description, then
// per-section blocks (placeholders, ports, images, resources, risk) emitted
// only when they carry content.
func writeCatalogApp(w io.Writer, app *types.CatalogApp) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\t%s\n", app.AppID, app.Name)
	fmt.Fprintf(&b, "  template\t%s %s\n", app.TemplateName, app.TemplateVersion)
	fmt.Fprintf(&b, "  channel\t%s\n", app.Channel)
	if app.Summary != "" {
		fmt.Fprintf(&b, "  summary\t%s\n", app.Summary)
	}
	if app.Description != "" {
		fmt.Fprintf(&b, "  description\t%s\n", app.Description)
	}

	writeCatalogPlaceholders(&b, app.Placeholders)
	writeCatalogPorts(&b, app.Ports)
	writeCatalogImagePins(&b, app.ImagePins)
	writeCatalogResources(&b, app.Resources)

	if len(app.RiskClassification) > 0 {
		fmt.Fprintf(&b, "\nRisk: %s\n", strings.Join(app.RiskClassification, ", "))
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("catalog show: writing detail: %w", err)
	}
	return nil
}

// writeCatalogPlaceholders appends the placeholders block to b, one placeholder
// per line with its type and inline markers. A secret placeholder is marked
// "generated" so the reader knows wdm produces the value at install time and
// never prompts for it; a required non-secret placeholder is marked "required".
// Skipped when there are no placeholders.
func writeCatalogPlaceholders(b *strings.Builder, placeholders []types.CatalogPlaceholder) {
	if len(placeholders) == 0 {
		return
	}
	b.WriteString("\nPlaceholders:\n")
	for _, ph := range placeholders {
		fmt.Fprintf(b, "  %s\t%s", ph.Key, ph.Type)
		switch {
		case ph.Secret:
			b.WriteString(" (generated)")
		case ph.Required:
			b.WriteString(" (required)")
		}
		if ph.Default != "" {
			fmt.Fprintf(b, " [default: %s]", ph.Default)
		}
		b.WriteString("\n")
		if ph.Description != "" {
			fmt.Fprintf(b, "      %s\n", ph.Description)
		}
	}
}

// writeCatalogPorts appends the ports block to b, one declared port per
// line as "<host> -> <container>/<protocol>" with the exposing service when
// the catalog records one. Skipped when there are no ports.
func writeCatalogPorts(b *strings.Builder, ports []types.CatalogPort) {
	if len(ports) == 0 {
		return
	}
	b.WriteString("\nPorts:\n")
	for _, port := range ports {
		fmt.Fprintf(b, "  %d -> %d", port.Host, port.Container)
		if port.Protocol != "" {
			fmt.Fprintf(b, "/%s", port.Protocol)
		}
		if port.Service != "" {
			fmt.Fprintf(b, " (%s)", port.Service)
		}
		b.WriteString("\n")
	}
}

// writeCatalogImagePins appends the images block to b, one pinned image per
// line as "<service>\t<image>:<tag>" (the tag omitted when the catalog pins
// only an image reference). Skipped when there are no image pins.
func writeCatalogImagePins(b *strings.Builder, pins []types.CatalogImagePin) {
	if len(pins) == 0 {
		return
	}
	b.WriteString("\nImages:\n")
	for _, pin := range pins {
		fmt.Fprintf(b, "  %s\t%s", pin.Service, pin.Image)
		if pin.Tag != "" {
			fmt.Fprintf(b, ":%s", pin.Tag)
		}
		b.WriteString("\n")
	}
}

// writeCatalogResources appends the recommended-resources block to b, one
// service per line with its recommended memory and CPU bands when the catalog
// records them — what the install path selects when the host guidance budget allows.
// Skipped when there are no resource bands.
func writeCatalogResources(b *strings.Builder, resources []types.CatalogResource) {
	if len(resources) == 0 {
		return
	}
	b.WriteString("\nResources (recommended):\n")
	for _, res := range resources {
		fmt.Fprintf(b, "  %s", res.Service)
		if res.MemoryRecommended != "" {
			fmt.Fprintf(b, "\tmem %s", res.MemoryRecommended)
		}
		if res.CPUsRecommended != "" {
			fmt.Fprintf(b, "\tcpus %s", res.CPUsRecommended)
		}
		b.WriteString("\n")
	}
}

// writeCatalogUpdateStatus renders the plain-mode `catalog check` block to w
// (stdout). The block is line-oriented and free of table-art so cut(1) and
// awk(1) stay usable: a "channel\t<name>" header, then current/latest version,
// update-available, and verified lines (booleans rendered yes/no), then the
// per-app change list when an update is available. An empty current version (no
// local catalog installed yet) renders as "(none)" so the block never shows a
// blank field.
func writeCatalogUpdateStatus(w io.Writer, status *types.CatalogUpdateStatus) error {
	var b strings.Builder

	fmt.Fprintf(&b, "channel\t%s\n", status.Channel)
	fmt.Fprintf(&b, "current\t%s\n", catalogVersionOrNone(status.CurrentVersion))
	fmt.Fprintf(&b, "latest\t%s\n", catalogVersionOrNone(status.LatestVersion))
	fmt.Fprintf(&b, "update available\t%s\n", yesNo(status.UpdateAvailable))
	fmt.Fprintf(&b, "verified\t%s\n", yesNo(status.Verified))

	writeCatalogChanges(&b, status.Changes)

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("catalog check: writing status: %w", err)
	}
	return nil
}

// writeCatalogUpdateResult renders the plain-mode `catalog update` finish
// block to w (stdout): a "channel\t<name>" header, the version transition
// (previous → applied, "(none)" when no local catalog existed), the
// verification detail when present, and the per-app change list when the
// apply realized any changes. Line-oriented and table-art-free to stay
// cut(1)/awk(1)-parseable.
func writeCatalogUpdateResult(w io.Writer, result *types.CatalogUpdateResult) error {
	var b strings.Builder

	fmt.Fprintf(&b, "channel\t%s\n", result.Channel)
	fmt.Fprintf(&b, "updated\t%s -> %s\n",
		catalogVersionOrNone(result.PreviousVersion), result.AppliedVersion)
	if result.VerificationDetail != "" {
		fmt.Fprintf(&b, "verification\t%s\n", result.VerificationDetail)
	}

	writeCatalogChanges(&b, result.Changes)

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("catalog update: writing result: %w", err)
	}
	return nil
}

// writeCatalogChanges appends the per-app change list to b, one change per
// line as "<app_id>\t<kind>\t<summary>" under a "Changes:" header. The
// schema constrains app_id to be tab- and newline-free; kind is the closed
// {added, updated, removed} set; summary is maintainer-authored free text.
// The block is skipped entirely when there are no changes.
func writeCatalogChanges(b *strings.Builder, changes []types.CatalogChange) {
	if len(changes) == 0 {
		return
	}
	b.WriteString("\nChanges:\n")
	for _, change := range changes {
		fmt.Fprintf(b, "  %s\t%s\t%s\n", change.AppID, change.Kind, change.Summary)
	}
}

// catalogVersionOrNone returns version, or "(none)" when it is empty, so a
// plain-mode line never shows a blank field for "no local catalog installed
// yet". An empty current/previous version is a legitimate state (the engine
// reports it for a never-installed catalog), not an error.
func catalogVersionOrNone(version string) string {
	if version == "" {
		return "(none)"
	}
	return version
}

// yesNo renders a boolean as the "yes"/"no" tokens the plain-mode catalog
// blocks use, keeping the update-available and verified lines scannable for
// cut(1)/awk(1) rather than emitting Go's "true"/"false" spelling.
func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}
