package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// eagerFake is a minimal Git Data API stand-in that records how blob uploads
// and commits interleave, plus peak upload concurrency.
type eagerFake struct {
	blobs        int32
	blobAttempts int32 // includes failed attempts, unlike blobs
	trees        int32
	commits      int32

	mu       sync.Mutex
	inFlight int
	peak     int

	blobStatus    int    // when non-zero, blob creation fails with this status
	blobMessage   string // error message for a failed blob; defaults to the quota rejection
	treeStatus    int    // when non-zero, tree creation fails with this status
	refEmpty      bool   // when true, getRef reports an empty repository
	folderMissing bool   // when true, the relay's folder 404s (repo recreated)
	repoMissing   bool   // when true, the repo itself 404s (deleted, or token lost access)
}

func (f *eagerFake) enter() {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
	f.mu.Unlock()
}

func (f *eagerFake) leave() {
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}

func (f *eagerFake) peakConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func (f *eagerFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/blobs"):
			f.enter()
			defer f.leave()
			atomic.AddInt32(&f.blobAttempts, 1)
			if f.blobStatus != 0 {
				msg := f.blobMessage
				if msg == "" {
					msg = "Repository is above its size quota."
				}
				w.WriteHeader(f.blobStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": msg})
				return
			}
			n := atomic.AddInt32(&f.blobs, 1)
			time.Sleep(5 * time.Millisecond) // widen the window so overlap is observable
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": fmt.Sprintf("blobsha%d", n)})

		case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/"):
			if f.refEmpty {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message":"Git Repository is empty."}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{"sha": "headsha"}})

		case r.Method == http.MethodPut && strings.Contains(path, "/contents/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"sha": "bootstrapsha"}})

		// Repo metadata: used by repoAccessible and RefreshRepoSize.
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/repos/owner/repo"):
			if f.repoMissing {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"size": 1024})

		// Folder lookup used by verifyRemoteIndex.
		case r.Method == http.MethodGet && strings.Contains(path, "/contents/"):
			if f.folderMissing {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"path": "x", "type": "file"}})

		case r.Method == http.MethodGet && strings.Contains(path, "/git/commits/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]any{"sha": "basetree"}})

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/trees"):
			if f.treeStatus != 0 {
				w.WriteHeader(f.treeStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "Repository is above its size quota."})
				return
			}
			atomic.AddInt32(&f.trees, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "newtree"})

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/commits"):
			atomic.AddInt32(&f.commits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "newcommit"})

		case r.Method == http.MethodPatch && strings.Contains(path, "/git/refs/heads/"):
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

func newEagerRelay(t *testing.T, f *eagerFake) *GitHubRelay {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	prev := githubAPI
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = prev; srv.Close() })

	r := NewGitHubRelay(GitHubRelayConfig{
		Enabled: true, Token: "tok", Repo: "owner/repo", MaxBytes: 8 << 20, TTLMinutes: 60,
	}, "ex.test", "pp")
	if r == nil {
		t.Fatal("relay should activate with a full config")
	}
	return r
}

// Upload must push the blob straight away and keep only its SHA, so the queue
// costs bytes-per-file rather than megabytes-per-file.
func TestRelayUploadsBlobEagerlyAndQueuesOnlySHA(t *testing.T) {
	f := &eagerFake{}
	r := newEagerRelay(t, f)

	body := bytes.Repeat([]byte("x"), 256*1024)
	if err := r.Upload(context.Background(), body); err != nil {
		t.Fatalf("upload: %v", err)
	}

	if got := atomic.LoadInt32(&f.blobs); got != 1 {
		t.Fatalf("blob uploads=%d, want 1 before any commit", got)
	}
	if got := atomic.LoadInt32(&f.commits); got != 0 {
		t.Fatalf("commits=%d, want 0 — the commit is deferred", got)
	}

	r.mu.Lock()
	n := len(r.ready)
	var sha string
	for _, o := range r.ready {
		sha = o.sha
	}
	r.mu.Unlock()

	if n != 1 {
		t.Fatalf("ready=%d, want 1", n)
	}
	if sha == "" {
		t.Error("queued entry should carry the blob SHA returned by GitHub")
	}
}

