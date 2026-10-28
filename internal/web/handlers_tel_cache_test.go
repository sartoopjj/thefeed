package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// telSeedCache fills a hub's store with one conversation and its messages.
func telSeedCache(t *testing.T, s *Server) tgbridge.Peer {
	t.Helper()
	peer := tgbridge.Peer{Kind: "channel", ID: 42, AccessHash: 7, Title: "News Channel"}
	s.tel.store().PutDialogs([]tgbridge.Dialog{{Peer: peer, When: 100, TopMsg: 9, Preview: "hi"}})
	s.tel.store().MergeThread(telCacheKey(peer), []tgbridge.Message{
		{ID: 8, Text: "older"}, {ID: 9, Text: "newest"},
	})
	return peer
}

type telCacheResp struct {
	OK       bool                 `json:"ok"`
	Cached   bool                 `json:"cached"`
	Reason   string               `json:"reason"`
	Messages []tgbridge.Message   `json:"messages"`
	Page     *tgbridge.DialogPage `json:"page"`
}

func telDo(t *testing.T, h func(http.ResponseWriter, *http.Request), url string) telCacheResp {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var resp telCacheResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: %v (body %s)", url, err, rec.Body.String())
	}
	return resp
}

// A conversation list already on disk must survive a lost tunnel.
//
// Answering "not connected" here is what made restarting the client -- or just
// reloading the page before the tunnel was back up -- look as if every
// conversation and every message had been wiped. They had not: they were
// sealed on disk the whole time.
func TestTelServesCacheWhileDisconnected(t *testing.T) {
	s := newTelTestServer(t)
	peer := telSeedCache(t, s)

	d := telDo(t, s.handleTelDialogs, "/api/tel/dialogs?limit=40")
	if !d.OK || d.Page == nil || len(d.Page.Dialogs) != 1 {
		t.Fatalf("dialogs: want the cached list, got %+v", d)
	}
	if !d.Cached {
		t.Error("dialogs: a cached answer must say so, or the UI claims stale counts are current")
	}

	q := "/api/tel/history?kind=" + string(peer.Kind) + "&id=42&accessHash=7&limit=30"
	h := telDo(t, s.handleTelHistory, q)
	if !h.OK || len(h.Messages) != 2 {
		t.Fatalf("history: want the cached messages, got %+v", h)
	}
	if !h.Cached {
		t.Error("history: a cached answer must say so")
	}
}

// Paging back is a request for messages we are saying we do not have, so it
// must still fail rather than re-serving the newest page as if it were older.
func TestTelCachedHistoryOnlyServesTheNewestPage(t *testing.T) {
	s := newTelTestServer(t)
	telSeedCache(t, s)
	r := telDo(t, s.handleTelHistory,
		"/api/tel/history?kind=channel&id=42&accessHash=7&limit=30&offsetId=8")
	if r.OK {
		t.Fatalf("paging back with no link must fail, got %+v", r)
	}
	if r.Reason != "tel_err_not_connected" {
		t.Errorf("reason = %q, want tel_err_not_connected", r.Reason)
	}
}

// cacheOnly must never depend on a tunnel: it exists so a freshly loaded page
// can show what it has while a connect still holds the one link.
func TestTelCacheOnlyNeedsNoTunnel(t *testing.T) {
	s := newTelTestServer(t)
	telSeedCache(t, s)

	d := telDo(t, s.handleTelDialogs, "/api/tel/dialogs?cacheOnly=1&limit=40")
	if !d.OK || !d.Cached || d.Page == nil || len(d.Page.Dialogs) != 1 {
		t.Fatalf("cacheOnly dialogs: %+v", d)
	}
	h := telDo(t, s.handleTelHistory,
		"/api/tel/history?kind=channel&id=42&accessHash=7&limit=30&cacheOnly=1")
	if !h.OK || !h.Cached || len(h.Messages) != 2 {
		t.Fatalf("cacheOnly history: %+v", h)
	}
}

