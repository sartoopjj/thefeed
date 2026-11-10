package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"

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
	c.SetChannelInfo(telCacheKey(telTestChannel), tgbridge.ChannelInfo{Members: 12345}, now)
	if n, ok := c.Members(telCacheKey(telTestChannel), now.Add(23*time.Hour)); !ok || n != 12345 {
		t.Fatalf("within the day: %d %v", n, ok)
	}
	if _, ok := c.Members(telCacheKey(telTestChannel), now.Add(25*time.Hour)); ok {
		t.Fatal("a count older than a day is still served as fresh")
	}
	// A hidden count is an answer too: asked once a day, not every open.
	c.SetChannelInfo("channel:77", tgbridge.ChannelInfo{Members: 0}, now)
	if n, ok := c.Members("channel:77", now.Add(time.Hour)); !ok || n != 0 {
		t.Fatalf("hidden count: %d %v", n, ok)
	}
	// Writing a new read drops expired counts kept from before descriptions
	// were (nothing to show from them); a read with its description stays.
	c.data.Members["channel:76"] = telMemberCount{N: 3, At: now.Unix()}
	c.SetChannelInfo("channel:78", tgbridge.ChannelInfo{Members: 5}, now.Add(48*time.Hour))
	if _, ok := c.data.Members["channel:76"]; ok {
		t.Fatal("an expired count with no description was kept")
	}
	if _, ok := c.data.Members["channel:77"]; !ok {
		t.Fatal("an old read was dropped with its description")
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
	c.SetChannelInfo(key, tgbridge.ChannelInfo{Members: 700}, now)
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
	s.tel.store().SetChannelInfo(telCacheKey(telTestChannel), tgbridge.ChannelInfo{Members: 10}, time.Now())
	if ok, known, n, raw := post(`{"kind":"channel","id":"9","accessHash":"1"}`); !ok || !known || n != 10 {
		t.Fatalf("a count read today was asked for again: %s", raw)
	}
}

