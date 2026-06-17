package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// These tests close the cliConfirmer prompt-matrix debt carried forward to
// the test package is `package cli`, so the unexported type is
// constructible — with crafted stdin readers and a buffer for stderr, and
// pin the full the confirmation rulesgating matrix:
//	                                   | TTY | no TTY
//	-----------------------------------+------------+------------------
//	safe + --yes | accept | accept
//	safe + no --yes | prompt y/N | reject (canceled)
//	database-risk + --accept-db-risk | accept | accept
//	database-risk + no flag | refuse | refuse
// isTTY is set on the struct literal rather than probed from os.Stdin so
// the TTY-prompt arms are exercisable without a real terminal — the
// production probe (stdinIsTTY) sets the field at construction, but the
// field itself is the seam the gating logic reads, so a test that sets it
// directly drives the same code path a real TTY would.

// safeConfirmation is the install/update/remove recreate consequence —
// any Kind other than the database-risk literal. --yes accepts it; a TTY
// prompts; no TTY without --yes declines.
func safeConfirmation() types.Confirmation {
	return types.Confirmation{
		Kind:    "update_deploy",
		Title:   "Recreate containers for vaultwarden",
		Message: "The stack will be recreated with the new images.",
	}
}

// dbRiskConfirmation is the PRD §20 database-risk warning. It is FLAG-ONLY
// in: only --accept-database-risk accepts it; neither --yes nor a
// TTY "y" ever does.
func dbRiskConfirmation() types.Confirmation {
	return types.Confirmation{
		Kind:    confirmationKindDatabaseRisk,
		Title:   "Database-risk update for vaultwarden 1.0.0 -> 1.1.0",
		Message: "This update may run database migrations that cannot be rolled back.",
	}
}

// TestConfirm_SafeYes_AcceptsWithoutPrompt pins that --yes accepts a safe
// confirmation on both TTY and non-TTY paths without reading stdin or
// printing a prompt: the in reader is failingReader (a read panics the
// test if consulted) and stderr stays empty.
func TestConfirm_SafeYes_AcceptsWithoutPrompt(t *testing.T) {
	t.Parallel()

	for _, isTTY := range []bool{true, false} {
		t.Run(ttyName(isTTY), func(t *testing.T) {
			t.Parallel()

			var errOut strings.Builder
			c := &cliConfirmer{
				err:   &errOut,
				in:    &failingReader{t: t},
				yes:   true,
				isTTY: isTTY,
			}

			ok, err := c.Confirm(t.Context(), safeConfirmation())
			require.NoError(t, err)
			assert.True(t, ok, "--yes must accept a safe confirmation")
			assert.Empty(t, errOut.String(), "--yes accept must not print a prompt")
		})
	}
}

// TestConfirm_SafeNoYesNoTTY_RejectsClosed pins that a safe confirmation
// without --yes and without a TTY declines fail-closed as (false, nil) —
// the engine maps that to ErrCodeUserCanceled —
// and never reads stdin.
func TestConfirm_SafeNoYesNoTTY_RejectsClosed(t *testing.T) {
	t.Parallel()

	c := &cliConfirmer{
		err:   &strings.Builder{},
		in:    &failingReader{t: t},
		yes:   false,
		isTTY: false,
	}

	ok, err := c.Confirm(t.Context(), safeConfirmation())
	require.NoError(t, err, "a no-TTY refusal is (false, nil), not an error")
	assert.False(t, ok, "no --yes and no TTY must decline a safe confirmation")
}

