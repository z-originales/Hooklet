// Package heavy — shared test infrastructure.
//
// TestMain starts a single RabbitMQ container shared by all heavy tests.
// This keeps Docker overhead to 1 container for the entire heavy suite.
//
// All test functions receive the shared port via sharedRabbitPort and pass it
// to their harness with harness.WithRabbitPort(sharedRabbitPort).
package heavy

import (
	"os"
	"testing"

	"hooklet/test/e2e/internal/harness"
)

// sharedRabbitPort is set once by TestMain and consumed by every test via
// harness.WithRabbitPort. It is package-level so all test files can read it.
var sharedRabbitPort int

func TestMain(m *testing.M) {
	port, cleanup := harness.StartSharedRabbit()
	sharedRabbitPort = port
	code := m.Run()
	cleanup()
	os.Exit(code)
}
