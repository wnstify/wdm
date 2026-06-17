package core_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the package test suite under goleak. internal/core
// drives the internal/docker streaming path end-to-end (Logs, plus the
// status/update/remove inspection calls), so a goroutine leaked by a
// log stream or a lingering fake-docker subprocess helper surfaces here
// as a suite failure instead of leaking across tests.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
