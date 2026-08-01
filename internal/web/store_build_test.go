package web

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// Google Play's Device and Network Abuse policy forbids an app distributed
// through the store from downloading executable code or updating itself. The
// Play build sets THEFEED_STORE_BUILD=1 (mobile.NewAndroidPlayServer), and
// every in-app update path must go quiet — including when the UI is bypassed.
func TestStoreBuildRefusesUpdateDownload(t *testing.T) {
	t.Setenv("THEFEED_STORE_BUILD", "1")
	s := &Server{dataDir: t.TempDir()}

	// Even with an explicit &asset=, which skips the empty-template check.
	for _, url := range []string{
		"/api/update/download?version=v9.9.9",
		"/api/update/download?version=v9.9.9&asset=thefeed-android-v9.9.9-arm64-v8a.apk",
	} {
		rec := httptest.NewRecorder()
		s.handleUpdateDownload(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != 403 {
			t.Errorf("GET %s = %d, want 403 (store build must not serve executables)", url, rec.Code)
		}
	}
}

// The check must report no update at all, so the frontend never renders a
// prompt. It also returns before the network call — a store build has no
// reason to ask GitHub anything.
func TestStoreBuildReportsNoUpdate(t *testing.T) {
	t.Setenv("THEFEED_STORE_BUILD", "1")
	s := &Server{dataDir: t.TempDir()}

	rec := httptest.NewRecorder()
	s.handleGitHubUpdateCheck(rec, httptest.NewRequest("GET", "/api/update/github", nil))
	if rec.Code != 200 {
		t.Fatalf("update check = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["hasUpdate"] == true {
		t.Error("hasUpdate must be false in a store build")
	}
	if u, _ := got["downloadURL"].(string); u != "" {
		t.Errorf("downloadURL = %q, want empty in a store build", u)
	}
}

// The frontend hides its update entry points off this flag.
func TestStoreBuildExposedInSettings(t *testing.T) {
	t.Setenv("THEFEED_STORE_BUILD", "")
	s := &Server{dataDir: t.TempDir()}

	if got := getSettings(t, s)["storeBuild"]; got != false {
		t.Errorf("storeBuild = %v, want false for a normal build", got)
	}

	t.Setenv("THEFEED_STORE_BUILD", "1")
	if got := getSettings(t, s)["storeBuild"]; got != true {
		t.Errorf("storeBuild = %v, want true when the flag is set", got)
	}
}

// Without the flag nothing changes: the GitHub/desktop builds keep their
// updater. Guards against the gate being left permanently on.
func TestNonStoreBuildStillServesUpdates(t *testing.T) {
	t.Setenv("THEFEED_STORE_BUILD", "")
	s := &Server{dataDir: t.TempDir()}
	rec := httptest.NewRecorder()
	s.handleUpdateDownload(rec, httptest.NewRequest("GET", "/api/update/download", nil))
	// No version param -> 400, NOT the 403 the store gate would produce.
	if rec.Code != 400 {
		t.Errorf("normal build = %d, want 400 (missing version), not the store-build 403", rec.Code)
	}
}
