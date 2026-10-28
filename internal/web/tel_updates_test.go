package web

import (
	"testing"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// A cursor is only usable once it points somewhere. Asking for a difference
// from nothing makes Telegram answer with the whole account.
func TestCursorKnown(t *testing.T) {
	cases := []struct {
		name string
		st   tgbridge.UpdateState
		want bool
	}{
		{"empty", tgbridge.UpdateState{}, false},
		{"no date", tgbridge.UpdateState{Pts: 5}, false},
		{"no pts", tgbridge.UpdateState{Date: 100}, false},
		{"complete", tgbridge.UpdateState{Pts: 5, Date: 100}, true},
	}
	for _, c := range cases {
		if got := c.st.Known(); got != c.want {
			t.Errorf("%s: Known() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCursorSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	want := tgbridge.UpdateState{Pts: 42, Qts: 7, Date: 1700, Seq: 3, Unread: 9}
	loadTelCache(dir).SetCursor(want)

	got := loadTelCache(dir).Cursor()
	if got != want {
		t.Fatalf("cursor came back as %+v, want %+v", got, want)
	}
}

// Signing out must drop the cursor with everything else: keeping it would tell
// the next account that its conversations are already up to date.
func TestForgetDropsTheCursor(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	c.SetCursor(tgbridge.UpdateState{Pts: 10, Date: 100})
	c.Forget()
	if got := loadTelCache(dir).Cursor(); got.Known() {
		t.Fatalf("the cursor survived a sign-out: %+v", got)
	}
}

// A difference moves the conversations it touches to the top, updates their
// preview and counts only what actually arrived from someone else.
func TestApplyNewUpdatesRowsAndCountsIncomingOnly(t *testing.T) {
	dir := t.TempDir()
	older := tgbridge.Peer{Kind: "channel", ID: 1, Title: "Older"}
	newer := tgbridge.Peer{Kind: "user", ID: 2, Title: "Newer"}

	c := loadTelCache(dir)
	c.PutDialogs([]tgbridge.Dialog{
		{Peer: older, Preview: "old line"},
		{Peer: newer, Preview: "also old"},
	})

	n := c.ApplyNew([]tgbridge.PeerMessages{{
		Peer: newer,
		Messages: []tgbridge.Message{
			{ID: 10, Date: 500, Text: "from them"},
			{ID: 11, Date: 501, Text: "mine", Out: true},
			{ID: 12, Date: 502, Service: true},
		},
	}})

	// Only the incoming, non-service message counts: our own echo is not news
	// and neither is "X joined the channel".
	if n != 1 {
		t.Fatalf("counted %d new, want 1 (ours and the service message must not count)", n)
	}

	ds := c.Dialogs()
	if len(ds) != 2 {
		t.Fatalf("got %d rows, want 2", len(ds))
	}
	if telCacheKey(ds[0].Peer) != telCacheKey(newer) {
		t.Fatalf("the changed conversation is not at the top: %+v", ds[0].Peer)
	}
	if ds[0].Unread != 1 {
		t.Errorf("unread = %d, want 1", ds[0].Unread)
	}
	if ds[0].TopMsg != 12 || ds[0].When != 502 {
		t.Errorf("row not advanced to the newest message: top=%d when=%d", ds[0].TopMsg, ds[0].When)
	}
	// The untouched conversation keeps its preview and position behind.
	if ds[1].Preview != "old line" {
		t.Errorf("an untouched row was disturbed: %+v", ds[1])
	}

	// And the messages themselves are now cached, so opening it costs nothing.
	if got := c.Thread(telCacheKey(newer)); len(got) != 3 {
		t.Fatalf("messages not stored: %+v", got)
	}
}

// A conversation that is not in the list yet must be inserted, not dropped:
// the first message from a new contact arrives this way.
func TestApplyNewInsertsAnUnknownConversation(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	fresh := tgbridge.Peer{Kind: "user", ID: 77, Title: "Stranger"}
	c.ApplyNew([]tgbridge.PeerMessages{{
		Peer:     fresh,
		Messages: []tgbridge.Message{{ID: 1, Date: 10, Text: "hello"}},
	}})

	ds := c.Dialogs()
	if len(ds) != 1 || telCacheKey(ds[0].Peer) != telCacheKey(fresh) {
		t.Fatalf("a new conversation was not added: %+v", ds)
	}
	if ds[0].Preview != "hello" || ds[0].Unread != 1 {
		t.Fatalf("row built wrong: %+v", ds[0])
	}
}

// The peer in a difference is fresher than the cached one, so a renamed
// channel shows its new name without a separate fetch.
func TestApplyNewRefreshesThePeer(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	before := tgbridge.Peer{Kind: "channel", ID: 5, Title: "Old Name"}
	c.PutDialogs([]tgbridge.Dialog{{Peer: before, Preview: "x"}})

	after := tgbridge.Peer{Kind: "channel", ID: 5, Title: "New Name"}
	c.ApplyNew([]tgbridge.PeerMessages{{
		Peer:     after,
		Messages: []tgbridge.Message{{ID: 2, Date: 20, Text: "y"}},
	}})

	if got := c.Dialogs()[0].Peer.Title; got != "New Name" {
		t.Fatalf("title = %q, want the fresher one", got)
	}
}

// Muting a conversation must stop it interrupting, while still counting
// unread and moving it up the list -- which is what muting means everywhere.
func TestMutedConversationsDoNotNotify(t *testing.T) {
	dir := t.TempDir()
	loud := tgbridge.Peer{Kind: "channel", ID: 1, Title: "Loud"}
	quiet := tgbridge.Peer{Kind: "channel", ID: 2, Title: "Quiet"}

	c := loadTelCache(dir)
	c.PutDialogs([]tgbridge.Dialog{
		{Peer: loud},
		{Peer: quiet, Muted: true},
	})

	n := c.ApplyNew([]tgbridge.PeerMessages{
		{Peer: quiet, Messages: []tgbridge.Message{{ID: 1, Date: 10, Text: "noisy channel"}}},
	})
	if n != 0 {
		t.Fatalf("a muted conversation asked to notify (%d)", n)
	}
	// But it still counts and still rises.
	ds := c.Dialogs()
	if telCacheKey(ds[0].Peer) != telCacheKey(quiet) || ds[0].Unread != 1 {
		t.Fatalf("a muted conversation must still count unread and move up: %+v", ds[0])
	}
	if !ds[0].Muted {
		t.Error("the mute flag was lost when the row was updated")
	}

	// An unmuted one still does notify.
	if got := c.ApplyNew([]tgbridge.PeerMessages{
		{Peer: loud, Messages: []tgbridge.Message{{ID: 2, Date: 11, Text: "hi"}}},
	}); got != 1 {
		t.Fatalf("an audible conversation notified %d times, want 1", got)
	}
}

// A conversation we have never listed must still be able to notify: the first
// message from a new contact arrives before any row exists for it.
func TestUnknownConversationStillNotifies(t *testing.T) {
	dir := t.TempDir()
	c := loadTelCache(dir)
	if got := c.ApplyNew([]tgbridge.PeerMessages{{
		Peer:     tgbridge.Peer{Kind: "user", ID: 9, Title: "New"},
		Messages: []tgbridge.Message{{ID: 1, Date: 10, Text: "hello"}},
	}}); got != 1 {
		t.Fatalf("notified %d times for a first message, want 1", got)
	}
}
