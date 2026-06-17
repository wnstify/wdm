package core

import (
	"context"
	"fmt"
	"time"

	"github.com/wnstify/wdm/internal/state"
)

// updateCheckPreferenceDaily is the only [types.Settings.UpdateCheckPreference]
// value that can trigger an automatic launch check; "manual" and "disabled"
// never do. It repeats the literal validated in validateSettings —
// the project has no shared named constant for these preference values, and
// introducing one is out of scope.
const updateCheckPreferenceDaily = "daily-on-launch"

// coreUpdateCheckNow is the core-side clock seam for the daily launch-check
// gate. It returns LOCAL wall-clock time (NOT UTC) because the calendar-day
// boundary the gate uses is the user's local midnight (see
// [state.DueForDailyCheck]). Tests pin it to drive the gate deterministically,
// following the [state] package's backupNowUTC precedent.
var coreUpdateCheckNow = func() time.Time { return time.Now() }

// DailyLaunchCheckDue reports whether an automatic daily-on-launch update
// check is currently due. It returns true only when the user's
// UpdateCheckPreference is "daily-on-launch" AND no successful check has been
// recorded for the current local calendar day; "manual" and
// "disabled" always return false.
// The update-check state is read from the resolved state dir on every call
// rather than cached on the engine, because UpdateSettings rewrites
// config.toml without refreshing the in-memory settings cache — caching the
// gate state here would risk a stale answer. A load failure (a corrupt state
// file) is propagated so the caller can surface it; an absent state file is
// the normal first-launch "never checked" condition and yields due.
// This is the read half of the launch-check gate E2 surfaces through
// pkg/engine; it neither mutates state nor performs any network work. It
// returns [ErrClosed] after [Engine.Close] and propagates a canceled context.
func (e *Engine) DailyLaunchCheckDue(ctx context.Context) (bool, error) {
	if e.isClosed() {
		return false, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("core.DailyLaunchCheckDue: %w", err)
	}
	if e.settings.UpdateCheckPreference != updateCheckPreferenceDaily {
		return false, nil
	}

	st, err := state.LoadUpdateCheckState(e.stateDir)
	if err != nil {
		return false, err
	}
	return state.DueForDailyCheck(st.LastCheck, coreUpdateCheckNow()), nil
}

// RecordDailyLaunchCheck records that a daily-on-launch update check ran by
// persisting the current instant as the last-check time under the resolved
// state dir.
// The caller MUST invoke this ONLY after a check has SUCCEEDED: a
// failed or offline check does not record, so the gate stays open and the
// check is retried on the next launch. The write is atomic and restrictive
// (0o600); it stores only a timestamp, no secrets.
// This is the record half of the launch-check gate E2 surfaces through
// pkg/engine. It returns [ErrClosed] after [Engine.Close] and propagates a
// canceled context.
func (e *Engine) RecordDailyLaunchCheck(ctx context.Context) error {
	if e.isClosed() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("core.RecordDailyLaunchCheck: %w", err)
	}

	st := &state.UpdateCheckState{LastCheck: coreUpdateCheckNow()}
	return state.SaveUpdateCheckState(e.stateDir, st)
}