// An empty cache says so plainly instead of claiming the link is down: the
// caller uses it to decide whether to show anything at all, not to report a
// connection problem the user cannot act on.
func TestTelCacheOnlyReportsAnEmptyCache(t *testing.T) {
	s := newTelTestServer(t)
	d := telDo(t, s.handleTelDialogs, "/api/tel/dialogs?cacheOnly=1&limit=40")
	if d.OK || d.Reason != "tel_err_no_cache" {
		t.Fatalf("dialogs: %+v", d)
	}
	h := telDo(t, s.handleTelHistory,
		"/api/tel/history?kind=channel&id=42&accessHash=7&cacheOnly=1")
	if h.OK || h.Reason != "tel_err_no_cache" {
		t.Fatalf("history: %+v", h)
	}
}

// A forced refresh with no link falls back to the list it has. The point of
// refresh=1 is to ask Telegram when there IS a link; with none, the user still
// gets their conversations rather than an error screen.
func TestTelForcedRefreshFallsBackToCache(t *testing.T) {
	s := newTelTestServer(t)
	telSeedCache(t, s)
	d := telDo(t, s.handleTelDialogs, "/api/tel/dialogs?limit=40&refresh=1")
	if !d.OK || d.Page == nil || len(d.Page.Dialogs) != 1 {
		t.Fatalf("forced refresh while down: %+v", d)
	}
}

// A chat read on the phone. Nothing else in a difference can bring the badge
// down -- it carries messages and read marks, never conversation rows -- so if
// this does not land, a conversation marked read elsewhere keeps showing new
// messages here until something pays for the whole list again.
func TestApplyInReadsClearsUnread(t *testing.T) {
	c := loadTelCache(t.TempDir())
	p := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 7, AccessHash: 1, Title: "News Channel"}
	c.PutDialogs([]tgbridge.Dialog{{Peer: p, Unread: 12, TopMsg: 90, ReadInbox: 70}})

	if n := c.ApplyInReads([]tgbridge.PeerInRead{{Peer: p, MaxID: 90, Unread: 0}}); n == 0 {
		t.Fatal("the read mark changed the row; it must report so")
	}
	d := c.Dialogs()[0]
	if d.ReadInbox != 90 {
		t.Errorf("readInbox = %d, want 90", d.ReadInbox)
	}
	// Zero is the whole point: it is what takes the badge off the row.
	if d.Unread != 0 {
		t.Errorf("unread = %d, want the badge cleared", d.Unread)
	}
}

// Telegram's still-unread count is the authority, including when it is not
// zero: reading part of a busy channel elsewhere leaves a smaller badge, not
// none.
func TestApplyInReadsTakesTelegramsCount(t *testing.T) {
	c := loadTelCache(t.TempDir())
	p := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 7, AccessHash: 1}
	c.PutDialogs([]tgbridge.Dialog{{Peer: p, Unread: 12, TopMsg: 90}})
	c.ApplyInReads([]tgbridge.PeerInRead{{Peer: p, MaxID: 80, Unread: 3}})
	if got := c.Dialogs()[0].Unread; got != 3 {
		t.Errorf("unread = %d, want Telegram's 3", got)
	}
}

// An update overtaken by a later one must not un-read what has since been
// read. Out-of-order delivery is normal on a lossy link, and a badge that
// comes back after the user cleared it is worse than one that lingers.
func TestApplyInReadsNeverGoesBackwards(t *testing.T) {
	c := loadTelCache(t.TempDir())
	p := tgbridge.Peer{Kind: tgbridge.PeerUser, ID: 3, AccessHash: 1}
	c.PutDialogs([]tgbridge.Dialog{{Peer: p, Unread: 0, ReadInbox: 90}})
	if n := c.ApplyInReads([]tgbridge.PeerInRead{{Peer: p, MaxID: 70, Unread: 5}}); n != 0 {
		t.Errorf("a stale mark must change nothing, reported %d changes", n)
	}
	d := c.Dialogs()[0]
	if d.ReadInbox != 90 || d.Unread != 0 {
		t.Errorf("stale mark moved the row: %+v", d)
	}
}

