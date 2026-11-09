package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

var (
	telTestChannel = tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 9, AccessHash: 1, Title: "News"}
	telTestGroup   = tgbridge.Peer{Kind: tgbridge.PeerGroup, ID: 5, Title: "Basic", Members: 42}
)

// A count Telegram sent with the list is an answer, at no cost; one read on
// demand holds for a day; past that it has to be read again.
func TestTelMembersCache(t *testing.T) {
	c := loadTelCache(t.TempDir())
	now := time.Unix(1_800_000_000, 0)
	c.PutDialogs([]tgbridge.Dialog{{Peer: telTestGroup}, {Peer: telTestChannel}})

	if n, ok := c.Members(telCacheKey(telTestGroup), now); !ok || n != 42 {
		t.Fatalf("free count: %d %v", n, ok)
	}
	if _, ok := c.Members(telCacheKey(telTestChannel), now); ok {
		t.Fatal("a channel with no count claims one")
	}
	c.SetMembers(telCacheKey(telTestChannel), 12345, now)
	if n, ok := c.Members(telCacheKey(telTestChannel), now.Add(23*time.Hour)); !ok || n != 12345 {
		t.Fatalf("within the day: %d %v", n, ok)
	}
	if _, ok := c.Members(telCacheKey(telTestChannel), now.Add(25*time.Hour)); ok {
		t.Fatal("a count older than a day is still served as fresh")
	}
	// A hidden count is an answer too: asked once a day, not every open.
	c.SetMembers("channel:77", 0, now)
	if n, ok := c.Members("channel:77", now.Add(time.Hour)); !ok || n != 0 {
		t.Fatalf("hidden count: %d %v", n, ok)
	}
	// Writing a new count drops the expired ones.
	c.SetMembers("channel:78", 5, now.Add(48*time.Hour))
	if _, ok := c.data.Members["channel:77"]; ok {
		t.Fatal("an expired count was kept")
	}
	// It survives a restart (read back at the time it was written).
	again := loadTelCache(c.path[:len(c.path)-len("/cache.bin")])
	if n, ok := again.Members("channel:78", now.Add(48*time.Hour)); !ok || n != 5 {
		t.Fatalf("after restart: %d %v", n, ok)
	}
}

// A count that came with a row is only as fresh as that row. When a later
// entity comes without one, the old count must not be kept: kept, it was
// served for ever and the day's read never happened, so a channel's count
// froze at whatever Telegram once sent. Unknown instead, the next open reads
// it -- at most once a day.
func TestTelMembersDoNotFreeze(t *testing.T) {
	c := loadTelCache(t.TempDir())
	now := time.Unix(1_800_000_000, 0)
	key := telCacheKey(telTestChannel)
	ch := telTestChannel
	ch.Members = 500
	c.PutDialogs([]tgbridge.Dialog{{Peer: ch, TopMsg: 1}})
	if n, ok := c.Members(key, now); !ok || n != 500 {
		t.Fatalf("the row's count: %d %v", n, ok)
	}
	bare := telTestChannel // same channel, no count
	c.MergeDialogs([]tgbridge.Dialog{{Peer: bare, TopMsg: 2}})
	if n, ok := c.Members(key, now.Add(90*24*time.Hour)); ok {
		t.Fatalf("a list read without a count kept the old one for 90 days: %d", n)
	}
	// Same through the update stream.
	c.MergeDialogs([]tgbridge.Dialog{{Peer: ch, TopMsg: 2}})
	c.ApplyNew([]tgbridge.PeerMessages{{Peer: bare, Messages: []tgbridge.Message{{ID: 3, Text: "hi"}}}})
	if n, ok := c.Members(key, now); ok {
		t.Fatalf("a message without a count kept the old one: %d", n)
	}
	// Read on the next open, it holds for the day, then is read again.
	c.SetMembers(key, 700, now)
	if n, ok := c.Members(key, now.Add(23*time.Hour)); !ok || n != 700 {
		t.Fatalf("today's read: %d %v", n, ok)
	}
	if _, ok := c.Members(key, now.Add(25*time.Hour)); ok {
		t.Fatal("yesterday's read is still served")
	}
	// A row that comes with a count is current, and wins.
	ch.Members = 501
	c.MergeDialogs([]tgbridge.Dialog{{Peer: ch, TopMsg: 4}})
	if n, ok := c.Members(key, now.Add(25*time.Hour)); !ok || n != 501 {
		t.Fatalf("a new count from the list: %d %v", n, ok)
	}
}

