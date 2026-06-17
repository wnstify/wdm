//go:build unix

package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/pkg/types"
)

// UpdateCheckFilename is the on-disk name of the update-check state file,
// stored beside runtime.lock under ~/.local/state/wdm. It records when the
// last successful daily-on-launch update check ran so the gate fires at most
// once per calendar day across launches.
const UpdateCheckFilename = "update-check.json"

// updateCheckSchemaVersion is the schema_version written into every
// update-check.json by this package. It mirrors the runtime.lock
// forward-compat marker ([runtimeLockSchemaVersion]); bump only when the
// field set changes in a forward-incompatible way. Callers MUST read the
// value back from [UpdateCheckState.SchemaVersion] rather than hard-coding
// it elsewhere.
const updateCheckSchemaVersion = 1

// UpdateCheckState is the on-disk JSON shape of
// ~/.local/state/wdm/update-check.json. It persists the instant of the
// last SUCCESSFUL daily-on-launch update check so the gate runs at most
// once per calendar day across launches.
// It carries no secrets — only a timestamp and a schema marker — but is
// written at 0o600 to match the per-user posture of runtime.lock.
// Field tags use snake_case so the file reads identically to the other
// persisted state (runtime.lock) and to what downstream debugging
// (cat / jq) expects.
type UpdateCheckState struct {
	// SchemaVersion is the stable forward-compat marker; today always
	// [updateCheckSchemaVersion] (= 1).
	SchemaVersion int `json:"schema_version"`

	// LastCheck is the instant of the last successful daily-on-launch
	// update check, encoded as RFC3339 by encoding/json's default
	// time.Time marshaling. The zero value means "never checked" — the
	// gate treats a never-checked state as due.
	LastCheck time.Time `json:"last_check"`
}

// UpdateCheckPath returns the absolute path of the update-check state file
// under stateDir. It mirrors how the engine builds the runtime.lock path
// (filepath.Join(stateDir, …)); stateDir is the engine-resolved
// ~/.local/state/wdm and is expected to be absolute.
func UpdateCheckPath(stateDir string) string {
	return filepath.Join(stateDir, UpdateCheckFilename)
}

// LoadUpdateCheckState reads the update-check state from
// <stateDir>/update-check.json.
// An absent file returns a zero-value [*UpdateCheckState] (meaning "never
// checked") and a nil error: absence is the normal first-launch condition,
// not a failure. A present-but-malformed file returns a typed
// [types.ErrCodeGeneric] error so the caller can surface a diagnostic
// rather than silently treating corruption as "never checked".
// stateDir MUST be absolute (the engine resolves it once at construction).
func LoadUpdateCheckState(stateDir string) (*UpdateCheckState, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return nil, fmt.Errorf("state.LoadUpdateCheckState: stateDir must be absolute, got %q", stateDir)
	}

	path := UpdateCheckPath(stateDir)
	// G304 is suppressed: path is the engine-controlled XDG state location
	// (UpdateCheckPath joins the validated-absolute stateDir with a fixed
	// filename), so no relative re-injection is possible.
	raw, err := os.ReadFile(path) //nolint:gosec // G304: engine-controlled XDG path under validated-absolute stateDir
	if errors.Is(err, os.ErrNotExist) {
		return &UpdateCheckState{}, nil
	}
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"could not read update-check state",
			"verify the state directory is readable by the current user",
			fmt.Errorf("state.LoadUpdateCheckState: reading %q: %w", path, err),
		)
	}

	var st UpdateCheckState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"update-check state is corrupt",
			fmt.Sprintf("remove %s and let the next launch recreate it", path),
			fmt.Errorf("state.LoadUpdateCheckState: parsing %q: %w", path, err),
		)
	}
	return &st, nil
}

// SaveUpdateCheckState persists st to <stateDir>/update-check.json via the
// atomic temp+fsync+rename write at 0o600 (no secrets are written — only a
// timestamp and a schema marker — but the restrictive mode matches the
// per-user posture of runtime.lock). The parent directory is created on
// demand by [WriteFileAtomic].
// SchemaVersion is forced to [updateCheckSchemaVersion] so a caller cannot
// persist a stale or zero marker; stateDir MUST be absolute.
func SaveUpdateCheckState(stateDir string, st *UpdateCheckState) error {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return fmt.Errorf("state.SaveUpdateCheckState: stateDir must be absolute, got %q", stateDir)
	}
	if st == nil {
		return fmt.Errorf("state.SaveUpdateCheckState: st must not be nil")
	}

	out := UpdateCheckState{
		SchemaVersion: updateCheckSchemaVersion,
		LastCheck:     st.LastCheck,
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"could not encode update-check state",
			"this is an internal error; please report it",
			fmt.Errorf("state.SaveUpdateCheckState: marshaling state: %w", err),
		)
	}

	path := UpdateCheckPath(stateDir)
	if err := WriteFileAtomic(path, raw, security.SecretFileMode); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"could not write update-check state",
			"verify the state directory is writable by the current user",
			fmt.Errorf("state.SaveUpdateCheckState: writing %q: %w", path, err),
		)
	}
	return nil
}

// DueForDailyCheck reports whether a daily-on-launch update check is due,
// given the last successful check instant and the current time. It is a
// pure function so the calendar logic is exhaustively testable with a
// pinned clock.
// Semantics:
//   - never checked (zero lastCheck) → due (true).
//   - lastCheck is NOT the same calendar day as now → due (true).
//   - lastCheck IS the same calendar day as now → not due (false).
//
// "Same calendar day" is computed in LOCAL time: both instants are
// converted to [time.Local] and their year/month/day compared. The day
// boundary is therefore the user's local midnight — "daily" from the
// user's wall-clock perspective — not UTC midnight. Raw instants are never
// compared directly.
func DueForDailyCheck(lastCheck, now time.Time) bool {
	if lastCheck.IsZero() {
		return true
	}
	return !sameLocalDay(lastCheck, now)
}

// sameLocalDay reports whether a and b fall on the same calendar day in the
// host's local time zone. Both instants are converted to [time.Local]
// before the year/month/day comparison so the boundary is local midnight.
func sameLocalDay(a, b time.Time) bool {
	ay, am, ad := a.In(time.Local).Date()
	by, bm, bd := b.In(time.Local).Date()
	return ay == by && am == bm && ad == bd
}