// A mark for a conversation that is not in the list is not a reason to invent
// a row: it would have no preview, no stamp and nothing to open.
func TestApplyInReadsIgnoresUnknownPeers(t *testing.T) {
	c := loadTelCache(t.TempDir())
	known := tgbridge.Peer{Kind: tgbridge.PeerUser, ID: 3, AccessHash: 1}
	c.PutDialogs([]tgbridge.Dialog{{Peer: known, Unread: 2}})
	c.ApplyInReads([]tgbridge.PeerInRead{
		{Peer: tgbridge.Peer{Kind: tgbridge.PeerUser, ID: 404}, MaxID: 5, Unread: 0},
	})
	if got := len(c.Dialogs()); got != 1 {
		t.Fatalf("list grew to %d rows", got)
	}
	if c.Dialogs()[0].Unread != 2 {
		t.Errorf("the known row was touched: %+v", c.Dialogs()[0])
	}
}

// Muting on another device has to reach the cached row, in both directions.
// The flag is not cosmetic: ApplyNew asks it whether an arriving message may
// announce itself, so a stale one is heard, not just seen.
func TestApplyMutesFollowsTelegramBothWays(t *testing.T) {
	c := loadTelCache(t.TempDir())
	loud := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 7, AccessHash: 1}
	quiet := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 8, AccessHash: 1}
	c.PutDialogs([]tgbridge.Dialog{
		{Peer: loud, Muted: false}, {Peer: quiet, Muted: true},
	})
	if n := c.ApplyMutes([]tgbridge.PeerMute{
		{Peer: loud, Muted: true}, {Peer: quiet, Muted: false},
	}); n != 2 {
		t.Fatalf("both rows changed, reported %d", n)
	}
	got := map[int64]bool{}
	for _, d := range c.Dialogs() {
		got[d.Peer.ID] = d.Muted
	}
	if !got[7] {
		t.Error("channel muted on the phone is still audible here")
	}
	if got[8] {
		t.Error("channel unmuted on the phone is still silent here")
	}
}

// Saying the same thing twice must not cost a disk write.
func TestApplyMutesNoopWhenUnchanged(t *testing.T) {
	c := loadTelCache(t.TempDir())
	p := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 7, AccessHash: 1}
	c.PutDialogs([]tgbridge.Dialog{{Peer: p, Muted: true}})
	if n := c.ApplyMutes([]tgbridge.PeerMute{{Peer: p, Muted: true}}); n != 0 {
		t.Errorf("nothing changed, reported %d changes", n)
	}
}

// A setting for a conversation that is not in the list is not a reason to
// invent a row: it would have no preview, no stamp and nothing to open.
func TestApplyMutesIgnoresUnknownPeers(t *testing.T) {
	c := loadTelCache(t.TempDir())
	known := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 7, AccessHash: 1}
	c.PutDialogs([]tgbridge.Dialog{{Peer: known, Muted: false}})
	c.ApplyMutes([]tgbridge.PeerMute{
		{Peer: tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 404}, Muted: true},
	})
	if got := len(c.Dialogs()); got != 1 {
		t.Fatalf("list grew to %d rows", got)
	}
	if c.Dialogs()[0].Muted {
		t.Error("the known row was touched")
	}
}

// Every cache-served answer used to say "no more pages". On an account with
// more conversations than one page holds that hid "load more chats"
// completely, because the first page comes from the cache every time except a
// forced refresh -- so the rest of the list was unreachable.
func TestCachedDialogPageOffersTheNextPage(t *testing.T) {
	rows := make([]tgbridge.Dialog, 40)
	for i := range rows {
		rows[i] = tgbridge.Dialog{
			Peer:   tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: int64(i + 1), AccessHash: 9},
			TopMsg: 500 - i, When: 1700000000 - i,
		}
	}
	page := cachedDialogPage(rows, 40, 0, false)
	if !page.More {
		t.Fatal("a full cached page must offer the next one")
	}
	// The cursor is the LAST row: Telegram pages backwards from the oldest.
	if page.NextID != rows[39].TopMsg {
		t.Errorf("nextId = %d, want %d", page.NextID, rows[39].TopMsg)
	}
	if page.NextDate != rows[39].When {
		t.Errorf("nextDate = %d, want %d", page.NextDate, rows[39].When)
	}
	// Without the peer's access hash Telegram cannot be asked to continue.
	if page.NextPeer.ID != rows[39].Peer.ID || page.NextPeer.AccessHash != 9 {
		t.Errorf("nextPeer = %+v, want the last row with its hash", page.NextPeer)
	}
}

