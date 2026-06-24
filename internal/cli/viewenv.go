package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newViewEnvCmd builds the top-level `view-env <app-id>` command: the
// read-only, redaction-safe view of a managed stack's effective environment
// (base .env merged with the user overlay .env.user). The engine masks every
// secret before the value reaches this layer, so this leaf only renders what
// it is given — it never re-derives or unmasks. Output follows the root's
// --json persistent flag: a line-oriented table on stdout in plain mode, the
// wdm.v1 envelope under --json (PRD §11, §24, §32).
func newViewEnvCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "view-env <app-id>",
		Short: "Show a managed app's effective environment with secrets redacted",
		Long: `View-env shows a managed app's effective environment — the base
.env merged with the .env.user overlay — with every secret value masked.

It is read-only and headless-safe. Secret values are redacted by the
engine before they reach output, so the view never prints a raw secret.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("view-env: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			result, err := eng.ViewEnvRedacted(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, result, writeViewEnv)
		},
	}

	return cmd
}

// writeViewEnv renders the redacted environment view to w (stdout). The
// layout is line-oriented (key TAB value, secret rows tagged) so cut(1) and
// awk(1) stay usable. Values are already redacted by the engine; this writer
// prints them verbatim and never reconstructs a raw secret.
func writeViewEnv(w io.Writer, result *types.ViewEnvResult) error {
	var b strings.Builder

	fmt.Fprintf(&b, "Environment for %s:\n", result.AppID)
	if len(result.Entries) == 0 {
		b.WriteString("  (no environment entries)\n")
	}
	for _, entry := range result.Entries {
		fmt.Fprintf(&b, "  %s\t%s", entry.Key, entry.Value)
		if entry.Secret {
			b.WriteString("\t(secret)")
		}
		b.WriteByte('\n')
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("view-env: writing output: %w", err)
	}
	return nil
}
