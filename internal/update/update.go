// Package update reads the latest GitHub Release tag for
// sartoopjj/thefeed and proxies the asset bytes when the user clicks
// Download. Separate from /api/version-check, which reads the
// version over the DNS protocol.
package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sartoopjj/thefeed/internal/version"
)

const (
	// Repo is the GitHub repo serving releases.
	Repo = "sartoopjj/thefeed"

	// LatestReleaseURL redirects to /releases/tag/{V}; HEAD it and
	// parse the Location header.
	LatestReleaseURL = "https://github.com/" + Repo + "/releases/latest"

	// BaseURL is the per-release asset directory.
	BaseURL = "https://github.com/" + Repo + "/releases/download"

	// browserUA mimics current Chrome on Windows.
	browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/124.0.0.0 Safari/537.36"
)

// Status is the JSON returned to the frontend.
type Status struct {
	Current     string `json:"current"`
	Latest      string `json:"latest"`
	HasUpdate   bool   `json:"hasUpdate"`
	DownloadURL string `json:"downloadURL"`
}

// httpClient performs the version check. CheckRedirect stops at the
// first hop so we can read the Location header.
var httpClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: stopOnRedirect,
}

func stopOnRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

// StoreBuild reports whether this binary ships through an app store, where
// both Google and Apple forbid an app from updating itself.
func StoreBuild() bool {
	return storeBuildFor(runtime.GOOS, os.Getenv("THEFEED_STORE_BUILD"))
}

// storeBuildFor is StoreBuild's testable core. GOOS=ios is how gomobile builds
// the App Store framework; the env flag comes from mobile.NewAndroidPlayServer.
// The macOS .dmg is a separate GOOS=darwin binary and keeps its updater.
func storeBuildFor(goos, env string) bool {
	return goos == "ios" || env == "1"
}

// Check fetches the latest tag and assembles a Status for the running
// platform.
func Check(ctx context.Context) (Status, error) {
	s := Status{Current: version.Version}
	// Nothing to offer, and returning early keeps a store build off the network.
	if StoreBuild() {
		return s, nil
	}
	latest, err := fetchLatestTag(ctx, httpClient, LatestReleaseURL)
	if err != nil {
		return s, err
	}
	s.Latest = latest
	s.HasUpdate = IsNewer(s.Latest, s.Current)
	s.DownloadURL = AssetURL(s.Latest)
	return s, nil
}

// fetchLatestTag sends a HEAD to url (LatestReleaseURL in production)
// and returns the trailing /tag/{V} segment from the redirect Location.
// url is a parameter so tests can hit httptest servers.
func fetchLatestTag(ctx context.Context, c *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("latest release: expected 3xx, got %s", resp.Status)
	}
	loc := resp.Header.Get("Location")
	idx := strings.LastIndex(loc, "/tag/")
	if idx < 0 {
		return "", fmt.Errorf("latest release: no /tag/ in Location %q", loc)
	}
	v := strings.TrimSpace(loc[idx+len("/tag/"):])
	// Strip any trailing slash or query.
	if i := strings.IndexAny(v, "/?#"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return "", fmt.Errorf("latest release: empty version in Location")
	}
	return v, nil
}

// AssetURL builds the github.com download URL for the running
// platform at the requested version.
func AssetURL(latest string) string {
	name := AssetFilename(latest)
	if name == "" {
		return ""
	}
	return BaseURL + "/" + strings.TrimSpace(latest) + "/" + name
}

// AssetFilename returns just the asset name for the running platform
// at the given version (e.g. "thefeed-client-v0.19.1-darwin-arm64").
// Falls back to a runtime-derived template if AssetTemplate wasn't
// injected at build time (e.g., `go run`).
func AssetFilename(latest string) string {
	tmpl := version.AssetTemplate
	if isAndroidAPK() {
		// APK wrapper takes priority over the bare client binary —
		// users who installed the APK should update the APK.
		tmpl = androidAPKTemplate()
	}
	if isMacApp() {
		// Same idea on macOS: a user who installed via DMG should be
		// pointed at the next DMG, not at a bare binary they'd have
		// to launch from Terminal. Overrides the build-time template
		// because the .app bundles the same darwin binary the CLI
		// users download — only the runtime context differs.
		tmpl = macAppTemplate()
	}
	if tmpl == "" {
		tmpl = defaultTemplate()
	}
	if tmpl == "" {
		return ""
	}
	return strings.ReplaceAll(tmpl, "{V}", strings.TrimSpace(latest))
}