// The commit step must reference the pre-uploaded SHAs and upload nothing more.
func TestRelayCommitUsesPreUploadedSHAs(t *testing.T) {
	f := &eagerFake{}
	r := newEagerRelay(t, f)

	for i := 0; i < 5; i++ {
		if err := r.Upload(context.Background(), bytes.Repeat([]byte{byte(i)}, 4096)); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	blobsBefore := atomic.LoadInt32(&f.blobs)
	if blobsBefore != 5 {
		t.Fatalf("blob uploads=%d, want 5", blobsBefore)
	}

	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if got := atomic.LoadInt32(&f.blobs); got != blobsBefore {
		t.Errorf("commit uploaded %d extra blob(s); it should only build a tree", got-blobsBefore)
	}
	if got := atomic.LoadInt32(&f.commits); got != 1 {
		t.Errorf("commits=%d, want exactly 1 for the whole batch", got)
	}
	r.mu.Lock()
	ready, known := len(r.ready), len(r.known)
	r.mu.Unlock()
	if ready != 0 {
		t.Errorf("ready=%d after a successful commit, want 0", ready)
	}
	if known != 5 {
		t.Errorf("known=%d, want 5", known)
	}
}

// Blob uploads must overlap, but only a few at a time: GitHub throttles
// clients that fire mutating requests at high concurrency.
func TestRelayUploadConcurrencyIsBounded(t *testing.T) {
	f := &eagerFake{}
	r := newEagerRelay(t, f)

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte(i), byte(i >> 8)}, 1024)
			if err := r.Upload(context.Background(), body); err != nil {
				t.Errorf("upload %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	peak := f.peakConcurrency()
	if peak > uploadConcurrency {
		t.Errorf("peak concurrent blob uploads=%d, exceeds the %d cap", peak, uploadConcurrency)
	}
	if peak < 2 {
		t.Errorf("peak concurrency=%d — uploads should overlap, not run one at a time", peak)
	}
}

// A quota rejection must arm backoff and then stop doing work entirely: no
// encryption, no uploads, nothing retained.
func TestRelayStopsWorkWhileBackedOff(t *testing.T) {
	f := &eagerFake{blobStatus: http.StatusForbidden}
	r := newEagerRelay(t, f)

	err := r.Upload(context.Background(), bytes.Repeat([]byte("a"), 2048))
	if err == nil {
		t.Fatal("expected the quota rejection to surface")
	}

	r.mu.Lock()
	quota, streak, retry := r.quotaExhausted, r.failStreak, r.retryAfter
	r.mu.Unlock()
	if !quota {
		t.Error("quotaExhausted should be set for an 'above its size quota' rejection")
	}
	if streak != 1 {
		t.Errorf("failStreak=%d, want 1", streak)
	}
	if !retry.After(time.Now()) {
		t.Error("retryAfter should be armed into the future")
	}

	// Further uploads must be skipped outright while backed off.
	attempts := atomic.LoadInt32(&f.blobs)
	for i := 0; i < 5; i++ {
		if err := r.Upload(context.Background(), bytes.Repeat([]byte{byte(i)}, 2048)); err != nil {
			t.Fatalf("backed-off upload should be a silent no-op, got: %v", err)
		}
	}
	if got := atomic.LoadInt32(&f.blobs); got != attempts {
		t.Errorf("backoff gate failed: blob attempts went %d → %d", attempts, got)
	}
	r.mu.Lock()
	ready := len(r.ready)
	r.mu.Unlock()
	if ready != 0 {
		t.Errorf("ready=%d while backed off, want 0 — nothing should be retained", ready)
	}
}

// A failed commit re-queues the SHAs (cheap) so the next tick retries, and the
// backoff gate then skips the immediate retry.
func TestRelayRequeuesSHAsOnCommitFailure(t *testing.T) {
	f := &eagerFake{treeStatus: http.StatusForbidden}
	r := newEagerRelay(t, f)

	for i := 0; i < 3; i++ {
		if err := r.Upload(context.Background(), bytes.Repeat([]byte{byte(i)}, 2048)); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("expected the tree rejection to surface")
	}

	r.mu.Lock()
	ready := len(r.ready)
	r.mu.Unlock()
	if ready != 3 {
		t.Fatalf("ready=%d after a failed commit, want the 3 SHAs re-queued", ready)
	}

	trees := atomic.LoadInt32(&f.trees)
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("backoff-gated flush should be a no-op, got: %v", err)
	}
	if got := atomic.LoadInt32(&f.trees); got != trees {
		t.Errorf("backoff gate failed: tree attempts went %d → %d", trees, got)
	}
}

