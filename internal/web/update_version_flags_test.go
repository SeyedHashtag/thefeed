package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// post sends one settings patch and fails on a non-2xx reply.
func postSettings(t *testing.T, s *Server, body string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSettings(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("POST %s = %d: %s", body, rec.Code, rec.Body.String())
	}
}

func getSettings(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /api/settings = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// downloadedVersion must be its own field. Downloading an update used to write
// skipUpdateVersion — the same flag the "don't show again" button sets — so a
// user who downloaded but never installed lost the prompt for that version for
// good. Recording a download must NOT imply a skip.
func TestDownloadedVersionIsSeparateFromSkip(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}

	postSettings(t, s, `{"downloadedVersion":"0.36.0"}`)
	got := getSettings(t, s)
	if got["downloadedVersion"] != "0.36.0" {
		t.Errorf("downloadedVersion = %v, want 0.36.0", got["downloadedVersion"])
	}
	if v := got["skipUpdateVersion"]; v != "" && v != nil {
		t.Errorf("downloading must not set skipUpdateVersion, got %v", v)
	}

	// The explicit skip is independent and does not disturb the download mark.
	postSettings(t, s, `{"skipUpdateVersion":"0.36.0"}`)
	got = getSettings(t, s)
	if got["skipUpdateVersion"] != "0.36.0" {
		t.Errorf("skipUpdateVersion = %v, want 0.36.0", got["skipUpdateVersion"])
	}
	if got["downloadedVersion"] != "0.36.0" {
		t.Errorf("downloadedVersion clobbered: %v", got["downloadedVersion"])
	}
}

// A patch that omits the fields must leave them untouched — the frontend sends
// one key at a time.
func TestUpdateVersionFlagsSurviveUnrelatedPatch(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	postSettings(t, s, `{"downloadedVersion":"1.2.3"}`)
	postSettings(t, s, `{"fontSize":18}`)

	got := getSettings(t, s)
	if got["downloadedVersion"] != "1.2.3" {
		t.Errorf("downloadedVersion = %v, want 1.2.3", got["downloadedVersion"])
	}
	if got["fontSize"] != float64(18) {
		t.Errorf("fontSize = %v, want 18", got["fontSize"])
	}
}

// Clearing is how a fresh install / reset is expressed.
func TestDownloadedVersionCanBeCleared(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	postSettings(t, s, `{"downloadedVersion":"9.9.9"}`)
	postSettings(t, s, `{"downloadedVersion":""}`)
	if v := getSettings(t, s)["downloadedVersion"]; v != "" && v != nil {
		t.Errorf("downloadedVersion = %v, want empty", v)
	}
}
