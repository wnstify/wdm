package logging

import (
	"time"
)

// PRD §24 fixes the wdm log retention policy: keep files for up to
// 30 days or 50 files, whichever bound is smaller, and always retain
// latest.log. These constants express that policy in code for the
// retention path ([OpenLogFile] → archive + prune) to consume.
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
