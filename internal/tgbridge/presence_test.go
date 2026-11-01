package tgbridge

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestOnlineAndOfflineAreReported(t *testing.T) {
	b, mock := mockBridge(t)
	for _, offline := range []bool{false, true} {
		want := offline
		mock.ExpectFunc(func(in bin.Encoder) {
			req, ok := in.(*tg.AccountUpdateStatusRequest)
			if !ok || req.Offline != want {
				t.Errorf("sent %#v, want offline=%v", in, want)
			}
		}).ThenTrue()
	}
	if err := b.SetOnline(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := b.SetOnline(t.Context(), false); err != nil {
		t.Fatal(err)
	}
}

func TestTypingGoesToTheConversation(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesSetTypingRequest)
		if !ok {
			t.Fatalf("sent %T", in)
		}
		p, ok := req.Peer.(*tg.InputPeerUser)
		if !ok || p.UserID != 8 || p.AccessHash != 9 {
			t.Errorf("typing in %#v", req.Peer)
		}
		if _, ok := req.Action.(*tg.SendMessageTypingAction); !ok {
			t.Errorf("action %#v", req.Action)
		}
	}).ThenTrue()
	if err := b.SetTyping(t.Context(), Peer{Kind: PeerUser, ID: 8, AccessHash: 9}); err != nil {
		t.Fatal(err)
	}
}

func TestPresenceNeedsAConnection(t *testing.T) {
	b := NewBridge()
	if err := b.SetOnline(t.Context(), true); err != ErrNotConnected {
		t.Errorf("SetOnline: %v", err)
	}
	if err := b.SetTyping(t.Context(), Peer{Kind: PeerUser, ID: 1}); err != ErrNotConnected {
		t.Errorf("SetTyping: %v", err)
	}
}
