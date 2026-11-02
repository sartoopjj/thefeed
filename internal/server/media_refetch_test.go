package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// fileServer serves one JPEG and counts requests; failing makes it answer 502.
func fileServer(t *testing.T, body []byte) (url string, hits *atomic.Int32, failing *atomic.Bool) {
	t.Helper()
	hits, failing = new(atomic.Int32), new(atomic.Bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/p.jpg", hits, failing
}

func relayedCache(t *testing.T) (*fakeGitHub, *GitHubRelay, *MediaCache) {
	t.Helper()
	fk, r := newGHTestRelay(t)
	mc := NewMediaCache(MediaCacheConfig{MaxFileBytes: 64 * 1024, TTL: time.Hour, DNSRelayEnabled: true})
	mc.SetGitHubRelay(r)
	return fk, r, mc
}

// A post's media whose upload failed is downloaded again on its next fetch,
// and the store uploads it; one the relay has is a plain cache hit.
func TestFailedRelayUploadIsFetchedAgain(t *testing.T) {
	fk, r, mc := relayedCache(t)
	body := fakeJPEG(3000)
	url, hits, _ := fileServer(t, body)
	ctx := context.Background()

	fk.mu.Lock()
	fk.blobRefuse, fk.blobStatus = 1, http.StatusBadGateway
	fk.mu.Unlock()
	if _, ok := downloadHTTPMedia(ctx, mc, protocol.MediaImage, url); !ok {
		t.Fatal("first fetch")
	}
	mc.WaitUploads(5 * time.Second)
	if r.Has(int64(len(body)), crc32Of(string(body))) {
		t.Fatal("the refused upload counts as done")
	}

	if _, ok := downloadHTTPMedia(ctx, mc, protocol.MediaImage, url); !ok || hits.Load() != 2 {
		t.Fatalf("not fetched again for the relay (requests %d)", hits.Load())
	}
	mc.WaitUploads(5 * time.Second)
	if !r.Has(int64(len(body)), crc32Of(string(body))) {
		t.Fatal("fetched again but not uploaded")
	}

	if _, ok := downloadHTTPMedia(ctx, mc, protocol.MediaImage, url); !ok || hits.Load() != 2 {
		t.Fatalf("a file the relay has was fetched again (requests %d)", hits.Load())
	}
}

// Fetching again for the relay must never cost the post its media: if that
// fetch fails, the cached copy is what the post keeps.
func TestRefetchFailureKeepsTheCachedMedia(t *testing.T) {
	fk, _, mc := relayedCache(t)
	body := fakeJPEG(3000)
	url, hits, failing := fileServer(t, body)
	ctx := context.Background()
	fk.mu.Lock()
	fk.blobRefuse, fk.blobStatus = 1, http.StatusBadGateway
	fk.mu.Unlock()
	first, _ := downloadHTTPMedia(ctx, mc, protocol.MediaImage, url)
	mc.WaitUploads(5 * time.Second)

	failing.Store(true)
	got, ok := downloadHTTPMedia(ctx, mc, protocol.MediaImage, url)
	if hits.Load() != 2 {
		t.Fatalf("expected a second fetch for the relay, got %d requests", hits.Load())
	}
	if !ok || got.CRC32 != first.CRC32 || got.Channel != first.Channel {
		t.Fatalf("the post lost its media: %+v ok=%v", got, ok)
	}
}

// Refetch asks only when it could help.
func TestRefetchOnlyWhenTheRelayCanTakeIt(t *testing.T) {
	body := fakeJPEG(2000)

	// No relay at all: DNS only, nothing to fetch again for.
	plain := NewMediaCache(MediaCacheConfig{MaxFileBytes: 64 * 1024, TTL: time.Hour, DNSRelayEnabled: true})
	plain.Store("k", "[IMAGE]", body, "image/jpeg", "a.jpg")
	if plain.Refetch("k") {
		t.Error("without a relay")
	}

	fk, r, mc := relayedCache(t)
	// Stored for DNS only (as avatars are): never fetched again for GitHub.
	mc.StoreWithOptions("avatar", "[PROFILE]", fakeJPEG(1500), "image/jpeg", "a.jpg", MediaCacheStoreOptions{SkipGitHub: true})
	if mc.Refetch("avatar") {
		t.Error("a DNS-only entry")
	}
	if mc.Refetch("missing") {
		t.Error("a key not in the cache")
	}

	fk.mu.Lock()
	fk.blobRefuse, fk.blobStatus = 1, http.StatusBadGateway
	fk.mu.Unlock()
	mc.Store("k", "[IMAGE]", body, "image/jpeg", "a.jpg")
	mc.WaitUploads(5 * time.Second)
	if !mc.Refetch("k") {
		t.Fatal("a failed upload is not asked for again")
	}
	r.pauseBlobs(time.Minute)
	if mc.Refetch("k") {
		t.Error("while GitHub has asked to pause")
	}
	r.paceMu.Lock()
	r.pauseUntil = time.Time{}
	r.paceMu.Unlock()
	r.mu.Lock()
	r.retryAfter = time.Now().Add(time.Minute)
	r.mu.Unlock()
	if mc.Refetch("k") {
		t.Error("during a backoff")
	}
}

// A post whose bytes match a DNS-only entry (a channel posting its own avatar)
// shares that entry, which must then go to the relay as well.
func TestSharedEntryBecomesRelayed(t *testing.T) {
	fk, r, mc := relayedCache(t)
	pic := fakeJPEG(1800)
	mc.StoreWithOptions("profile-pic:x", "[PROFILE]", pic, "image/jpeg", "x.jpg", MediaCacheStoreOptions{SkipGitHub: true})
	mc.WaitUploads(5 * time.Second)
	if r.Has(int64(len(pic)), crc32Of(string(pic))) {
		t.Fatal("a DNS-only entry went to the relay")
	}
	// The post's upload fails: it must still be asked for again.
	fk.mu.Lock()
	fk.blobRefuse, fk.blobStatus = 1, http.StatusBadGateway
	fk.mu.Unlock()
	mc.Store("post-1", "[IMAGE]", pic, "image/jpeg", "p.jpg")
	mc.WaitUploads(5 * time.Second)
	if !mc.Refetch("post-1") {
		t.Fatal("the post sharing a DNS-only entry is never fetched again for the relay")
	}
	mc.Store("post-1", "[IMAGE]", pic, "image/jpeg", "p.jpg")
	mc.WaitUploads(5 * time.Second)
	if !r.Has(int64(len(pic)), crc32Of(string(pic))) {
		t.Fatal("the post sharing it was not relayed")
	}
}

// Uploads waiting their turn hold no more than maxUploadBytes in copies; a
// file left out is neither fetched again for the relay nor lost to it, and
// every byte is given back once the uploads end, failed ones too.
func TestUploadQueueIsCapped(t *testing.T) {
	fk, r, mc := relayedCache(t)
	body := fakeJPEG(2000)
	mc.uploadBytes.Store(maxUploadBytes - 100)
	mc.Store("k", "[IMAGE]", body, "image/jpeg", "a.jpg")
	if mc.uploading.Load() != 0 {
		t.Fatal("an upload was queued past the cap")
	}
	if mc.Refetch("k") {
		t.Fatal("fetched again for a full queue")
	}

	mc.uploadBytes.Store(0)
	if !mc.Refetch("k") {
		t.Fatal("the file left out is never asked for again")
	}
	fk.mu.Lock()
	fk.blobRefuse, fk.blobStatus = 1, http.StatusBadGateway
	fk.mu.Unlock()
	mc.Store("k", "[IMAGE]", body, "image/jpeg", "a.jpg")
	mc.WaitUploads(5 * time.Second)
	if n := mc.uploadBytes.Load(); n != 0 {
		t.Fatalf("a failed upload kept %d bytes", n)
	}
	mc.Store("k", "[IMAGE]", body, "image/jpeg", "a.jpg")
	mc.WaitUploads(5 * time.Second)
	if !r.Has(int64(len(body)), crc32Of(string(body))) {
		t.Fatal("not uploaded once there was room")
	}
	if n := mc.uploadBytes.Load(); n != 0 {
		t.Fatalf("an upload kept %d bytes", n)
	}
	if !mc.uploadRoom(2 * maxUploadBytes) {
		t.Fatal("a file bigger than the cap can never be uploaded")
	}
}
