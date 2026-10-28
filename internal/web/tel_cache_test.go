package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

func msg(id int, text string) tgbridge.Message {
	return tgbridge.Message{ID: id, Date: 1000 + id, Text: text}
}

func peer(id int64, title string) tgbridge.Peer {
	return tgbridge.Peer{Kind: "channel", ID: id, Title: title}
}

func TestCacheSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	p := peer(7, "Channel")
	c := loadTelCache(dir)
	c.PutDialogs([]tgbridge.Dialog{{Peer: p, Unread: 3, Preview: "latest"}})
	c.MergeThread(telCacheKey(p), []tgbridge.Message{msg(1, "one"), msg(2, "two")})

	// A new process, same data dir.
	again := loadTelCache(dir)
	ds := again.Dialogs()
	if len(ds) != 1 || ds[0].Unread != 3 || ds[0].Preview != "latest" {
		t.Fatalf("dialogs did not survive: %+v", ds)
	}
	if got := again.Thread(telCacheKey(p)); len(got) != 2 || got[1].Text != "two" {
		t.Fatalf("messages did not survive: %+v", got)
	}
}

func TestCacheIsSealedOnDisk(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	c.PutDialogs([]tgbridge.Dialog{{Peer: peer(1, "Secret Channel"), Preview: "secret text"}})

	raw, err := os.ReadFile(telCachePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"Secret Channel", "secret text"} {
		if containsBytes(raw, needle) {
			t.Fatalf("%q is readable in the cache file", needle)
		}
	}
}

func containsBytes(hay []byte, needle string) bool {
	n := []byte(needle)
	for i := 0; i+len(n) <= len(hay); i++ {
		if string(hay[i:i+len(n)]) == needle {
			return true
		}
	}
	return false
}

