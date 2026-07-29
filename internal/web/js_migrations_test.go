package web

import (
	"os/exec"
	"testing"
)

// TestJSMigrations runs the node unit tests for the one-time data migrations:
// a single-user client tracks its applied level on the server (its loopback
// port changes and wipes localStorage), while a shared backend tracks it per
// browser and writes no global settings. Skipped when node isn't installed.
func TestJSMigrations(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/migrations.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("migration unit tests failed:\n%s", out)
	}
	t.Logf("%s", out)
}