// A transient blob failure (502, timeout) must NOT arm the global backoff:
// while backed off Upload silently drops every file, and relay-only files are
// too big for DNS, so they'd have no fallback at all.
func TestRelayTransientBlobErrorDoesNotArmBackoff(t *testing.T) {
	f := &eagerFake{blobStatus: http.StatusBadGateway, blobMessage: "Server Error"}
	r := newEagerRelay(t, f)

	if err := r.Upload(context.Background(), bytes.Repeat([]byte("a"), 2048)); err == nil {
		t.Fatal("expected the 502 to surface")
	}
	r.mu.Lock()
	quota, streak, retry := r.quotaExhausted, r.failStreak, r.retryAfter
	r.mu.Unlock()
	if quota {
		t.Error("quotaExhausted must stay false for a transient error")
	}
	if streak != 0 || !retry.IsZero() {
		t.Errorf("failStreak=%d retryAfter=%v — a transient error must not arm backoff", streak, retry)
	}

	// The next file must still be attempted rather than silently dropped.
	before := atomic.LoadInt32(&f.blobAttempts)
	_ = r.Upload(context.Background(), bytes.Repeat([]byte("b"), 2048))
	if got := atomic.LoadInt32(&f.blobAttempts); got == before {
		t.Error("later uploads were skipped — they must still be attempted after a transient failure")
	}
}

// If the remote repo is recreated while a flush is in progress, the queued
// SHAs point into a repo that no longer exists. Re-queuing them would fail
// identically forever while Upload's dedup blocked the re-upload, so they must
// be dropped.
func TestRelayDropsQueuedSHAsWhenRemoteResetMidFlush(t *testing.T) {
	f := &eagerFake{}
	r := newEagerRelay(t, f)

	for i := 0; i < 3; i++ {
		if err := r.Upload(context.Background(), bytes.Repeat([]byte{byte(i)}, 2048)); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}

	// The flush now finds an empty repo (bootstrap resets the index) and then
	// fails to build the tree, because the pre-uploaded blobs are gone.
	f.refEmpty = true
	f.treeStatus = http.StatusUnprocessableEntity
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("expected the tree rejection to surface")
	}

	r.mu.Lock()
	ready := len(r.ready)
	r.mu.Unlock()
	if ready != 0 {
		t.Errorf("ready=%d after a mid-flush remote reset, want 0 — the stale SHAs must be dropped", ready)
	}
}

// A SHA that keeps failing to commit (GitHub garbage-collects unreachable
// blobs) must eventually be dropped instead of wedging the queue forever.
func TestRelayDropsSHAsAfterRepeatedCommitFailures(t *testing.T) {
	f := &eagerFake{}
	r := newEagerRelay(t, f)

	if err := r.Upload(context.Background(), bytes.Repeat([]byte("z"), 2048)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	f.treeStatus = http.StatusUnprocessableEntity

	for i := 0; i < maxCommitAttempts; i++ {
		// flushPending with ignoreBackoff so the test isn't gated on wall clock.
		if err := r.flushPending(context.Background(), true); err == nil {
			t.Fatalf("attempt %d: expected the tree rejection to surface", i)
		}
	}

	r.mu.Lock()
	ready := len(r.ready)
	r.mu.Unlock()
	if ready != 0 {
		t.Errorf("ready=%d after %d failed commits, want 0", ready, maxCommitAttempts)
	}
}

// Deleting and recreating the relay repo by hand leaves the local index
// claiming files that no longer exist. Bootstrapping an empty repo must reset
// it, or Upload dedups against phantom files and clients 404 forever.
func TestRelayForgetsKnownAfterRemoteReset(t *testing.T) {
	dir := t.TempDir()
	r := NewGitHubRelay(GitHubRelayConfig{
		Enabled: true, Token: "tok", Repo: "owner/repo", MaxBytes: 1 << 20, TTLMinutes: 60,
		StatePath: dir + "/gh_relay_state.json",
	}, "ex.test", "pp")
	if r == nil {
		t.Fatal("relay should activate")
	}

	r.mu.Lock()
	r.known["ghost-1"] = &ghEntry{size: 10, crc: 1, lastSeen: time.Now()}
	r.known["ghost-2"] = &ghEntry{size: 20, crc: 2, lastSeen: time.Now()}
	r.mu.Unlock()

	r.forgetKnownAfterRemoteReset()

	r.mu.Lock()
	n := len(r.known)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("known=%d after remote reset, want 0", n)
	}
}

