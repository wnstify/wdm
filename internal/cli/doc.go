// Package cli implements the wdm Cobra command tree (PRD §32, §37).
// Import boundary: internal/cli MUST NOT import any internal/* sibling —
// the depguard cli-uses-engine rule (.golangci.yml) enforces this. The
// engine reaches subcommands via injection: cmd/wdm supplies a
// `func (engine.Engine, error)` closure to [NewRootCmd], and subcommand
// leaves invoke that factory on their execution path, never at construction
// time. internal/cli sees only the public API — pkg/engine + pkg/types — so
// the future GUI (PRD §37) can plug into the same contract.
// Persistent flags: --json (PRD §32) and --debug (PRD §24). --debug is
// registered here so Cobra accepts it and --help documents it, but it is
// consumed by cmd/wdm before flag parsing, since the engine — whose default
// file sink the flag raises to debug level — is constructed there.
// Output discipline (PRD §32): subcommands branch on the --json flag (read
// via cmd.Flags.GetBool("json")). When set, output goes through [EmitJSON],
// which wraps the payload in the wdm.v1 envelope (types.Envelope). Plain
// human-readable output is the default and writes to cmd.OutOrStdout;
// errors and diagnostics write to cmd.ErrOrStderr (golang-cli: output to
// stdout, logs to stderr).
// Error discipline: the root command sets SilenceUsage and SilenceErrors so
// cmd/wdm controls stderr output and PRD §27 exit-code mapping. Subcommands
// wrap their domain errors via [types.WrapError] so cmd/wdm can route them to
// the correct exit code; untyped errors from [cobra.Command.Execute] default
// to (usage/validation), since they originate from Cobra's flag and
// subcommand parser.
package cli
