package logging

import (
	"context"
	"time"
)

// PRD §24 fixes the wdm log retention policy: keep files for up to
// 30 days or 50 files, whichever bound is smaller, and always retain
// latest.log. These constants express that policy in code for the
// concrete rotator to consume.
const (
	// RetentionMaxAge bounds the age of any non-[LatestLogName] log
	// file before the rotator removes it (PRD §24).
	RetentionMaxAge = 30 * 24 * time.Hour

	// RetentionMaxFiles caps the total number of log files
	// (including [LatestLogName]) kept in the log directory
	// (PRD §24).
	RetentionMaxFiles = 50

	// LatestLogName is the file name PRD §24 reserves for the
	// current session's log; it is always retained regardless of
	// age or count.
	LatestLogName = "latest.log"
)

// Rotator owns the wdm log directory lifecycle: it archives the
// current [LatestLogName] under a timestamped name, opens a fresh
// one, and applies the retention policy from [RetentionMaxAge],
// [RetentionMaxFiles], and the always-keep rule for [LatestLogName]
// (PRD §24).
// [NoopRotator] is available for callers that do not want file rotation.
type Rotator interface {
	// Rotate archives the current [LatestLogName], opens a fresh
	// one, and prunes any file that violates the retention policy.
	// Implementations MUST honor ctx cancellation and MUST be safe
	// to call concurrently with writes to the active log file.
	Rotate(ctx context.Context) error
}

// NoopRotator is the default [Rotator]: it returns nil without
// touching disk. Wired by callers that need the Rotator contract
// before the concrete rotator lands (: the
// rotator only matters once real log files flow).
var NoopRotator Rotator = noopRotator{}

// noopRotator is the empty-struct backing for [NoopRotator]. It
// carries no state and is safe to share across goroutines.
type noopRotator struct{}

// Rotate returns nil without touching disk.
func (noopRotator) Rotate(_ context.Context) error { return nil }