// A short page is the end of the list -- the live path's own rule. Claiming
// otherwise hands the user a button that fetches nothing.
func TestCachedDialogPageStopsAtAShortPage(t *testing.T) {
	rows := []tgbridge.Dialog{
		{Peer: tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 1}, TopMsg: 10},
	}
	if page := cachedDialogPage(rows, 40, 0, false); page.More {
		t.Errorf("6 of 6 conversations is not a full page: %+v", page)
	}
	// A row with no top message cannot be paged from at all.
	full := make([]tgbridge.Dialog, 40)
	for i := range full {
		full[i] = tgbridge.Dialog{Peer: tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: int64(i + 1)}}
	}
	if page := cachedDialogPage(full, 40, 0, false); page.More {
		t.Errorf("no cursor to continue from, must not claim more: %+v", page)
	}
}

// Telegram's own count is what tells a partial cache from a complete one.
// Forty rows of three hundred looks exactly like all forty of forty, and only
// the count says which — the case that left an account with ~300 conversations
// stuck on the first page with no way to ask for the rest.
func TestCachedDialogPageUsesTelegramsTotal(t *testing.T) {
	rows := make([]tgbridge.Dialog, 40)
	for i := range rows {
		rows[i] = tgbridge.Dialog{
			Peer:   tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: int64(i + 1), AccessHash: 9},
			TopMsg: 500 - i, When: 1700000000 - i,
		}
	}
	page := cachedDialogPage(rows, 40, 300, false)
	if !page.More || page.NextID != rows[39].TopMsg {
		t.Fatalf("40 of 300 must offer the rest: %+v", page)
	}
	if page.TotalKnown != 300 {
		t.Errorf("totalKnown = %d, want 300 — the list cannot say '40 of 300'", page.TotalKnown)
	}

	// The whole list cached: no more to ask for, whatever its length.
	all := make([]tgbridge.Dialog, 300)
	for i := range all {
		all[i] = tgbridge.Dialog{
			Peer:   tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: int64(i + 1)},
			TopMsg: 1000 - i,
		}
	}
	if p := cachedDialogPage(all, 40, 300, false); p.More {
		t.Error("300 of 300 must not offer another page")
	}
	// A full page that IS the whole account: the count overrides the
	// short-page rule, which would otherwise claim a 41st page for ever.
	exact := rows
	if p := cachedDialogPage(exact, 40, 40, false); p.More {
		t.Error("40 of 40 must not offer another page")
	}
}

// The count survives a restart, or the next start is back to guessing from
// the page length alone.
func TestDialogTotalPersists(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	c.SetDialogTotal(300)
	// Zero is "Telegram did not say", never "the account has none".
	c.SetDialogTotal(0)
	if got := c.DialogTotal(); got != 300 {
		t.Fatalf("total = %d, want 300 kept", got)
	}
	if got := loadTelCache(dir).DialogTotal(); got != 300 {
		t.Errorf("after reload total = %d, want 300", got)
	}
}

// Restoring a backup must pick the eviction clock back up past what it
// restored, exactly as loading from disk does.
//
// The counter restarted at zero while every restored thread carried a much
// larger Touched, so the next thread the user opened was stamped lowest and
// was the first one evicted — the one they were reading.
func TestImportKeepsTheEvictionClockAhead(t *testing.T) {
	src := loadTelCache(t.TempDir())
	p := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 1, AccessHash: 1}
	for i := 0; i < 5; i++ {
		src.MergeThread(telCacheKey(tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: int64(i + 1)}),
			[]tgbridge.Message{{ID: i + 1, Text: "x", Date: 1}})
	}
	blob, err := src.export()
	if err != nil {
		t.Fatal(err)
	}

	dst := loadTelCache(t.TempDir())
	dst.importFrom(blob)

	// Whatever the user opens next must be stamped ABOVE everything restored.
	dst.MergeThread(telCacheKey(p), []tgbridge.Message{{ID: 99, Text: "just opened", Date: 2}})

	dst.mu.Lock()
	newest := dst.data.Threads[telCacheKey(p)].Touched
	var highestRestored int64
	for k, th := range dst.data.Threads {
		if k != telCacheKey(p) && th.Touched > highestRestored {
			highestRestored = th.Touched
		}
	}
	dst.mu.Unlock()

	if newest <= highestRestored {
		t.Fatalf("the thread just opened is stamped %d, below a restored one at %d — "+
			"it is first in line to be evicted", newest, highestRestored)
	}
}

