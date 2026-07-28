package web

import (
	"os/exec"
	"testing"
)

// TestJSI18NCoverage runs the node checks over i18n.js: every t('key') used in
// the frontend must exist in every language table. t() falls back to the key
// itself, so a missing one renders as raw text (the update dialog shipped
// showing a literal "update_downloading"). Skipped when node isn't installed.
func TestJSI18NCoverage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/i18n.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("i18n coverage failed:\n%s", out)
	}
	t.Logf("%s", out)
}