// The opposite rule to the session: a cache that cannot be read is refilled,
// not locked. Being stuck would cost the user everything; refilling costs
// queries.
func TestUnreadableCacheStartsEmptyInsteadOfLocking(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	c.PutDialogs([]tgbridge.Dialog{{Peer: peer(1, "x")}})

	if err := os.WriteFile(telCachePath(dir), []byte("not a sealed blob at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	again := loadTelCache(dir)
	if len(again.Dialogs()) != 0 {
		t.Fatal("a corrupt cache must read as empty")
	}
	// And it must still be usable afterwards, not wedged.
	again.PutDialogs([]tgbridge.Dialog{{Peer: peer(2, "y")}})
	if len(loadTelCache(dir).Dialogs()) != 1 {
		t.Fatal("the cache did not recover after being corrupted")
	}
}

// Losing the key must not wedge the app. The cache goes; the session is the
// thing that gets protected instead (see tel_crypto_test.go).
func TestCacheWithoutAKeyIsEmptyNotFatal(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	c.PutDialogs([]tgbridge.Dialog{{Peer: peer(1, "x")}})
	if err := os.RemoveAll(telCryptoDir(dir)); err != nil {
		t.Fatal(err)
	}
	// No panic, no error, just nothing cached.
	if got := loadTelCache(dir).Dialogs(); len(got) != 0 {
		t.Fatalf("expected an empty cache, got %+v", got)
	}
}

// A newer copy of a message must replace the old one rather than appear twice:
// an edit keeps its id.
func TestMergeReplacesEditedMessages(t *testing.T) {
	dir := t.TempDir()
	p := peer(3, "c")
	c := loadTelCache(dir)
	c.MergeThread(telCacheKey(p), []tgbridge.Message{msg(1, "before"), msg(2, "two")})
	c.MergeThread(telCacheKey(p), []tgbridge.Message{msg(1, "after")})

	got := c.Thread(telCacheKey(p))
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2: an edit must not duplicate", len(got))
	}
	if got[0].ID != 1 || got[0].Text != "after" {
		t.Fatalf("edit did not replace the original: %+v", got[0])
	}
	if got[1].ID != 2 {
		t.Fatalf("messages out of order: %+v", got)
	}
}

func TestThreadIsBounded(t *testing.T) {
	dir := t.TempDir()
	p := peer(4, "c")
	c := loadTelCache(dir)
	var many []tgbridge.Message
	for i := 1; i <= telCacheMaxPerThread*2; i++ {
		many = append(many, msg(i, "m"))
	}
	c.MergeThread(telCacheKey(p), many)

	got := c.Thread(telCacheKey(p))
	if len(got) != telCacheMaxPerThread {
		t.Fatalf("kept %d messages, want the %d cap", len(got), telCacheMaxPerThread)
	}
	// The NEWEST must be the ones kept.
	if got[len(got)-1].ID != telCacheMaxPerThread*2 {
		t.Fatalf("kept the wrong end: last id %d", got[len(got)-1].ID)
	}
}

func TestThreadCountIsBounded(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	for i := 1; i <= telCacheMaxThreads+20; i++ {
		c.MergeThread(telCacheKey(peer(int64(i), "c")), []tgbridge.Message{msg(1, "m")})
	}
	c.mu.Lock()
	n := len(c.data.Threads)
	_, oldestKept := c.data.Threads[telCacheKey(peer(1, "c"))]
	_, newestKept := c.data.Threads[telCacheKey(peer(int64(telCacheMaxThreads+20), "c"))]
	c.mu.Unlock()

	if n > telCacheMaxThreads {
		t.Fatalf("holding %d threads, over the %d cap", n, telCacheMaxThreads)
	}
	if oldestKept {
		t.Error("the least recently touched thread should have been evicted first")
	}
	if !newestKept {
		t.Error("the most recent thread was evicted")
	}
}

// Signing out must not leave one account's conversations for the next.
func TestForgetClearsEverything(t *testing.T) {
	dir := t.TempDir()
	p := peer(9, "c")
	c := loadTelCache(dir)
	c.PutDialogs([]tgbridge.Dialog{{Peer: p}})
	c.MergeThread(telCacheKey(p), []tgbridge.Message{msg(1, "private")})

	c.Forget()
	if len(c.Dialogs()) != 0 || len(c.Thread(telCacheKey(p))) != 0 {
		t.Fatal("Forget left data in memory")
	}
	if _, err := os.Stat(telCachePath(dir)); !os.IsNotExist(err) {
		t.Fatal("Forget left the cache file on disk")
	}
	if got := loadTelCache(dir).Dialogs(); len(got) != 0 {
		t.Fatalf("a reload after Forget still returned %+v", got)
	}
}

// The list must come back without waiting for the network when the cache is
// warm: fetching it costs about a minute on a degraded link, and the rows are
// almost always the ones already held.
func TestDialogsAnswerFromCacheWhenWarm(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	s.tel = newTelHub(s, dir)
	p := tgbridge.Peer{Kind: "channel", ID: 3, Title: "Chan"}
	s.tel.store().PutDialogs([]tgbridge.Dialog{{Peer: p, Unread: 1, Preview: "from cache"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tel/dialogs", nil)
	s.handleTelDialogs(rec, req)

	// No bridge exists here at all, so anything other than a cached answer
	// would be an error.
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK     bool `json:"ok"`
		Cached bool `json:"cached"`
		Page   struct {
			Dialogs []tgbridge.Dialog `json:"dialogs"`
		} `json:"page"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || !got.Cached {
		t.Fatalf("want a cached answer, got ok=%v cached=%v", got.OK, got.Cached)
	}
	if len(got.Page.Dialogs) != 1 || got.Page.Dialogs[0].Preview != "from cache" {
		t.Fatalf("wrong rows: %+v", got.Page.Dialogs)
	}
}

// Clearing storage frees the conversations but must never sign the user out:
// the session is not a cache, and re-logging in is not a cost anyone chose.
func TestClearingTheCacheKeepsTheSession(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	s.tel = newTelHub(s, dir)
	if err := s.tel.sessionFile().StoreSession(context.Background(), []byte(`{"auth":"keep me"}`)); err != nil {
		t.Fatal(err)
	}
	s.tel.store().PutDialogs([]tgbridge.Dialog{{Peer: tgbridge.Peer{Kind: "channel", ID: 1}}})

	s.tel.store().Forget()

	if got := loadTelCache(s.tel.dir).Dialogs(); len(got) != 0 {
		t.Fatalf("the cache survived clearing: %+v", got)
	}
	got, err := s.tel.sessionFile().LoadSession(context.Background())
	if err != nil {
		t.Fatalf("clearing the cache lost the session: %v", err)
	}
	if string(got) != `{"auth":"keep me"}` {
		t.Fatalf("session changed: %q", got)
	}
}