// Recreating the repo *with* a README leaves it non-empty, so the empty-repo
// bootstrap never runs. The index must still be reset — detected by the relay's
// folder being absent — or Upload dedups against files that no longer exist.
func TestRelayResetsIndexWhenFolderMissingOnNonEmptyRepo(t *testing.T) {
	f := &eagerFake{folderMissing: true} // repo exists (README) but our folder doesn't
	r := newEagerRelay(t, f)

	r.mu.Lock()
	r.known["ghost-1"] = &ghEntry{size: 10, crc: 1, lastSeen: time.Now()}
	r.known["ghost-2"] = &ghEntry{size: 20, crc: 2, lastSeen: time.Now()}
	r.mu.Unlock()

	if err := r.verifyRemoteIndex(context.Background()); err != nil {
		t.Fatalf("verifyRemoteIndex: %v", err)
	}
	r.mu.Lock()
	n := len(r.known)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("known=%d after the folder vanished, want 0", n)
	}
}

// A fine-grained token is scoped to a repository ID, so recreating a repo with
// the same name revokes access and every call 404s — indistinguishable from a
// missing folder. The index must be KEPT in that case (we cannot see the repo,
// so we cannot conclude our files are gone), and the error must say why.
func TestRelayKeepsIndexWhenRepoInaccessible(t *testing.T) {
	f := &eagerFake{folderMissing: true, repoMissing: true}
	r := newEagerRelay(t, f)

	r.mu.Lock()
	r.known["real-1"] = &ghEntry{size: 10, crc: 1, lastSeen: time.Now()}
	r.mu.Unlock()

	err := r.verifyRemoteIndex(context.Background())
	if err == nil {
		t.Fatal("an inaccessible repo must surface an error, not be treated as an empty folder")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should point at the token/access cause, got: %v", err)
	}
	r.mu.Lock()
	n := len(r.known)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("known=%d — the index must survive an access failure", n)
	}
}

// Observed in production after recreating the repo: GitHub answers writes with
// 403 "Resource not accessible by personal access token" because a fine-grained
// token is bound to the old repository ID. That needs operator action, so it
// must flag tokenDenied and back off fully rather than retry every cycle.
func TestRelayFlagsTokenAccessDenied(t *testing.T) {
	f := &eagerFake{
		blobStatus:  http.StatusForbidden,
		blobMessage: "Resource not accessible by personal access token",
	}
	r := newEagerRelay(t, f)

	if err := r.Upload(context.Background(), bytes.Repeat([]byte("a"), 2048)); err == nil {
		t.Fatal("expected the access rejection to surface")
	}

	r.mu.Lock()
	denied, quota, retry := r.tokenDenied, r.quotaExhausted, r.retryAfter
	r.mu.Unlock()

	if !denied {
		t.Error("tokenDenied should be set for 'not accessible by personal access token'")
	}
	if quota {
		t.Error("this is an access failure, not a size-quota failure")
	}
	if time.Until(retry) < maxFlushBackoff/2 {
		t.Errorf("retryAfter=%s away — an access failure needs operator action, so back off fully",
			time.Until(retry))
	}
	if st := r.Status(); st == nil || !st.TokenDenied {
		t.Error("Status() must expose tokenDenied so the report can show it")
	}
}

