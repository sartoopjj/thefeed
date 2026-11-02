package server

import (
	"context"
	"errors"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func newGHTestRelay(t *testing.T) (*fakeGitHub, *GitHubRelay) {
	t.Helper()
	fk, _ := newFakeGitHub(t)
	r := NewGitHubRelay(GitHubRelayConfig{Enabled: true, Token: "tok", Repo: "owner/repo", MaxBytes: 1 << 20, TTLMinutes: 60}, "feed.example.com", "test-passphrase")
	return fk, r
}

func timed(f func()) time.Duration {
	start := time.Now()
	f()
	return time.Since(start)
}

// The burst goes at once; after it, each blob waits for the next slot; idle
// time refills no more than the burst.
func TestPaceSpendsTheBurstThenWaits(t *testing.T) {
	_, r := newGHTestRelay(t)
	ctx := context.Background()
	if d := timed(func() {
		for i := 0; i < blobBurst; i++ {
			_ = r.pace(ctx)
		}
	}); d > time.Second {
		t.Fatalf("the burst took %s", d)
	}
	// Nearly a slot refilled: the next blob waits for the rest of it.
	r.paceMu.Lock()
	r.paceTokens, r.paceAt = 0.9, time.Now()
	r.paceMu.Unlock()
	if d := timed(func() { _ = r.pace(ctx) }); d < 600*time.Millisecond || d > 2*time.Second {
		t.Fatalf("waited %s for a tenth of a slot (%s)", d, blobInterval/10)
	}
	r.paceMu.Lock()
	r.paceAt = time.Now().Add(-24 * time.Hour)
	r.paceMu.Unlock()
	_ = r.pace(ctx)
	r.paceMu.Lock()
	left := r.paceTokens
	r.paceMu.Unlock()
	if left > blobBurst-1 {
		t.Fatalf("a day idle banked %.0f slots, more than the burst", left+1)
	}
}

// A pause holds every blob, and a cancelled wait gives its slot back.
func TestPacePauseAndCancel(t *testing.T) {
	_, r := newGHTestRelay(t)
	r.pauseBlobs(700 * time.Millisecond)
	if d := timed(func() { _ = r.pace(context.Background()) }); d < 600*time.Millisecond {
		t.Fatalf("a paused pacer let a blob through after %s", d)
	}
	r.pauseBlobs(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.Upload(ctx, []byte("waiting")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("upload during a pause: %v", err)
	}
	r.mu.Lock()
	inFlight := len(r.uploading)
	r.mu.Unlock()
	if len(r.uploadSem) != 0 || inFlight != 0 {
		t.Fatalf("a cancelled wait kept its slot (%d) or its mark (%d)", len(r.uploadSem), inFlight)
	}
}

func TestRateLimitWaitReadsGitHub(t *testing.T) {
	resp := func(code int, h map[string]string) *http.Response {
		rec := httptest.NewRecorder()
		for k, v := range h {
			rec.Header().Set(k, v)
		}
		rec.WriteHeader(code)
		return rec.Result()
	}
	reset := strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10)
	for _, c := range []struct {
		name string
		resp *http.Response
		msg  string
		want time.Duration
		ok   bool
	}{
		{"Retry-After", resp(403, map[string]string{"Retry-After": "120"}), "", 120 * time.Second, true},
		{"429", resp(429, map[string]string{"Retry-After": "5"}), "", 5 * time.Second, true},
		{"primary limit", resp(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}), "", 90 * time.Second, true},
		{"secondary, no header", resp(403, nil), "403 — You have exceeded a secondary rate limit", time.Minute, true},
		{"token denied", resp(403, nil), "403 — Resource not accessible by personal access token", 0, false},
		{"server error", resp(502, map[string]string{"Retry-After": "5"}), "", 0, false},
	} {
		got, ok := rateLimitWait(c.resp, c.msg)
		if ok != c.ok || (ok && (got < c.want-3*time.Second || got > c.want)) {
			t.Errorf("%s: %s %v, want %s %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// GitHub's refusal pauses the uploads after it, for as long as it asked.
func TestRateLimitedBlobPausesUploads(t *testing.T) {
	fk, r := newGHTestRelay(t)
	fk.mu.Lock()
	fk.blobRefuse, fk.blobStatus, fk.blobRetryAfter = 1, http.StatusForbidden, "1"
	fk.mu.Unlock()
	var rl *rateLimitedError
	if err := r.Upload(context.Background(), []byte("first")); !errors.As(err, &rl) {
		t.Fatalf("refused upload: %v", err)
	}
	if d := timed(func() { _ = r.Upload(context.Background(), []byte("second")) }); d < 800*time.Millisecond {
		t.Fatalf("the next upload went after %s, inside GitHub's pause", d)
	}
	if !r.Has(int64(len("second")), crc32Of("second")) {
		t.Fatal("the upload after the pause did not land")
	}
}

// An upload that failed is tried again the next time the file is stored; one
// still running, or done, is not repeated.
func TestStoreAgainRetriesAFailedUpload(t *testing.T) {
	fk, r := newGHTestRelay(t)
	mc := NewMediaCache(MediaCacheConfig{MaxFileBytes: 64 * 1024, TTL: time.Hour, DNSRelayEnabled: true})
	mc.SetGitHubRelay(r)
	body := fakeJPEG(3000)
	blobs := func() int {
		fk.mu.Lock()
		defer fk.mu.Unlock()
		return fk.blobs
	}

	fk.mu.Lock()
	fk.blobRefuse, fk.blobStatus = 1, http.StatusBadGateway
	fk.mu.Unlock()
	mc.Store("post-1", "[IMAGE]", body, "image/jpeg", "a.jpg")
	mc.WaitUploads(5 * time.Second)
	if r.Has(int64(len(body)), crc32Of(string(body))) {
		t.Fatal("the refused upload counts as done")
	}
	mc.Store("post-1", "[IMAGE]", body, "image/jpeg", "a.jpg")
	mc.WaitUploads(5 * time.Second)
	if !r.Has(int64(len(body)), crc32Of(string(body))) || blobs() != 1 {
		t.Fatalf("not retried on the next store (blobs %d)", blobs())
	}
	// Known now: storing it again, under any key, starts no upload at all.
	mc.Store("post-1", "[IMAGE]", body, "image/jpeg", "a.jpg")
	mc.Store("post-2", "[IMAGE]", body, "image/jpeg", "b.jpg")
	if n := mc.uploading.Load(); n != 0 {
		t.Fatalf("%d upload(s) started for a file the relay has", n)
	}
	if blobs() != 1 {
		t.Fatalf("a file the relay has was uploaded again (blobs %d)", blobs())
	}

	// Two stores while the first upload is still running: one upload.
	fk.mu.Lock()
	fk.blobDelay = 300 * time.Millisecond
	fk.mu.Unlock()
	other := fakeJPEG(2000)
	mc.Store("post-3", "[IMAGE]", other, "image/jpeg", "c.jpg")
	time.Sleep(50 * time.Millisecond)
	mc.Store("post-3", "[IMAGE]", other, "image/jpeg", "c.jpg")
	mc.WaitUploads(5 * time.Second)
	if blobs() != 2 {
		t.Fatalf("an upload in flight was started twice (blobs %d)", blobs())
	}
}

func TestWantsRefusesWhatUploadWouldDrop(t *testing.T) {
	_, r := newGHTestRelay(t)
	if r.Wants(2<<20, 1) {
		t.Error("over the relay's size cap")
	}
	if !r.Wants(100, 1) {
		t.Error("an ordinary missing file")
	}
	r.mu.Lock()
	r.retryAfter = time.Now().Add(time.Minute)
	r.mu.Unlock()
	if r.Wants(100, 1) {
		t.Error("during a backoff that would drop it")
	}
}

func crc32Of(s string) uint32 { return crc32.ChecksumIEEE([]byte(s)) }

// Two uploads of the same file at once: one blob.
func TestConcurrentUploadsOfOneFile(t *testing.T) {
	fk, r := newGHTestRelay(t)
	fk.mu.Lock()
	fk.blobDelay = 300 * time.Millisecond
	fk.mu.Unlock()
	body := []byte("same file")
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- r.Upload(context.Background(), body) }()
	}
	<-done
	<-done
	fk.mu.Lock()
	n := fk.blobs
	fk.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d blobs for one file", n)
	}
}
