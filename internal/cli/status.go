package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newAppsStatusCmd builds the `apps status <app-id>` leaf (PRD §18, §32;
// injected factory and renders the read-only [types.AppStatus] in one of two
// forms based on the root's --json persistent flag:
//   - Plain mode: a scannable status block on stdout — the overall state, any
//     PRD §18 attention reasons, per-service lines (name, state, health,
//     published ports), and the snapshot time.
//   - JSON mode: the AppStatus wrapped in the wdm.v1 envelope on stdout and
//     nothing else (PRD §32). AppStatus
//     marshals to a JSON object, so it is the envelope.data object directly.
//
// Exit code: a needs_attention stack still exits 0. PRD §18 treats
// needs-attention as a display state, not a failure, and the engine returns it
// as a successful read (non-nil *AppStatus, nil error). Typed errors that
// drive non-zero PRD §27 codes are reserved for failures that prevent
// reporting at all: a busy stack (ErrCodeRuntimeLockHeld → 4), an unmanaged or
// uninstalled app (ErrCodeUsageValidation → 2), an unreachable daemon
// (ErrCodeDockerUnavailable → 5). This leaf returns whatever the engine
// returns, so cmd/wdm's exitCodeFor maps the engine's success-vs-failure line.
// The §27 contract is that the exit code describes the command's outcome, not
// the subject's health — a script polling status reads needs_attention from
// the payload without treating the poll as a tool failure.
// Status is read-only (PRD §26): no runtime lock, no progress, no
// [types.Confirmer]. The engine factory is invoked inside RunE, and only
// there, so `wdm apps status --help` never reaches [engine.New] (PRD §14
// self-update smoke-check invariant, mirrored from `apps list`).
func newAppsStatusCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <app-id>",
		Short: "Show the runtime status of a managed stack",
		Long: `Status reports the runtime state of a managed app: whether it
is running or needs attention, the reasons behind a needs-attention
state, and per-service container details.

Status is read-only. A stack that needs attention still exits 0 —
the exit code reports whether status could be read, not whether the
app is healthy. Read the state field (or attention_reasons under
--json) to branch on health.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps status: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			status, err := eng.Status(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, status, writeStatus)
		},
	}

	return cmd
}

// writeStatus renders the plain-mode PRD §18 status block to w (stdout). The
// layout is line-oriented and free of table-art so cut(1) and awk(1) stay
// usable: a header line with the app and overall state, an optional
// attention-reasons list, an optional per-service block, and the snapshot time
// when the engine recorded one.
func writeStatus(w io.Writer, status *types.AppStatus) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\t%s\n", status.AppID, status.State)
	if status.Message != "" {
		fmt.Fprintf(&b, "  %s\n", status.Message)
	}

	if len(status.AttentionReasons) > 0 {
		b.WriteString("\nNeeds attention:\n")
		for _, reason := range status.AttentionReasons {
			fmt.Fprintf(&b, "  - %s\n", reason)
		}
	}

	if len(status.Services) > 0 {
		b.WriteString("\nServices:\n")
		for _, svc := range status.Services {
			writeServiceStatus(&b, svc)
		}
	}

	if status.UpdatedAt != nil {
		fmt.Fprintf(&b, "\nChecked at %s\n", status.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("apps status: writing status: %w", err)
	}
	return nil
}

// writeServiceStatus appends one service's runtime line to b: the service
// name, its container state, an optional health word, and any published host
// ports. A needs-attention service is flagged inline so a human sees which
// service is the problem without cross-referencing the reason list.
func writeServiceStatus(b *strings.Builder, svc types.ServiceStatus) {
	fmt.Fprintf(b, "  %s\t%s", svc.Service, svc.State)
	if svc.Health != "" {
		fmt.Fprintf(b, " (%s)", svc.Health)
	}
	if svc.NeedsAttention {
		b.WriteString(" !")
	}
	b.WriteString("\n")

	if svc.Message != "" {
		fmt.Fprintf(b, "      %s\n", svc.Message)
	}
	for _, p := range svc.PublishedPorts {
		fmt.Fprintf(b, "      %s:%d -> %d/%s\n", p.HostIP, p.HostPort, p.ContainerPort, p.Protocol)
	}
}
