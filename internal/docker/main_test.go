package docker

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the package test suite under goleak so the streaming
// goroutines this package owns since — the scanner
// pumps, the cancel watchdog (dockerCancelWaitDelay), and the stderr
// drains in ComposeLogs/StreamLogs — are proven to be joined by the
// time every test returns. A leaked pump or watchdog fails the suite
// here rather than silently surviving into the next test.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
