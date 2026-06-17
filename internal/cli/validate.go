package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newAppsValidateCmd builds the `apps validate <app-id>` leaf (PRD §18:418
// "Validate config", §18:427 compose-validation condition;
// the injected factory and renders the read-only [types.ValidationResult] in
// one of two forms based on the root's --json persistent flag:
//   - Plain mode: a scannable validation block on stdout — the app and a valid
//     yes/no verdict, the Compose project, the Compose file path, and, only
//     when invalid, the redactor-scrubbed detail lines.
//   - JSON mode: the ValidationResult wrapped in the wdm.v1 envelope on stdout
//     and nothing else (PRD §32).
//     ValidationResult marshals to a JSON object, so it is the envelope.data
//     object directly.
//
// Exit code: an invalid-but-readable Compose file (Valid:false) still exits 0.
// the engine returns a non-nil *ValidationResult with a nil error, exactly as
// `apps status` returns a needs_attention stack at exit 0. This leaf adds NO
// exit-code logic; cmd/wdm's exitCodeFor maps the engine's success-vs-failure
// line. Typed errors that drive non-zero PRD §27 codes are reserved for faults
// that prevent validation at all: an unmanaged or uninstalled app or a
// malformed.env (ErrCodeUsageValidation → 2), a busy stack
// (ErrCodeRuntimeLockHeld → 4), an unreachable daemon (ErrCodeDockerUnavailable
// → 5), or a generic fault (ErrCodeGeneric → 1). The §27 contract is that the
// exit code describes the command's outcome, not the subject's health.
// ValidateConfig is read-only (PRD §26): no runtime lock, no progress, no
// [types.Confirmer]. The engine factory is invoked inside RunE, and only
// there, so `wdm apps validate --help` never reaches [engine.New] (PRD §14
// self-update smoke-check invariant, mirrored from `apps status`).
func newAppsValidateCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate <app-id>",
		Short: "Validate a managed stack's Docker Compose configuration",
		Long: `Validate runs docker compose config against a managed app's
on-disk Compose file and reports whether it is valid.

Validate is read-only. An invalid Compose file still exits 0 — the exit
code reports whether validation could run, not whether the file is valid.
Read the valid field (or the detail under --json) to branch on the
verdict.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps validate: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			result, err := eng.ValidateConfig(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			if useJSON {
				return EmitJSON(cmd.OutOrStdout(), result)
			}
			return writeValidation(cmd.OutOrStdout(), result)
		},
	}

	return cmd
}

// writeValidation renders the plain-mode validation block to w (stdout). The
// layout is line-oriented and free of table-art so cut(1) and awk(1) stay
// usable: a header line with the app and the valid verdict, the Compose
// project and file when the engine recorded them, and — only when the verdict
// is invalid — the redactor-scrubbed detail indented beneath a "Detail:" label.
func writeValidation(w io.Writer, result *types.ValidationResult) error {
	var b strings.Builder

	verdict := "no"
	if result.Valid {
		verdict = "yes"
	}
	fmt.Fprintf(&b, "%s\tvalid=%s\n", result.AppID, verdict)

	if result.ComposeProject != "" {
		fmt.Fprintf(&b, "  project\t%s\n", result.ComposeProject)
	}
	if result.ComposeFile != "" {
		fmt.Fprintf(&b, "  file\t%s\n", result.ComposeFile)
	}

	if !result.Valid && result.Detail != "" {
		b.WriteString("\nDetail:\n")
		for _, line := range strings.Split(result.Detail, "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("apps validate: writing result: %w", err)
	}
	return nil
}
