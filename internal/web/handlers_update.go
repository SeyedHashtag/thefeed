package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sartoopjj/thefeed/internal/update"
)

func (s *Server) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	v, err := s.checkLatestVersion(r.Context())
	if err != nil {
		if strings.Contains(err.Error(), "no config") {
			http.Error(w, "no config", 400)
			return
		}
		errStr := err.Error()
		if strings.Contains(errStr, "integrity check failed") || strings.Contains(errStr, "message authentication failed") || strings.Contains(errStr, "cipher") {
			http.Error(w, "invalid passphrase", 400)
			return
		}
		http.Error(w, fmt.Sprintf("version check failed: %v", err), 502)
		return
	}

	writeJSON(w, map[string]any{"ok": true, "latestVersion": v})
}

// handleGitHubUpdateCheck reads the latest GitHub Release tag for
// sartoopjj/thefeed and returns a download URL tailored to this
// binary's platform. Independent of the DNS-protocol version check
// above — works without a configured profile.
func (s *Server) handleGitHubUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	st, err := update.Check(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("update check failed: %v", err), 502)
		return
	}
	writeJSON(w, st)
}

// handleUpdateDownload proxies a release asset through the server so
// the UI can paint a progress bar (and Android can route the bytes
// through the native bridge to MediaStore Downloads). The frontend
// hits this with ?version=v0.19.1[&asset=...]; the server streams
// the body back with Content-Disposition: attachment.
func (s *Server) handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	// Defence in depth: an explicit ?asset= would skip the checks below.
	if update.StoreBuild() {
		http.Error(w, "in-app updates are disabled in store builds", 403)
		return
	}
	q := r.URL.Query()
	v := strings.TrimSpace(q.Get("version"))
	if v == "" {
		http.Error(w, "version query parameter required", 400)
		return
	}
	asset := strings.TrimSpace(q.Get("asset"))
	if asset == "" {
		asset = update.AssetFilename(v)
	}
	if asset == "" {
		http.Error(w, "no asset known for this platform; pass &asset=... explicitly", 400)
		return
	}

	// 10-minute budget covers the largest shipped artifact (signed
	// Android APK, ~20 MB) on slow links.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	s.addLog(fmt.Sprintf("update: download requested version=%s asset=%s", v, asset))
	err := update.StreamAsset(ctx, v, asset, w, s.addLog)
	if err != nil {
		// If body bytes already flew, http.Error writes after the
		// fact and the browser sees a truncated download — the real
		// error is in /api/logs. Pre-write errors return a clean 502.
		s.addLog(fmt.Sprintf("update: download failed: %v", err))
		http.Error(w, fmt.Sprintf("download failed: %v", err), 502)
		return
	}
}