// TestConfirm_SafeTTYPrompt_AnswerMatrix pins the interactive y/N prompt
// for a safe confirmation on a TTY: only "y"/"yes" (case-insensitive,
// whitespace-trimmed) accept; an empty line, EOF (no trailing newline),
// and any non-affirmative answer decline (default No). The prompt banner
// and the confirmation Title/Message must reach stderr.
func TestConfirm_SafeTTYPrompt_AnswerMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		answer string
		accept bool
	}{
		{name: "lowercase y", answer: "y\n", accept: true},
		{name: "lowercase yes", answer: "yes\n", accept: true},
		{name: "uppercase Y", answer: "Y\n", accept: true},
		{name: "mixed-case YeS", answer: "YeS\n", accept: true},
		{name: "y with surrounding spaces", answer: "  y  \n", accept: true},
		{name: "empty line declines", answer: "\n", accept: false},
		{name: "eof without newline declines", answer: "", accept: false},
		{name: "n declines", answer: "n\n", accept: false},
		{name: "no declines", answer: "no\n", accept: false},
		{name: "garbage declines", answer: "maybe\n", accept: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var errOut strings.Builder
			cfg := safeConfirmation()
			c := &cliConfirmer{
				err:   &errOut,
				in:    strings.NewReader(tc.answer),
				yes:   false,
				isTTY: true,
			}

			ok, err := c.Confirm(t.Context(), cfg)
			require.NoError(t, err)
			assert.Equal(t, tc.accept, ok, "answer %q acceptance", tc.answer)

			banner := errOut.String()
			assert.Contains(t, banner, cfg.Title, "prompt must surface the confirmation Title on stderr")
			assert.Contains(t, banner, cfg.Message, "prompt must surface the confirmation Message on stderr")
			assert.Contains(t, banner, "[y/N]", "prompt must show the y/N affordance with No as the default")
		})
	}
}

// TestConfirm_DBRisk_FlagAcceptsEchoes pins that --accept-database-risk
// authorizes the database-risk warning, echoes the verbatim Title and
// Message plus the acceptance line naming the flag to stderr, and never
// reads stdin (the decision is flag-only). It holds on both TTY and
// non-TTY paths because the database-risk row is flag-only by design.
func TestConfirm_DBRisk_FlagAcceptsEchoes(t *testing.T) {
	t.Parallel()

	for _, isTTY := range []bool{true, false} {
		t.Run(ttyName(isTTY), func(t *testing.T) {
			t.Parallel()

			var errOut strings.Builder
			cfg := dbRiskConfirmation()
			c := &cliConfirmer{
				err:          &errOut,
				in:           &failingReader{t: t},
				yes:          false,
				acceptDBRisk: true,
				isTTY:        isTTY,
			}

			ok, err := c.Confirm(t.Context(), cfg)
			require.NoError(t, err)
			assert.True(t, ok, "--accept-database-risk must authorize the database-risk warning")

			out := errOut.String()
			assert.Contains(t, out, cfg.Title, "the warning Title must be echoed on acceptance")
			assert.Contains(t, out, cfg.Message, "the verbatim warning Message must be echoed on acceptance")
			assert.Contains(t, out, acceptDatabaseRiskFlag, "the acceptance line must name the flag that authorized it")
		})
	}
}

// TestConfirm_DBRisk_NoFlagRefusesNamingFlag pins that without
// --accept-database-risk the database-risk warning is refused fail-closed
// as (false, nil) — even with --yes, even on a TTY (the FLAG-ONLY rule:
// --yes and a TTY "y" never satisfy it). The refusal prints the verbatim
// warning and names --accept-database-risk so the decline is never silent,
// and never reads stdin.
func TestConfirm_DBRisk_NoFlagRefusesNamingFlag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		yes   bool
		isTTY bool
	}{
		{name: "no yes, no tty", yes: false, isTTY: false},
		{name: "no yes, tty", yes: false, isTTY: true},
		{name: "yes does not satisfy db-risk", yes: true, isTTY: false},
		{name: "yes and tty still refuse", yes: true, isTTY: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var errOut strings.Builder
			cfg := dbRiskConfirmation()
			c := &cliConfirmer{
				err:          &errOut,
				in:           &failingReader{t: t},
				yes:          tc.yes,
				acceptDBRisk: false,
				isTTY:        tc.isTTY,
			}

			ok, err := c.Confirm(t.Context(), cfg)
			require.NoError(t, err, "a database-risk refusal is (false, nil), not an error")
			assert.False(t, ok, "without --accept-database-risk the database-risk warning must be refused")

			out := errOut.String()
			assert.Contains(t, out, cfg.Message, "the verbatim warning must print before refusing")
			assert.Contains(t, out, acceptDatabaseRiskFlag, "the refusal must name the flag that would authorize it")
		})
	}
}

