package tgbridge

import (
	"reflect"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestChannelBased(t *testing.T) {
	for _, c := range []struct {
		p    Peer
		want bool
	}{
		{Peer{Kind: PeerChannel, ID: 1, AccessHash: 1}, true},
		{Peer{Kind: PeerForum, ID: 1, AccessHash: 1}, true},
		{Peer{Kind: PeerGroup, ID: 1, AccessHash: 1}, true}, // supergroup
		{Peer{Kind: PeerGroup, ID: 1}, false},               // basic group
		{Peer{Kind: PeerUser, ID: 1, AccessHash: 1}, false},
		{Peer{Kind: PeerBot, ID: 1, AccessHash: 1}, false},
	} {
		if got := c.p.ChannelBased(); got != c.want {
			t.Errorf("%+v: %v", c.p, got)
		}
	}
}

func TestEditSendsTheNewText(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesEditMessageRequest)
		if !ok || req.ID != 42 || req.Message != "fixed" {
			t.Errorf("sent %#v", in)
		}
		if p, ok := req.Peer.(*tg.InputPeerUser); !ok || p.UserID != 8 {
			t.Errorf("peer %#v", req.Peer)
		}
	}).ThenResult(&tg.Updates{})
	if err := b.EditText(t.Context(), Peer{Kind: PeerUser, ID: 8, AccessHash: 9}, 42, "fixed"); err != nil {
		t.Fatal(err)
	}
}

// The account's box and a channel delete by different methods.
func TestDeleteUsesTheRightMethod(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesDeleteMessagesRequest)
		if !ok || !req.Revoke || !reflect.DeepEqual(req.ID, []int{5}) {
			t.Errorf("box delete sent %#v", in)
		}
	}).ThenResult(&tg.MessagesAffectedMessages{})
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesDeleteMessagesRequest)
		if !ok || req.Revoke {
			t.Errorf("delete-for-me sent %#v", in)
		}
	}).ThenResult(&tg.MessagesAffectedMessages{})
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.ChannelsDeleteMessagesRequest)
		if !ok || !reflect.DeepEqual(req.ID, []int{7}) {
			t.Fatalf("channel delete sent %#v", in)
		}
		if ch, ok := req.Channel.(*tg.InputChannel); !ok || ch.ChannelID != 30 || ch.AccessHash != 3 {
			t.Errorf("channel %#v", req.Channel)
		}
	}).ThenResult(&tg.MessagesAffectedMessages{})
	ctx := t.Context()
	if err := b.DeleteMessages(ctx, Peer{Kind: PeerUser, ID: 8, AccessHash: 9}, []int{5}, true); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteMessages(ctx, Peer{Kind: PeerGroup, ID: 20}, []int{6}, false); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteMessages(ctx, Peer{Kind: PeerGroup, ID: 30, AccessHash: 3}, []int{7}, false); err != nil {
		t.Fatal(err)
	}
}

func TestEditAndDeleteNeedAConnection(t *testing.T) {
	b := NewBridge()
	if err := b.EditText(t.Context(), Peer{Kind: PeerUser, ID: 1}, 1, "x"); err != ErrNotConnected {
		t.Errorf("edit: %v", err)
	}
	if err := b.DeleteMessages(t.Context(), Peer{Kind: PeerUser, ID: 1}, []int{1}, true); err != ErrNotConnected {
		t.Errorf("delete: %v", err)
	}
}

// Deleting a conversation: our box deletes history (revoke: for both sides), a
// channel or supergroup is left, direct messages need no call at all.
func TestDeleteChatUsesTheRightMethod(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesDeleteHistoryRequest)
		if !ok || !req.Revoke {
			t.Errorf("private chat sent %#v", in)
		}
	}).ThenResult(&tg.MessagesAffectedHistory{})
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.ChannelsLeaveChannelRequest)
		if !ok {
			t.Fatalf("supergroup sent %T", in)
		}
		if ch, ok := req.Channel.(*tg.InputChannel); !ok || ch.ChannelID != 30 {
			t.Errorf("left %#v", req.Channel)
		}
	}).ThenResult(&tg.Updates{})
	ctx := t.Context()
	if err := b.DeleteChat(ctx, Peer{Kind: PeerUser, ID: 8, AccessHash: 9}, true); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteChat(ctx, Peer{Kind: PeerGroup, ID: 30, AccessHash: 3}, false); err != nil {
		t.Fatal(err)
	}
	// No expectation left: a call here fails the test.
	if err := b.DeleteChat(ctx, Peer{Kind: PeerGroup, ID: 200, AccessHash: 2, Direct: true}, false); err != nil {
		t.Fatal(err)
	}
}
