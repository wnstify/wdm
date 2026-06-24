package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newAppsRedeployCmd builds the `apps redeploy <app-id>` leaf (issue #97). It
// calls [engine.Engine.RedeployStack] through the injected factory to apply a
// managed stack's user-overlay edits: the engine runs `docker compose up -d`,
// which re-reads the on-disk Compose file plus the content-gated
// docker-compose.override.yml and re-evaluates each service's
// env_file: [.env.user], recreating only the containers whose effective config
// changed. Unlike `apps restart` (plain `docker compose restart`, which reuses
// the running containers without re-reading config and so does NOT pick up
// overlay edits), redeploy applies them — without re-rendering anything from
// the catalog. It re-renders no template, generates no secret, and changes no
// image or version; it deploys the on-disk files as they already are.
//
// It renders [types.RestartResult] (reused; the shape fits) in one of two forms
// based on the root's --json persistent flag:
//   - Plain mode: a redeploy-finish screen on stdout — a headline, the services
//     it recreated, and the post-redeploy status state. Progress lines stream
//     to stderr.
//   - JSON mode: the full result wrapped in the wdm.v1 envelope on stdout, and
//     nothing else (PRD §32). Progress is suppressed.
//
// The flag set mirrors `apps restart`. Redeploy is whole-stack only, so there
// is no per-service flag. The flags are --yes (a safe-confirmation bypass),
// --stack-path (the engine's fail-closed cross-check), and the inherited
// --json. Redeploy never produces a database-risk confirmation, so acceptDBRisk
// is wired false; "redeploy_safe" is a safe confirmation that --yes accepts.
//
// Exit codes map from the engine's typed errors exactly as `apps restart` does:
// 0 success (a needs_attention result still exits 0), 2 usage validation,
// 4 runtime-lock held, 5 docker unavailable (a `docker compose up` failure,
// including an invalid override, propagates the typed code unchanged),
// 7 user-canceled, 1 generic.
//
// The engine factory is invoked inside RunE, and only there, so
// `wdm apps redeploy --help` never reaches [engine.New] (PRD §14).
func newAppsRedeployCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		assumeYes bool
		stackPath string
	)

	cmd := &cobra.Command{
		Use:   "redeploy <app-id>",
		Short: "Apply overlay changes by recreating a managed stack",
		Long: `Redeploy recreates a managed app from its on-disk files to apply
your overlay edits: it runs docker compose up -d, which re-reads the
Compose file and your docker-compose.override.yml and re-evaluates each
service's .env.user, recreating only the containers whose effective
config changed.

Use this after editing .env.user or docker-compose.override.yml. Plain
"apps restart" reuses the running containers without re-reading config,
so it does NOT apply overlay edits; redeploy does. Redeploy never
re-renders templates from the catalog and never changes images,
versions, or secrets.

Redeploy affects the whole stack; there is no per-service option in this
version.

--yes accepts the safe redeploy confirmation without prompting. Use
--stack-path to assert which managed stack path is being redeployed; it
is verified against the app's resolved stack and refuses on a
mismatch.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps redeploy: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			req := types.RestartRequest{
				AppID:     args[0],
				StackPath: stackPath,
			}

			confirmer, onProgress := stateChangeIO(cmd, assumeYes, false, useJSON)

			result, err := eng.RedeployStack(cmd.Context(), req, onProgress, confirmer)
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, result, writeRedeployFinish)
		},
	}

	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept the safe redeploy confirmation without prompting")
	cmd.Flags().StringVar(&stackPath, "stack-path", "", "assert the managed stack path being redeployed (verified against the app)")

	return cmd
}

// writeRedeployFinish renders the redeploy-finish screen to w (stdout): that
// the stack was redeployed, the services it recreated, then the post-redeploy
// status state. The layout is line-oriented and free of table-art so cut(1)
// and awk(1) stay usable, mirroring writeRestartFinish. The "redeployed and
// running" headline is gated on the post-redeploy status: needs-attention means
// a container did not come back cleanly or verification failed, so the headline
// stays neutral and defers to the status block below.
func writeRedeployFinish(w io.Writer, result *types.RestartResult) error {
	var b strings.Builder

	if result.Status != nil && result.Status.NeedsAttention {
		fmt.Fprintf(&b, "%s was redeployed; see the status below for services that need attention.\n", result.AppID)
	} else {
		fmt.Fprintf(&b, "%s was redeployed and is running.\n", result.AppID)
	}

	if len(result.RestartedServices) > 0 {
		b.WriteString("\nRecreated services:\n")
		for _, service := range result.RestartedServices {
			fmt.Fprintf(&b, "  - %s\n", service)
		}
	}

	if result.Status != nil {
		fmt.Fprintf(&b, "\nStatus: %s\n", result.Status.State)
		if result.Status.Message != "" {
			fmt.Fprintf(&b, "  %s\n", result.Status.Message)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("apps redeploy: writing finish screen: %w", err)
	}
	return nil
}