// TestConfirm_ContextCanceledFirst pins that a context already canceled at
// entry surfaces a non-nil error before any branch runs (ctx.Err first),
// for both a safe and a database-risk confirmation, and never reads stdin
// or prints anything.
func TestConfirm_ContextCanceledFirst(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		conf types.Confirmation
	}{
		{name: "safe", conf: safeConfirmation()},
		{name: "database-risk", conf: dbRiskConfirmation()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			var errOut strings.Builder
			c := &cliConfirmer{
				err:          &errOut,
				in:           &failingReader{t: t},
				yes:          true, // would otherwise accept; the ctx check must win
				acceptDBRisk: true,
				isTTY:        true,
			}

			ok, err := c.Confirm(ctx, tc.conf)
			require.Error(t, err, "a canceled context must surface an error before any decision")
			assert.ErrorIs(t, err, context.Canceled)
			assert.False(t, ok, "a canceled context must not accept")
			assert.Empty(t, errOut.String(), "a ctx-canceled refusal must not print a prompt")
		})
	}
}

// TestConfirm_PromptReadErrorWraps pins that a non-EOF read error from the
// answer source on the TTY prompt path surfaces as a wrapped error (not a
// silent decline): the read failure is something the operator should see,
// unlike an EOF which legitimately declines.
func TestConfirm_PromptReadErrorWraps(t *testing.T) {
	t.Parallel()

	readErr := errors.New("stdin device error")
	c := &cliConfirmer{
		err:   &strings.Builder{},
		in:    &errReader{err: readErr},
		yes:   false,
		isTTY: true,
	}

	ok, err := c.Confirm(t.Context(), safeConfirmation())
	require.Error(t, err, "a non-EOF read error must surface, not silently decline")
	assert.ErrorIs(t, err, readErr, "the underlying read error must remain reachable in the chain")
	assert.False(t, ok)
}

// TestConfirm_NewCLIConfirmer_WiresFields pins that the constructor sets
// every gating field from its arguments. The probe's value depends on the
// harness's stdin (go test typically wires /dev/null, a char device), so
// the assertion compares against a fresh stdinIsTTY reading rather than
// a hard-coded value.
func TestConfirm_NewCLIConfirmer_WiresFields(t *testing.T) {
	t.Parallel()

	out := &strings.Builder{}
	errOut := &strings.Builder{}
	in := strings.NewReader("")

	c := newCLIConfirmer(out, errOut, in, true, true)

	assert.Same(t, out, c.out, "out must be wired through")
	assert.Same(t, errOut, c.err, "err must be wired through")
	assert.Same(t, in, c.in, "in must be wired through")
	assert.True(t, c.yes, "assumeYes must wire the yes field")
	assert.True(t, c.acceptDBRisk, "acceptDBRisk must wire the acceptDBRisk field")
	// isTTY is probed from os.Stdin at construction, so its value depends on
	// how the test process was launched (a terminal vs a pipe). The probe
	// must agree with a fresh stdinIsTTY reading rather than a hard-coded
	// value — pinning that the constructor consults the probe, not that the
	// environment is non-interactive.
	assert.Equal(t, stdinIsTTY(), c.isTTY, "isTTY must come from the stdinIsTTY probe")
}

func ttyName(isTTY bool) string {
	if isTTY {
		return "tty"
	}
	return "no_tty"
}

// failingReader fails the test if Read is ever called. It backs the
// confirmer's `in` on the arms that must decide without consulting stdin
// (--yes accept, no-TTY decline, every database-risk arm, the ctx-first
// refusal), proving those paths never touch the answer source.
type failingReader struct{ t *testing.T }

func (r *failingReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("Read called on a path that must not consult stdin")
	return 0, io.EOF
}

// errReader returns a fixed non-EOF error on the first Read, modeling a
// broken stdin device on the prompt path.
type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }
