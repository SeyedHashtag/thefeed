package web

import (
	"os/exec"
	"testing"
)

// TestJSDownloadUnit runs the zero-dependency node unit tests for
// triggerDownload: desktop browsers must get a real <a download> even though
// they expose the Web Share API, and iOS must keep using the share sheet.
// Skipped when node isn't installed.
func TestJSDownloadUnit(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/download.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("js unit tests failed:\n%s", out)
	}
	t.Logf("%s", out)
}
