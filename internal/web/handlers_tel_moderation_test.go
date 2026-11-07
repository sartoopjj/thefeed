package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// Block applies to people and bots only: Telegram's blocklist has no meaning
// for a group or channel, where leaving or reporting is the answer.
func TestTelBlockOnlyPeopleAndBots(t *testing.T) {
	s := newTelTestServer(t)
	for _, kind := range []string{"group", "channel", "forum"} {
		rec := httptest.NewRecorder()
		s.handleTelBlock(rec, telJSONReq("/api/tel/block", `{"peer":{"kind":"`+kind+`","id":"5"},"blocked":true}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", kind, rec.Code)
		}
	}
}

// A block Telegram did not take must not show as blocked: the user would
// believe they are protected when they are not.
func TestTelBlockNotRecordedWhenTelegramDidNotTakeIt(t *testing.T) {
	s := newTelTestServer(t)
	rec := httptest.NewRecorder()
	s.handleTelBlock(rec, telJSONReq("/api/tel/block", `{"peer":{"kind":"user","id":"42","accessHash":"9"},"blocked":true}`))
	var resp struct {
		OK bool `json:"ok"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.OK {
		t.Fatal("reported success with no connection")
	}
	if got := s.tel.store().BlockedPeers(); len(got) != 0 {
		t.Fatalf("marked blocked anyway: %v", got)
	}
}

// The list survives a restart, and goes with the account at sign-out.
func TestTelBlockedListPersistsAndLeavesWithTheAccount(t *testing.T) {
	s := newTelTestServer(t)
	st := s.tel.store()
	st.SetBlocked(tgbridge.Peer{Kind: "user", ID: 42, AccessHash: 9, Title: "Abuser"}, true)
	st.SetBlocked(tgbridge.Peer{Kind: "bot", ID: 7, AccessHash: 1, Title: "Spam bot"}, true)
	st.SetBlocked(tgbridge.Peer{Kind: "bot", ID: 7}, false)
	if got := loadTelCache(s.tel.dir).BlockedPeers(); len(got) != 1 || got[0].Peer.ID != 42 || got[0].Peer.Title != "Abuser" || got[0].Date == 0 {
		t.Fatalf("after restart: %+v", got)
	}
	rec := httptest.NewRecorder()
	s.handleTelBlock(rec, httptest.NewRequest(http.MethodGet, "/api/tel/block", nil))
	var list struct {
		Blocked []tgbridge.BlockedPeer `json:"blocked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Blocked) != 1 || list.Blocked[0].Peer.AccessHash != 9 {
		t.Fatalf("GET: %s", rec.Body.String())
	}
	if err := s.tel.wipeSession(); err != nil {
		t.Fatal(err)
	}
	if got := loadTelCache(s.tel.dir).BlockedPeers(); len(got) != 0 {
		t.Fatalf("the next account inherits the blocklist: %v", got)
	}
}

// Blocks learnt from another device. An update often names only the id; it
// must not wipe the name and access hash already known, which Unblock needs.
func TestTelBlocksFromOtherDevices(t *testing.T) {
	st := newTelTestServer(t).tel.store()
	full := tgbridge.Peer{Kind: "user", ID: 42, AccessHash: 9, Title: "Abuser"}
	st.SetBlocked(full, true)
	st.ApplyBlocks([]tgbridge.PeerBlock{
		{Peer: tgbridge.Peer{Kind: "user", ID: 42}, Blocked: true}, // repeat, no entity
		{Peer: tgbridge.Peer{Kind: "user", ID: 77}, Blocked: true}, // new, no entity
	})
	got := map[int64]tgbridge.Peer{}
	for _, b := range st.BlockedPeers() {
		got[b.Peer.ID] = b.Peer
	}
	if p := got[42]; p.AccessHash != full.AccessHash || p.Title != full.Title {
		t.Errorf("the known details were lost: %+v", got[42])
	}
	if _, ok := got[77]; !ok {
		t.Error("a block made on another device was not recorded")
	}
	// Unblocked on the phone: gone here too.
	st.ApplyBlocks([]tgbridge.PeerBlock{{Peer: tgbridge.Peer{Kind: "user", ID: 42}, Blocked: false}})
	for _, b := range st.BlockedPeers() {
		if b.Peer.ID == 42 {
			t.Fatal("an unblock from another device was missed")
		}
	}
}

// Telegram's list replaces ours when it is complete; a partial page must not
// drop the entries past it.
func TestTelBlockedListReplace(t *testing.T) {
	st := newTelTestServer(t).tel.store()
	st.SetBlocked(tgbridge.Peer{Kind: "user", ID: 1, AccessHash: 1}, true)
	st.ReplaceBlocked([]tgbridge.BlockedPeer{{Peer: tgbridge.Peer{Kind: "user", ID: 2, AccessHash: 2}}}, false, st.BlockEdits())
	if n := len(st.BlockedPeers()); n != 2 {
		t.Fatalf("partial page: %d entries, want 2", n)
	}
	st.ReplaceBlocked([]tgbridge.BlockedPeer{{Peer: tgbridge.Peer{Kind: "user", ID: 2, AccessHash: 2}}}, true, st.BlockEdits())
	if ps := st.BlockedPeers(); len(ps) != 1 || ps[0].Peer.ID != 2 {
		t.Fatalf("complete list: %+v (unblocked elsewhere must go)", ps)
	}
}

// A block learnt without the person's details takes them from the cached
// conversation list: Unblock needs the access hash.
func TestTelBlockWithoutDetailsUsesTheConversationList(t *testing.T) {
	st := newTelTestServer(t).tel.store()
	st.MergeDialogs([]tgbridge.Dialog{{Peer: tgbridge.Peer{Kind: "user", ID: 42, AccessHash: 9, Title: "Known"}}})
	st.ApplyBlocks([]tgbridge.PeerBlock{{Peer: tgbridge.Peer{Kind: "user", ID: 42}, Blocked: true}})
	got := st.BlockedPeers()
	if len(got) != 1 || got[0].Peer.AccessHash != 9 || got[0].Peer.Title != "Known" {
		t.Fatalf("got %+v", got)
	}
}

// A repeat of a block (an echo of our own, or another update) keeps when it
// was made and the deleted mark: the list's order and names must not change.
func TestTelRepeatBlockKeepsDateAndDeletedMark(t *testing.T) {
	st := newTelTestServer(t).tel.store()
	st.ReplaceBlocked([]tgbridge.BlockedPeer{
		{Peer: tgbridge.Peer{Kind: "user", ID: 1, AccessHash: 1, Title: "Old"}, Date: 1000},
		{Peer: tgbridge.Peer{Kind: "user", ID: 2, AccessHash: 2}, Date: 2000, Deleted: true},
	}, true, st.BlockEdits())
	st.ApplyBlocks([]tgbridge.PeerBlock{
		{Peer: tgbridge.Peer{Kind: "user", ID: 1, AccessHash: 1, Title: "Renamed"}, Blocked: true},
		{Peer: tgbridge.Peer{Kind: "user", ID: 2, AccessHash: 2, Title: "x"}, Blocked: true},
	})
	by := map[int64]tgbridge.BlockedPeer{}
	for _, b := range st.BlockedPeers() {
		by[b.Peer.ID] = b
	}
	if b := by[1]; b.Date != 1000 || b.Peer.Title != "Renamed" {
		t.Errorf("row 1: %+v (date must stay, name may update)", b)
	}
	if b := by[2]; b.Date != 2000 || !b.Deleted {
		t.Errorf("row 2: %+v (deleted mark and date must stay)", b)
	}
}

// ApplyBlocks says whether the list changed: that decides whether the page is
// told. A repeat of a known block is not a change.
func TestTelApplyBlocksReportsChanges(t *testing.T) {
	st := newTelTestServer(t).tel.store()
	p := tgbridge.Peer{Kind: "user", ID: 42, AccessHash: 9}
	if !st.ApplyBlocks([]tgbridge.PeerBlock{{Peer: p, Blocked: true}}) {
		t.Error("a new block was not reported")
	}
	if st.ApplyBlocks([]tgbridge.PeerBlock{{Peer: p, Blocked: true}}) {
		t.Error("a repeat block was reported as a change")
	}
	if !st.ApplyBlocks([]tgbridge.PeerBlock{{Peer: p, Blocked: false}}) {
		t.Error("an unblock was not reported")
	}
	if st.ApplyBlocks([]tgbridge.PeerBlock{{Peer: p, Blocked: false}}) {
		t.Error("unblocking someone not blocked was reported as a change")
	}
}

// The blocklist is read only when the user is at a person's chat: opened, or
// refreshed by hand. Never on the timer poll, never for an older page.
func TestTelBlocklistReadOnlyWhenLooking(t *testing.T) {
	for _, c := range []struct {
		name          string
		query         string
		poll, looking bool
	}{
		{"chat opened", "limit=20", false, true},
		{"timer poll", "limit=20&minId=500", true, false},
		{"manual refresh", "limit=20&minId=500&fresh=1", true, true},
		{"older page", "limit=20&offsetId=300", false, false},
	} {
		q, _ := url.ParseQuery(c.query)
		if poll, looking := telHistoryKind(q); poll != c.poll || looking != c.looking {
			t.Errorf("%s: poll=%v looking=%v, want %v %v", c.name, poll, looking, c.poll, c.looking)
		}
	}
	// And only for people and bots: channels and groups never spend it.
	h := newTelTestServer(t).tel
	for _, kind := range []tgbridge.PeerKind{tgbridge.PeerChannel, tgbridge.PeerGroup, tgbridge.PeerForum} {
		h.noteBlockedInterest(tgbridge.Peer{Kind: kind, ID: 1})
		if h.wantBlocked.Load() {
			t.Fatalf("%s asked for the blocklist", kind)
		}
	}
	h.noteBlockedInterest(tgbridge.Peer{Kind: tgbridge.PeerBot, ID: 2})
	if !h.wantBlocked.Load() {
		t.Fatal("a bot's chat did not ask for the blocklist")
	}
}

// A message Telegram delivered proves the block was lifted elsewhere.
func TestTelDeliveredClearsAStaleBlock(t *testing.T) {
	h := newTelTestServer(t).tel
	p := tgbridge.Peer{Kind: tgbridge.PeerUser, ID: 42, AccessHash: 9}
	h.store().SetBlocked(p, true)
	h.noteDelivered(p)
	if len(h.store().BlockedPeers()) != 0 {
		t.Fatal("a delivered message left the chat marked blocked")
	}
}

// Something that is not an invite link is refused before any query: a public
// @username or another site must not reach Telegram's invite calls.
func TestTelInviteRefusesNonInvites(t *testing.T) {
	s := newTelTestServer(t)
	for _, link := range []string{"@durov", "https://t.me/durov", "https://example.com/+AbCdEfGh123", ""} {
		rec := httptest.NewRecorder()
		s.handleTelInvite(rec, telJSONReq("/api/tel/invite", `{"link":`+strconv.Quote(link)+`,"action":"check"}`))
		var resp struct {
			OK     bool   `json:"ok"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.OK || resp.Reason != "tel_err_invite_invalid" {
			t.Errorf("%q: %s", link, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	s.handleTelInvite(rec, telJSONReq("/api/tel/invite", `{"link":"https://t.me/+AbCdEfGh123","action":"delete"}`))
	if rec.Code != 400 {
		t.Errorf("unknown action: status %d", rec.Code)
	}
}

// A read of Telegram's list that was in flight while the user blocked or
// unblocked someone here predates that change: it must not undo it.
func TestTelStaleBlocklistReadIsDropped(t *testing.T) {
	st := newTelTestServer(t).tel.store()
	edits := st.BlockEdits()                                               // the read starts
	st.SetBlocked(tgbridge.Peer{Kind: "user", ID: 7, AccessHash: 7}, true) // the user blocks meanwhile
	if st.ReplaceBlocked(nil, true, edits) {
		t.Fatal("a read that overlapped a block was applied")
	}
	if len(st.BlockedPeers()) != 1 {
		t.Fatal("the block made during the read was wiped")
	}
	if !st.ReplaceBlocked(nil, true, st.BlockEdits()) || len(st.BlockedPeers()) != 0 {
		t.Fatal("a read with no overlap was not applied")
	}
}
