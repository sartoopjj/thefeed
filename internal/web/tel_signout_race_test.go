package web

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// A call that was already on the tunnel when the user signed out must not
// write its answer into the cache: the next account signed in on this device
// would see the previous account's conversations.
//
// Over DNS a dialog or history fetch can take tens of seconds, so signing out
// in the middle of one is the ordinary case, not a corner. Repeated, because
// a user who switches accounts does it more than once.
func TestCallInFlightAtSignOutWritesNothing(t *testing.T) {
	s := newTelTestServer(t)
	h := s.tel
	for round := 0; round < 20; round++ {
		h.mu.Lock()
		h.bridge = &tgbridge.Bridge{} // account 1's connection
		h.mu.Unlock()

		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			// Exactly the shape of every cache writer: fetch through the
			// bridge, and write only if the fetch succeeded.
			var rows []tgbridge.Dialog
			err := h.withBridge(context.Background(), func(b *tgbridge.Bridge) error {
				close(started)
				<-release // the answer is still crossing the tunnel
				rows = []tgbridge.Dialog{{Peer: tgbridge.Peer{Kind: "user", ID: 1001, Title: "account one's chat"}}}
				return nil
			})
			if err == nil {
				h.store().MergeDialogs(rows)
			}
			done <- err
		}()
		<-started

		rec := httptest.NewRecorder()
		s.handleTelAuth(rec, telJSONReq("/api/tel/auth", `{"action":"logout"}`))
		if rec.Code != 200 {
			t.Fatalf("round %d: logout status %d: %s", round, rec.Code, rec.Body.String())
		}
		close(release)
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("round %d: a call that outlived the sign-out reported success", round)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the call never returned", round)
		}
		if ds := h.store().Dialogs(); len(ds) != 0 {
			t.Fatalf("round %d: the next account starts with %d of the previous account's chats", round, len(ds))
		}
		// And from disk, which is what a restart reads.
		if ds := loadTelCache(h.dir).Dialogs(); len(ds) != 0 {
			t.Fatalf("round %d: the saved cache holds %d of the previous account's chats", round, len(ds))
		}
	}
}