// The converse: a healthy repo whose folder is present must keep its index.
func TestRelayKeepsIndexWhenFolderPresent(t *testing.T) {
	f := &eagerFake{}
	r := newEagerRelay(t, f)

	r.mu.Lock()
	r.known["real-1"] = &ghEntry{size: 10, crc: 1, lastSeen: time.Now()}
	r.mu.Unlock()

	if err := r.verifyRemoteIndex(context.Background()); err != nil {
		t.Fatalf("verifyRemoteIndex: %v", err)
	}
	r.mu.Lock()
	n := len(r.known)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("known=%d, want 1 — a present folder must not reset the index", n)
	}
}

// Relay-only entries (larger than the DNS block cap) have no channel, so a
// sweep that only walked byChannel never expired them: they leaked in
// byKey/byHash and their size stayed in the byte counters forever.
func TestMediaCacheSweepExpiresRelayOnlyEntries(t *testing.T) {
	cache := NewMediaCache(MediaCacheConfig{
		MaxFileBytes:    1024, // anything bigger is relay-only
		TTL:             30 * time.Millisecond,
		DNSRelayEnabled: true,
	})
	// A relay must be attached for oversized files to be accepted at all
	// (they're stored relay-only rather than rejected). SkipGitHub keeps the
	// test off the network.
	cache.SetGitHubRelay(newEagerRelay(t, &eagerFake{}))

	small := bytes.Repeat([]byte("s"), 512)   // fits DNS → gets a channel
	big := bytes.Repeat([]byte("b"), 64*1024) // relay-only → no channel
	if _, err := cache.StoreWithOptions("small", protocol.MediaImage, small, "image/jpeg", "s.jpg", MediaCacheStoreOptions{SkipGitHub: true}); err != nil {
		t.Fatalf("store small: %v", err)
	}
	if _, err := cache.StoreWithOptions("big", protocol.MediaFile, big, "video/mp4", "b.mp4", MediaCacheStoreOptions{SkipGitHub: true}); err != nil {
		t.Fatalf("store big: %v", err)
	}

	if got := cache.Stats().Entries; got != 2 {
		t.Fatalf("entries=%d, want 2 before expiry", got)
	}

	time.Sleep(60 * time.Millisecond)
	if n := cache.Sweep(); n != 2 {
		t.Fatalf("sweep evicted %d, want 2 (the relay-only entry must expire too)", n)
	}

	st := cache.Stats()
	if st.Entries != 0 {
		t.Errorf("entries=%d after sweep, want 0", st.Entries)
	}
	if st.Bytes != 0 {
		t.Errorf("bytes=%d after sweep, want 0 — the counter must come back down", st.Bytes)
	}
	if st.RAMBytes != 0 {
		t.Errorf("ramBytes=%d after sweep, want 0", st.RAMBytes)
	}
}

// RAMBytes must reflect only content actually held in memory: a relay-only
// entry keeps no blocks, so it contributes to Bytes but not to RAMBytes.
func TestMediaCacheRAMBytesExcludesRelayOnly(t *testing.T) {
	cache := NewMediaCache(MediaCacheConfig{
		MaxFileBytes:    1024,
		TTL:             time.Hour,
		DNSRelayEnabled: true,
	})
	cache.SetGitHubRelay(newEagerRelay(t, &eagerFake{}))

	big := bytes.Repeat([]byte("b"), 32*1024)
	if _, err := cache.StoreWithOptions("big", protocol.MediaFile, big, "video/mp4", "b.mp4", MediaCacheStoreOptions{SkipGitHub: true}); err != nil {
		t.Fatalf("store big: %v", err)
	}
	st := cache.Stats()
	if st.Bytes != int64(len(big)) {
		t.Errorf("bytes=%d, want %d (logical size)", st.Bytes, len(big))
	}
	if st.RAMBytes != 0 {
		t.Errorf("ramBytes=%d, want 0 — relay-only entries hold no blocks", st.RAMBytes)
	}

	small := bytes.Repeat([]byte("s"), 300)
	if _, err := cache.StoreWithOptions("small", protocol.MediaImage, small, "image/jpeg", "s.jpg", MediaCacheStoreOptions{SkipGitHub: true}); err != nil {
		t.Fatalf("store small: %v", err)
	}
	if st := cache.Stats(); st.RAMBytes <= 0 {
		t.Errorf("ramBytes=%d, want >0 once a DNS-sized file is cached", st.RAMBytes)
	}
}
