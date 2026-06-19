package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newResourcesCmd builds the top-level `resources <app-id>` command
// (issue #28). It sits at the root, not under `apps`, mirroring the
// product surface for per-app resource management.
// With NO limit flags it prints the read-only current-values view: each
// overridable service's resource limits currently in effect plus the
// catalog's allowed bands (min / recommended / max), by calling the
// read-only [engine.Engine.ResourceSettings].
// With one or more of --memory / --cpus / --pids it calls
// [engine.Engine.Reconfigure] to change the targeted service's limits:
// the engine edits only the resource vars in the stack's .env in place,
// validates the unchanged compose, and recreates the container.
// --service selects the target; when omitted it resolves to the app's
// primary (first catalog-overridable) service.
// Output follows the root's --json persistent flag in both modes:
//   - Plain mode: a line-oriented view / finish screen on stdout, with
//     engine progress streamed to stderr.
//   - JSON mode: the result wrapped in the wdm.v1 envelope on stdout, and
//     nothing else (PRD §32). Progress is suppressed.
func newResourcesCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		service   string
		memory    string
		cpus      string
		pids      int
		assumeYes bool
		stackPath string
	)

	cmd := &cobra.Command{
		Use:   "resources <app-id>",
		Short: "View or change a managed app's resource limits",
		Long: `Resources shows or changes a managed app's per-service resource
limits (memory, CPUs, PIDs).

With no limit flags it prints the limits currently in effect for each
adjustable service alongside the catalog's allowed bands.

With one or more of --memory, --cpus, or --pids it changes the selected
service's limits: wdm edits only the resource values in the stack's .env
in place, validates the Compose file, and recreates the container (a
brief downtime). Secrets, derived values, and every other line in your
.env are preserved byte-for-byte.

Use --service to target a specific service; when omitted it defaults to
the app's primary (first adjustable) service. --yes accepts the recreate
confirmation without prompting.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("resources: reading --json: %w", err)
			}

			changeRequested := cmd.Flags().Changed("memory") ||
				cmd.Flags().Changed("cpus") ||
				cmd.Flags().Changed("pids")

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			appID := args[0]

			if !changeRequested {
				settings, err := eng.ResourceSettings(cmd.Context(), appID)
				if err != nil {
					return err
				}
				if useJSON {
					return EmitJSON(cmd.OutOrStdout(), settings)
				}
				return writeResourceSettings(cmd.OutOrStdout(), settings)
			}

			// With --service omitted, target the app's primary (first
			// catalog-overridable) service, matching how the TUI screen
			// auto-selects. The literal "app" is wrong for apps whose
			// primary service is named otherwise (for example baserow's
			// "baserow"), so resolve it from the read-only settings.
			targetService := service
			if !cmd.Flags().Changed("service") {
				resolved, err := resolvePrimaryService(cmd.Context(), eng, appID)
				if err != nil {
					return err
				}
				targetService = resolved
			}

			req := types.ReconfigureRequest{
				AppID:     appID,
				Service:   targetService,
				StackPath: stackPath,
			}
			if cmd.Flags().Changed("memory") {
				req.Memory = &memory
			}
			if cmd.Flags().Changed("cpus") {
				req.CPUs = &cpus
			}
			if cmd.Flags().Changed("pids") {
				req.PIDs = &pids
			}

			// Reconfigure recreates a container (disruptive), so the
			// confirmation is not a database-risk prompt; acceptDBRisk is wired
			// false, and --yes accepts the recreate confirmation.
			confirmer := newCLIConfirmer(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), assumeYes, false)

			var onProgress types.ProgressFn
			if !useJSON {
				onProgress = stderrProgress(cmd.ErrOrStderr())
			}

			result, err := eng.Reconfigure(cmd.Context(), req, onProgress, confirmer)
			if err != nil {
				return err
			}

			if useJSON {
				return EmitJSON(cmd.OutOrStdout(), result)
			}
			return writeReconfigureFinish(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().StringVar(&service, "service", "", "service whose resource limits change (defaults to the app's primary service)")
	cmd.Flags().StringVar(&memory, "memory", "", "new memory limit (for example 1g)")
	cmd.Flags().StringVar(&cpus, "cpus", "", "new CPU quota (for example 1.5)")
	cmd.Flags().IntVar(&pids, "pids", 0, "new pid limit")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept the recreate confirmation without prompting")
	cmd.Flags().StringVar(&stackPath, "stack-path", "", "assert the managed stack path being reconfigured (verified against the app)")

	return cmd
}

// resolvePrimaryService returns the service the reconfigure targets when
// --service is omitted: the first catalog-overridable service the app
// declares, matching the TUI screen's auto-selection. It reads the
// read-only [engine.Engine.ResourceSettings] (no runtime lock, no Docker
// call). An app that declares no adjustable service is refused with a
// usage error rather than defaulting to a literal name that would fail
// the band lookup downstream.
func resolvePrimaryService(ctx context.Context, eng engine.Engine, appID string) (string, error) {
	settings, err := eng.ResourceSettings(ctx, appID)
	if err != nil {
		return "", err
	}
	if settings != nil {
		for _, svc := range settings.Services {
			if svc.Adjustable {
				return svc.Service, nil
			}
		}
	}
	return "", types.NewError(
		types.ErrCodeUsageValidation,
		"this app declares no adjustable resource limits",
		"this app has no service whose resource limits can be changed",
	)
}

// writeResourceSettings renders the read-only current-values view to w
// (stdout): one block per service with the limits currently in effect
// and the catalog's allowed bands. The layout is line-oriented so cut(1)
// and awk(1) stay usable, mirroring the other finish screens.
func writeResourceSettings(w io.Writer, settings *types.ResourceSettings) error {
	var b strings.Builder

	fmt.Fprintf(&b, "Resource limits for %s:\n", settings.AppID)
	if len(settings.Services) == 0 {
		b.WriteString("  (this app declares no adjustable resource limits)\n")
	}
	for _, svc := range settings.Services {
		fmt.Fprintf(&b, "\nService: %s", svc.Service)
		if !svc.Adjustable {
			b.WriteString(" (not adjustable)")
		}
		b.WriteByte('\n')
		fmt.Fprintf(&b, "  memory: current=%s allowed min=%s recommended=%s max=%s\n",
			emptyDash(svc.CurrentMemory), emptyDash(svc.MemoryMin), emptyDash(svc.MemoryRecommended), emptyDash(svc.MemoryMax))
		fmt.Fprintf(&b, "  cpus:   current=%s allowed min=%s recommended=%s max=%s\n",
			emptyDash(svc.CurrentCPUs), emptyDash(svc.CPUsMin), emptyDash(svc.CPUsRecommended), emptyDash(svc.CPUsMax))
		fmt.Fprintf(&b, "  pids:   current=%d allowed default=%d max=%d\n",
			svc.CurrentPIDs, svc.PIDsDefault, svc.PIDsMax)
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("resources: writing output: %w", err)
	}
	return nil
}

// writeReconfigureFinish renders the reconfigure-finish screen to w
// (stdout): the service changed, the applied limits, and the
// post-recreate status state. The "running" headline is gated on the
// post-reconfigure status, mirroring writeRestartFinish.
func writeReconfigureFinish(w io.Writer, result *types.ReconfigureResult) error {
	var b strings.Builder

	if result.Status != nil && result.Status.NeedsAttention {
		fmt.Fprintf(&b, "%s service %s was reconfigured; see the status below for services that need attention.\n",
			result.AppID, result.Service)
	} else {
		fmt.Fprintf(&b, "%s service %s was reconfigured and is running.\n", result.AppID, result.Service)
	}

	b.WriteString("\nApplied limits:\n")
	fmt.Fprintf(&b, "  memory: %s\n", emptyDash(result.Memory))
	fmt.Fprintf(&b, "  cpus:   %s\n", emptyDash(result.CPUs))
	fmt.Fprintf(&b, "  pids:   %d\n", result.PIDs)

	if result.BackupPath != "" {
		fmt.Fprintf(&b, "\nConfig backup: %s\n", result.BackupPath)
	}

	if result.Status != nil {
		fmt.Fprintf(&b, "\nStatus: %s\n", result.Status.State)
		if result.Status.Message != "" {
			fmt.Fprintf(&b, "  %s\n", result.Status.Message)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("resources: writing finish screen: %w", err)
	}
	return nil
}

// emptyDash renders an empty string as "-" so the line-oriented view
// keeps aligned columns when a value is absent.
func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