// A page the user paid for must be kept.
//
// PutDialogs replaces the list, which is right for a first page and wrong for
// every page after it — so later pages were dropped. The user pressed "load
// more chats", spent a page of DNS queries on it, and the next start began
// again from the forty rows of page one.
func TestMergeDialogsKeepsLaterPages(t *testing.T) {
	c := loadTelCache(t.TempDir())
	page := func(from, n int) []tgbridge.Dialog {
		out := make([]tgbridge.Dialog, n)
		for i := range out {
			out[i] = tgbridge.Dialog{
				Peer:   tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: int64(from + i), AccessHash: 9},
				TopMsg: 1000 - from - i,
			}
		}
		return out
	}
	c.PutDialogs(page(1, 40))
	if n := c.MergeDialogs(page(41, 40)); n != 40 {
		t.Fatalf("merged %d rows, want 40 new ones", n)
	}
	if got := len(c.Dialogs()); got != 80 {
		t.Fatalf("cache holds %d rows, want 80 — the second page was dropped", got)
	}

	// A row seen again is refreshed, not duplicated.
	again := page(41, 1)
	again[0].Unread = 7
	if n := c.MergeDialogs(again); n != 0 {
		t.Errorf("a row already held is not new, reported %d", n)
	}
	if got := len(c.Dialogs()); got != 80 {
		t.Fatalf("cache grew to %d rows on a repeat", got)
	}
	for _, d := range c.Dialogs() {
		if d.Peer.ID == 41 && d.Unread != 7 {
			t.Errorf("the repeated row kept its stale count: %+v", d)
		}
	}
}

// A partial page must never destroy the pages the user paid for.
//
// Two places fetched a SHORT page and then replaced the whole cached list with
// it: the resync (twenty rows, to re-establish the cursor) and the first-page
// refresh (forty). On an account with hundreds of conversations that threw
// away everything past the twentieth or fortieth row — including every page
// the user had asked for with "load more chats", each one a page of DNS
// queries. And the resync runs on any cursor gap, or on a single post from a
// channel the cache had not seen.
func TestPartialPagesDoNotDestroyLoadedOnes(t *testing.T) {
	c := loadTelCache(t.TempDir())
	row := func(id int64, muted bool) tgbridge.Dialog {
		return tgbridge.Dialog{
			Peer:   tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: id, AccessHash: 9},
			TopMsg: int(1000 - id), Muted: muted,
		}
	}
	// What the user built up: 120 conversations, several pages of queries.
	all := make([]tgbridge.Dialog, 0, 120)
	for i := int64(1); i <= 120; i++ {
		all = append(all, row(i, false))
	}
	c.PutDialogs(all[:40])
	c.MergeDialogs(all[40:])
	if got := len(c.Dialogs()); got != 120 {
		t.Fatalf("setup holds %d rows", got)
	}

	// A resync lands: twenty rows, as telWatchDialogs fetches.
	c.MergeDialogs(all[:20])
	if got := len(c.Dialogs()); got != 120 {
		t.Fatalf("a twenty-row resync left %d of 120 rows — the pages the user "+
			"paid for were thrown away", got)
	}

	// And it still carries the fresh values for the rows it did cover.
	c.MergeDialogs([]tgbridge.Dialog{row(3, true)})
	for _, d := range c.Dialogs() {
		if d.Peer.ID == 3 && !d.Muted {
			t.Error("a refreshed row did not take its new notification setting")
		}
	}
	if got := len(c.Dialogs()); got != 120 {
		t.Fatalf("refreshing one row changed the count to %d", got)
	}
}