// The page's step asks for a count only where one costs a call and is
// missing: never for people, bots, basic groups or direct messages, never
// twice in a day. Without a link, a call that is needed fails visibly.
func TestTelMembersFetch(t *testing.T) {
	s := newTelTestServer(t)
	direct := tgbridge.Peer{Kind: tgbridge.PeerGroup, ID: 31, AccessHash: 4, Direct: true}
	s.tel.store().PutDialogs([]tgbridge.Dialog{{Peer: telTestGroup}, {Peer: telTestChannel}, {Peer: direct}})
	post := func(peer string) (ok, known bool, n int, raw string) {
		rec := httptest.NewRecorder()
		s.handleTelMembers(rec, telJSONReq("/api/tel/members", `{"peer":`+peer+`}`))
		var resp struct {
			OK      bool `json:"ok"`
			Known   bool `json:"known"`
			Members int  `json:"members"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.OK, resp.Known, resp.Members, rec.Body.String()
	}
	// There is no link in this test server: an answer that needed one fails.
	for name, peer := range map[string]string{
		"person":      `{"kind":"user","id":"1","accessHash":"1"}`,
		"bot":         `{"kind":"bot","id":"2","accessHash":"1"}`,
		"basic group": `{"kind":"group","id":"5","accessHash":"0"}`,
		// Named as the page names it; the cached row says it is direct.
		"direct chat": `{"kind":"group","id":"31","accessHash":"4"}`,
	} {
		// ok is the proof: with no link, any call to Telegram fails.
		if ok, _, _, raw := post(peer); !ok {
			t.Errorf("%s: went to Telegram for a count: %s", name, raw)
		}
	}
	// The basic group's count came with its row, free.
	if _, known, n, raw := post(`{"kind":"group","id":"5","accessHash":"0"}`); !known || n != 42 {
		t.Errorf("basic group lost its free count: %s", raw)
	}
	if _, known, _, raw := post(`{"kind":"user","id":"1","accessHash":"1"}`); known {
		t.Errorf("a person got a count: %s", raw)
	}
	if ok, _, _, raw := post(`{"kind":"channel","id":"9","accessHash":"1"}`); ok {
		t.Fatalf("a missing count was answered without asking Telegram: %s", raw)
	}
	s.tel.store().SetMembers(telCacheKey(telTestChannel), 10, time.Now())
	if ok, known, n, raw := post(`{"kind":"channel","id":"9","accessHash":"1"}`); !ok || !known || n != 10 {
		t.Fatalf("a count read today was asked for again: %s", raw)
	}
}

// The page reads the count from here: the cache only, never the link.
func TestTelMembersEndpoint(t *testing.T) {
	s := newTelTestServer(t)
	s.tel.store().SetMembers(telCacheKey(telTestChannel), 12345, time.Now())
	rec := httptest.NewRecorder()
	s.handleTelMembers(rec, httptest.NewRequest(http.MethodGet, "/api/tel/members?kind=channel&id=9&accessHash=1", nil))
	var resp struct {
		OK      bool `json:"ok"`
		Members int  `json:"members"`
		Known   bool `json:"known"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.OK || !resp.Known || resp.Members != 12345 {
		t.Fatalf("got %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleTelMembers(rec, httptest.NewRequest(http.MethodGet, "/api/tel/members?kind=channel&id=10&accessHash=1", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.OK || resp.Known {
		t.Fatalf("unknown channel: %s", rec.Body.String())
	}
}
