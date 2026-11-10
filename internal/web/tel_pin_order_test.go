package web

import (
	"testing"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// A row re-read on its own (a delivery tick, a mute repair) is pinned but
// carries no pin order: that only comes with a list page. It keeps the one it
// had. Unpinned, it has none. The next list page sets them all afresh.
func TestTelPinOrderSurvivesSingleRowReads(t *testing.T) {
	c := loadTelCache(t.TempDir())
	a := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 1, AccessHash: 1}
	b := tgbridge.Peer{Kind: tgbridge.PeerChannel, ID: 2, AccessHash: 1}
	c.PutDialogs([]tgbridge.Dialog{{Peer: a, Pinned: true, PinOrder: 2}, {Peer: b, Pinned: true, PinOrder: 1}})
	c.MergeDialogs([]tgbridge.Dialog{{Peer: a, Pinned: true, Unread: 3}}) // single-row read
	if got := c.Dialogs()[0]; got.PinOrder != 2 || got.Unread != 3 {
		t.Fatalf("single-row read: pinOrder %d unread %d, want 2 and 3", got.PinOrder, got.Unread)
	}
	c.MergeDialogs([]tgbridge.Dialog{{Peer: a}}) // unpinned elsewhere
	if got := c.Dialogs()[0]; got.Pinned || got.PinOrder != 0 {
		t.Fatalf("unpinned row kept a pin order: %+v", got)
	}
	c.MergeDialogs([]tgbridge.Dialog{{Peer: a, Pinned: true, PinOrder: 1}, {Peer: b, Pinned: true, PinOrder: 2}}) // a list page
	if c.Dialogs()[0].PinOrder != 1 || c.Dialogs()[1].PinOrder != 2 {
		t.Fatalf("a list page did not set the order: %+v", c.Dialogs())
	}
}