// The page reads the count from here: the cache only, never the link.
func TestTelMembersEndpoint(t *testing.T) {
	s := newTelTestServer(t)
	s.tel.store().SetChannelInfo(telCacheKey(telTestChannel), tgbridge.ChannelInfo{Members: 12345}, time.Now())
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

// Telegram refusing a channel's full info (private, banned, gone) is an
// answer: held for the day like a hidden count, or every open asked again.
// A link failure is not: nothing is learnt, the next open tries again.
func TestTelMembersRefusalIsAnAnswer(t *testing.T) {
	if !telMembersRefused(tgerr.New(400, "CHANNEL_PRIVATE")) {
		t.Fatal("Telegram's refusal was not taken as an answer")
	}
	if telMembersRefused(errors.New("dial tcp: i/o timeout")) || telMembersRefused(context.DeadlineExceeded) {
		t.Fatal("a link failure was taken as Telegram's answer")
	}
	if telMembersRefused(tgerr.New(420, "FLOOD_WAIT_30")) {
		t.Fatal("a flood wait is not an answer about the channel")
	}
}

// A channel's description comes with the same full-info read as its member
// count. It holds for a day like the count; past that it is read again, but
// the old one is still there to show if the new read fails.
func TestTelChannelAboutCache(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	now := time.Unix(1_800_000_000, 0)
	key := telCacheKey(telTestChannel)
	if _, read, _ := c.About(key, now); read {
		t.Fatal("a description never read claims to be")
	}
	// A count kept before descriptions were: the description was never read.
	c.data.Members = map[string]telMemberCount{}
	c.data.Members[key] = telMemberCount{N: 9, At: now.Unix()}
	if _, read, _ := c.About(key, now); read {
		t.Fatal("a count kept from before reads as an (empty) description")
	}
	c.SetChannelInfo(key, tgbridge.ChannelInfo{Members: 9, About: "Hourly news"}, now)
	if a, read, fresh := c.About(key, now.Add(23*time.Hour)); a != "Hourly news" || !read || !fresh {
		t.Fatalf("within the day: %q %v %v", a, read, fresh)
	}
	a, read, fresh := c.About(key, now.Add(25*time.Hour))
	if a != "Hourly news" || !read || fresh {
		t.Fatalf("a day on: %q %v %v, want the old text, not fresh", a, read, fresh)
	}
	// Telegram refuses it now (left, went private): no count, the old text
	// stays, and it is not asked again today.
	later := now.Add(30 * time.Hour)
	c.SetInfoRefused(key, later)
	if a, read, fresh := c.About(key, later.Add(time.Hour)); a != "Hourly news" || !read || !fresh {
		t.Fatalf("after a refusal: %q %v %v", a, read, fresh)
	}
	if n, ok := c.Members(key, later); !ok || n != 0 {
		t.Fatalf("refusal count: %d %v", n, ok)
	}
	// An empty description read is an answer too.
	c.SetChannelInfo("channel:5", tgbridge.ChannelInfo{}, now)
	if a, read, fresh := c.About("channel:5", now); a != "" || !read || !fresh {
		t.Fatalf("empty description: %q %v %v", a, read, fresh)
	}
	// It survives a restart.
	if a, _, _ := loadTelCache(dir).About(key, later); a != "Hourly news" {
		t.Fatalf("after restart: %q", a)
	}
	// Bounded: past telInfoMax the oldest reads go, never the one just made.
	for i := 0; i < telInfoMax+20; i++ {
		c.SetChannelInfo(fmt.Sprintf("channel:%d", 1000+i), tgbridge.ChannelInfo{About: "x"}, now.Add(time.Duration(i)*time.Second))
	}
	if len(c.data.Members) != telInfoMax {
		t.Fatalf("kept %d reads, bound is %d", len(c.data.Members), telInfoMax)
	}
	if _, read, _ := c.About(fmt.Sprintf("channel:%d", 1000+telInfoMax+19), now); !read {
		t.Fatal("the newest read was evicted")
	}
	if _, read, _ := c.About("channel:1000", now); read {
		t.Fatal("the oldest read was kept past the bound")
	}
}

// Clearing the Telegram cache (or every cache) drops member counts and
// descriptions with the conversations: nothing about a channel survives it.
func TestTelClearCacheDropsChannelInfo(t *testing.T) {
	for _, which := range []string{"one", "all"} {
		s := newTelTestServer(t)
		key := telCacheKey(telTestChannel)
		s.tel.store().SetChannelInfo(key, tgbridge.ChannelInfo{Members: 7, About: "about"}, time.Now())
		rec := httptest.NewRecorder()
		if which == "one" {
			s.handleCacheClearOne(rec, telJSONReq("/api/cache/clear-one", `{"which":"telegram"}`))
		} else {
			s.handleClearCache(rec, httptest.NewRequest(http.MethodPost, "/api/cache/clear", nil))
		}
		if rec.Code != 200 {
			t.Fatalf("%s: clear answered %d %s", which, rec.Code, rec.Body.String())
		}
		if _, ok := s.tel.store().Members(key, time.Now()); ok {
			t.Errorf("%s: the member count survived clearing the cache", which)
		}
		if _, read, _ := s.tel.store().About(key, time.Now()); read {
			t.Errorf("%s: the description survived clearing the cache", which)
		}
		if _, read, _ := loadTelCache(s.tel.dir).About(key, time.Now()); read {
			t.Errorf("%s: the description is still on disk", which)
		}
	}
}

// The info sheet asks for a description only when today's read has none:
// one call, shared with the member count. A failed read keeps what was
// there; with nothing there, it says so and is tried again next time.
func TestTelChannelAboutFetch(t *testing.T) {
	s := newTelTestServer(t)
	free := telTestChannel
	free.Members = 500 // the count came free with the row
	s.tel.store().PutDialogs([]tgbridge.Dialog{{Peer: free}})
	key := telCacheKey(telTestChannel)
	calls := 0
	var answer tgbridge.ChannelInfo
	var fail error
	orig := telReadChannelInfo
	defer func() { telReadChannelInfo = orig }()
	telReadChannelInfo = func(ctx context.Context, h *telHub, p tgbridge.Peer) (tgbridge.ChannelInfo, error) {
		calls++
		return answer, fail
	}
	type resp struct {
		OK         bool   `json:"ok"`
		Members    int    `json:"members"`
		Known      bool   `json:"known"`
		About      string `json:"about"`
		AboutRead  bool   `json:"aboutRead"`
		AboutFresh bool   `json:"aboutFresh"`
	}
	post := func(body string) resp {
		rec := httptest.NewRecorder()
		s.handleTelMembers(rec, telJSONReq("/api/tel/members", body))
		var r resp
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return r
	}
	ch := `{"kind":"channel","id":"9","accessHash":"1"}`
	// The chat's own step: the count is free, so no call.
	if r := post(`{"peer":` + ch + `}`); !r.OK || r.Members != 500 || calls != 0 {
		t.Fatalf("count from the row: %+v, %d calls", r, calls)
	}
	// The info sheet: the description was never read, so one call.
	answer = tgbridge.ChannelInfo{Members: 501, About: "Hourly news"}
	if r := post(`{"peer":` + ch + `,"about":true}`); !r.OK || r.About != "Hourly news" || !r.AboutFresh || calls != 1 {
		t.Fatalf("first open of the info: %+v, %d calls", r, calls)
	}
	// Again today: from the cache.
	if r := post(`{"peer":` + ch + `,"about":true}`); r.About != "Hourly news" || calls != 1 {
		t.Fatalf("second open today: %+v, %d calls", r, calls)
	}
	// A day on, Telegram cannot be reached: the old text is still answered.
	m := s.tel.store().data.Members[key]
	m.At -= 25 * 3600
	m.AboutAt -= 25 * 3600
	s.tel.store().data.Members[key] = m
	fail = errors.New("dial tcp: i/o timeout")
	rec := httptest.NewRecorder()
	s.handleTelMembers(rec, telJSONReq("/api/tel/members", `{"peer":`+ch+`,"about":true}`))
	if calls != 2 {
		t.Fatalf("a stale description was not read again: %d calls", calls)
	}
	rec = httptest.NewRecorder()
	s.handleTelMembers(rec, httptest.NewRequest(http.MethodGet, "/api/tel/members?kind=channel&id=9&accessHash=1", nil))
	var g resp
	_ = json.Unmarshal(rec.Body.Bytes(), &g)
	if g.About != "Hourly news" || !g.AboutRead || g.AboutFresh {
		t.Fatalf("after a failed read: %+v, want the old text, marked stale", g)
	}
	// Telegram refuses: the old text stays, held for the day.
	fail = tgerr.New(400, "CHANNEL_PRIVATE")
	if r := post(`{"peer":` + ch + `,"about":true}`); !r.OK || r.About != "Hourly news" || !r.AboutFresh || calls != 3 {
		t.Fatalf("refused: %+v, %d calls", r, calls)
	}
	if r := post(`{"peer":` + ch + `,"about":true}`); calls != 3 {
		t.Fatalf("a refusal was asked again the same day: %+v", r)
	}
	// Not channels: never a call, whatever is asked.
	s.tel.store().PutDialogs([]tgbridge.Dialog{{Peer: telTestGroup}})
	for _, p := range []string{`{"kind":"user","id":"1","accessHash":"1"}`, `{"kind":"group","id":"5","accessHash":"0"}`} {
		if r := post(`{"peer":` + p + `,"about":true}`); !r.OK || calls != 3 {
			t.Fatalf("%s: %+v, %d calls", p, r, calls)
		}
	}
}

// The chat's step and its info sheet opened together ask once between them.
func TestTelChannelInfoAskedOnce(t *testing.T) {
	s := newTelTestServer(t)
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	orig := telReadChannelInfo
	defer func() { telReadChannelInfo = orig }()
	telReadChannelInfo = func(ctx context.Context, h *telHub, p tgbridge.Peer) (tgbridge.ChannelInfo, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return tgbridge.ChannelInfo{Members: 3, About: "a"}, nil
	}
	var wg sync.WaitGroup
	for _, body := range []string{`{"peer":{"kind":"channel","id":"9","accessHash":"1"}}`,
		`{"peer":{"kind":"channel","id":"9","accessHash":"1"},"about":true}`} {
		wg.Add(1)
		go func(b string) {
			defer wg.Done()
			s.handleTelMembers(httptest.NewRecorder(), telJSONReq("/api/tel/members", b))
		}(body)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls != 1 {
		t.Fatalf("asked Telegram %d times for one channel", calls)
	}
}