// The list must be able to END, even when Telegram's count is never reached.
//
// That count includes conversations this client cannot show — a dialog whose
// entity did not arrive with the page is dropped — so "held < total" can stay
// true no matter how many pages are fetched. A list of 197 showing 198 kept
// offering a button that fetched nothing, for ever. Telegram saying "no page
// after this one" is the end marker; the count is not.
func TestCachedDialogPageEndsWhenTelegramSaysSo(t *testing.T) {
	rows := make([]tgbridge.Dialog, 197)
	for i := range rows {
		rows[i] = tgbridge.Dialog{
			Peer:   tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: int64(i + 1), AccessHash: 9},
			TopMsg: 1000 - i,
		}
	}
	// Before the end is known, the count is still the useful signal.
	if p := cachedDialogPage(rows, 40, 198, false); !p.More {
		t.Error("197 of 198 with no end marker yet: still worth offering more")
	}
	// Once Telegram has said the list ends, the count is irrelevant.
	p := cachedDialogPage(rows, 40, 198, true)
	if p.More {
		t.Fatal("Telegram said the list ends, but the cache still offers another " +
			"page — the button fetches nothing and never goes away")
	}
	// The count is still reported, so the UI can show it if it wants to.
	if p.TotalKnown != 198 {
		t.Errorf("totalKnown = %d, want 198", p.TotalKnown)
	}
}

// The marker has to survive a restart, or the next start offers the dead
// button again.
func TestDialogsCompletePersists(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	if c.DialogsComplete() {
		t.Fatal("a fresh cache has not reached the end of anything")
	}
	c.SetDialogsComplete(true)
	if !loadTelCache(dir).DialogsComplete() {
		t.Error("the end marker did not survive a reload")
	}
	// And it can be taken back: a new conversation means there is more again.
	c.SetDialogsComplete(false)
	if loadTelCache(dir).DialogsComplete() {
		t.Error("the end marker could not be cleared")
	}
}

// A conversation the cache never listed must not be called "not muted".
//
// A difference names a conversation and carries its message; it says nothing
// about notification settings. Most rows on a busy account arrive that way, so
// a cache that filled the gap with the zero value reported most of the list as
// audible -- including the channels the user had deliberately silenced.
func TestTelCacheDoesNotInventNotMuted(t *testing.T) {
	c := loadTelCache(t.TempDir())
	ch := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 7, AccessHash: 1, Title: "Quiet"}

	// Straight from a difference: the row did not exist before.
	c.ApplyNew([]tgbridge.PeerMessages{{
		Peer:     ch,
		Messages: []tgbridge.Message{{ID: 10, Date: 1, Text: "post"}},
	}})

	rows := c.Dialogs()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want the one the difference named", len(rows))
	}
	if !rows[0].NotifyUnknown {
		t.Fatal("a row built from a difference claims to know its notification setting; " +
			"it was never asked, and reporting that as \"not muted\" makes a silenced channel ring")
	}
	if !c.hasUnknownNotify(ch) {
		t.Fatal("the row is not offered for repair, so it would stay a guess for ever")
	}

	// The repair: Telegram's own row for that peer, which says it IS muted.
	c.MergeDialogs([]tgbridge.Dialog{{Peer: ch, Muted: true, TopMsg: 10}})
	rows = c.Dialogs()
	if !rows[0].Muted {
		t.Fatal("the authoritative row did not replace the invented one")
	}
	if rows[0].NotifyUnknown {
		t.Fatal("still marked unknown after Telegram answered; it would be re-asked every poll")
	}
	if len(c.UnknownNotify(10)) != 0 {
		t.Fatal("a repaired row is still queued for repair")
	}
}

// A peer Telegram declines to describe must stop costing a call every poll.
func TestTelCacheStopsAskingAboutAPeerItAlreadyAsked(t *testing.T) {
	c := loadTelCache(t.TempDir())
	ch := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 8, AccessHash: 1, Title: "Gone"}
	c.ApplyNew([]tgbridge.PeerMessages{{
		Peer:     ch,
		Messages: []tgbridge.Message{{ID: 1, Date: 1, Text: "x"}},
	}})
	if len(c.UnknownNotify(10)) != 1 {
		t.Fatal("the invented row should be queued for repair")
	}
	// Asked, and Telegram returned nothing for it.
	c.ClearNotifyUnknown([]string{telCacheKey(ch)})
	if got := len(c.UnknownNotify(10)); got != 0 {
		t.Fatalf("still queued after being asked (%d); that is one wasted call every poll, for ever", got)
	}
}

// hasUnknownNotify reports whether the peer is queued for a settings repair.
func (c *telCache) hasUnknownNotify(p tgbridge.Peer) bool {
	for _, q := range c.UnknownNotify(100) {
		if telCacheKey(q) == telCacheKey(p) {
			return true
		}
	}
	return false
}
