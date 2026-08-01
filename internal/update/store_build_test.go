package update

import (
	"runtime"
	"testing"
)

// Both branches, from any host: the old test only asserted the iOS case when
// GOOS was already ios, so removing the check left the suite green.
func TestStoreBuildFor(t *testing.T) {
	cases := []struct {
		goos, env string
		want      bool
	}{
		{"ios", "", true},      // App Store / TestFlight
		{"ios", "1", true},     // both signals
		{"android", "1", true}, // Play build
		{"android", "", false}, // GitHub APK keeps its updater
		{"darwin", "", false},  // the .dmg
		{"windows", "", false},
		{"linux", "", false},
		{"darwin", "0", false}, // only "1" enables it
	}
	for _, c := range cases {
		if got := storeBuildFor(c.goos, c.env); got != c.want {
			t.Errorf("storeBuildFor(%q, %q) = %v, want %v", c.goos, c.env, got, c.want)
		}
	}
}

// StoreBuild must read the real env, not a cached value.
func TestStoreBuildReadsEnv(t *testing.T) {
	t.Setenv("THEFEED_STORE_BUILD", "")
	if want := runtime.GOOS == "ios"; StoreBuild() != want {
		t.Errorf("StoreBuild() = %v with no env, want %v on GOOS=%s", StoreBuild(), want, runtime.GOOS)
	}
	t.Setenv("THEFEED_STORE_BUILD", "1")
	if !StoreBuild() {
		t.Error("StoreBuild() = false with THEFEED_STORE_BUILD=1")
	}
}