// IsNewer compares semver-ish version strings, tolerating the "v" prefix
// and numeric pre-release suffixes. Returns false if either side is "dev".
func IsNewer(latest, current string) bool {
	a := strings.TrimPrefix(strings.TrimSpace(latest), "v")
	b := strings.TrimPrefix(strings.TrimSpace(current), "v")
	if a == "" || b == "" {
		return false
	}
	if b == "dev" {
		// `go run` / unreleased build — never nag.
		return false
	}
	if a == b {
		return false
	}
	as := strings.Split(stripPre(a), ".")
	bs := strings.Split(stripPre(b), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
		}
		if ai > bi {
			return true
		}
		if ai < bi {
			return false
		}
	}
	return false
}

func stripPre(v string) string {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		return v[:i]
	}
	return v
}

// isAndroidAPK returns true when this binary is running inside the
// Android APK wrapper rather than as a standalone Termux/CLI client.
// Two signals are checked:
//   - THEFEED_ANDROID_APK=1 set by ThefeedService.kt before exec.
//   - The executable path lives under com.thefeed.android's
//     nativeLibraryDir, which always contains "com.thefeed.android".
func isAndroidAPK() bool {
	if runtime.GOOS != "android" {
		return false
	}
	if os.Getenv("THEFEED_ANDROID_APK") == "1" {
		return true
	}
	if exe, err := os.Executable(); err == nil {
		if strings.Contains(exe, "com.thefeed.android") {
			return true
		}
	}
	return false
}

// androidAPKTemplate returns the asset name for the user-facing APK
// (not the raw client binary) at version "{V}". Universal builds —
// flagged at startup by mobile.NewAndroidUniversalServer setting
// THEFEED_ANDROID_UNIVERSAL — stay on the universal asset across
// updates so the user doesn't silently get downgraded to a per-ABI
// split they didn't ask for.
func androidAPKTemplate() string {
	if os.Getenv("THEFEED_ANDROID_UNIVERSAL") == "1" {
		return "thefeed-android-{V}-universal.apk"
	}
	var abi string
	switch runtime.GOARCH {
	case "arm":
		abi = "armeabi-v7a"
	case "amd64":
		abi = "x86_64"
	case "386":
		abi = "x86"
	default:
		abi = "arm64-v8a"
	}
	return "thefeed-android-{V}-" + abi + ".apk"
}

// isMacApp returns true when this binary is running inside the macOS
// .app bundle shipped via the DMG. The Cocoa launcher (mac/Thefeed.swift)
// spawns thefeed-client from Contents/MacOS/, so the executable path
// always contains ".app/Contents/MacOS/". Standalone CLI users on
// darwin (downloaded the bare thefeed-client-{V}-darwin-* binary) have
// no such path component and fall through to the build-time template.
func isMacApp() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return isMacAppPath(exe)
}

// isMacAppPath is the path-only half of isMacApp, split out for tests
// so they don't depend on the test binary's actual location.
func isMacAppPath(p string) bool {
	return strings.Contains(p, ".app/Contents/MacOS/")
}

// macAppTemplate returns the asset name for the DMG bundle at version
// "{V}". The .app inside the DMG is universal (lipo'd amd64+arm64), so
// — unlike Android — we don't need to vary by GOARCH.
func macAppTemplate() string {
	return "thefeed-macos-{V}.dmg"
}

// defaultTemplate is the fallback used when AssetTemplate wasn't
// injected by ldflags. Mirrors the matrix in .github/workflows/build.yml.
func defaultTemplate() string {
	switch runtime.GOOS {
	case "android":
		return "thefeed-client-android-" + runtime.GOARCH
	case "windows":
		return "thefeed-client-{V}-windows-" + runtime.GOARCH + ".exe"
	default:
		return "thefeed-client-{V}-" + runtime.GOOS + "-" + runtime.GOARCH
	}
}
