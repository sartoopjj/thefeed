package tgbridge

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func directChannel() *tg.Channel {
	c := &tg.Channel{ID: 100, AccessHash: 1, Title: "News", Broadcast: true, BroadcastMessagesAllowed: true, Photo: &tg.ChatPhotoEmpty{}}
	c.SetLinkedMonoforumID(200)
	return c
}

func directMonoforum(stars int64) *tg.Channel {
	// Not joined: Telegram marks it left. Megagroup and forum, like Telegram's.
	m := &tg.Channel{ID: 200, AccessHash: 2, Title: "News", Megagroup: true, Forum: true, Left: true, Monoforum: true, Photo: &tg.ChatPhotoEmpty{}}
	m.SetLinkedMonoforumID(100)
	if stars > 0 {
		m.SetSendPaidMessagesStars(stars)
	}
	return m
}

// The flag and the id arrive with the channel object already fetched, so
// showing the button costs nothing.
func TestChannelSaysItTakesDirectMessages(t *testing.T) {
	if p := peerFromChannel(directChannel()); p.DirectID != 200 || p.Direct {
		t.Errorf("channel: %+v", p)
	}
	plain := &tg.Channel{ID: 101, Broadcast: true}
	if p := peerFromChannel(plain); p.DirectID != 0 {
		t.Errorf("a channel without direct messages offers them: %+v", p)
	}
}

// The monoforum is written to without joining, so "left" must not make it
// read-only, and it is one conversation, not a forum of topics.
func TestDirectConversationIsWritable(t *testing.T) {
	p := peerFromChannel(directMonoforum(0))
	if p.Kind != PeerGroup || !p.Direct || p.ReadOnly || p.AccessHash != 2 {
		t.Errorf("monoforum: %+v", p)
	}
	if _, ok := p.inputPeer().(*tg.InputPeerChannel); !ok {
		t.Errorf("addressed as %T", p.inputPeer())
	}
	if paid := peerFromChannel(directMonoforum(50)); paid.PaidStars != 50 {
		t.Errorf("price lost: %+v", paid)
	}
}

func TestDirectMessagesIsOneCall(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.ChannelsGetFullChannelRequest)
		if !ok {
			t.Fatalf("sent %T", in)
		}
		ch, ok := req.Channel.(*tg.InputChannel)
		if !ok || ch.ChannelID != 100 || ch.AccessHash != 1 {
			t.Errorf("asked about %#v", req.Channel)
		}
	}).ThenResult(&tg.MessagesChatFull{
		FullChat: &tg.ChannelFull{ID: 100, ChatPhoto: &tg.PhotoEmpty{}},
		Chats:    []tg.ChatClass{directChannel(), directMonoforum(0)},
	})
	p, err := b.DirectMessages(t.Context(), peerFromChannel(directChannel()))
	if err != nil || p.ID != 200 || !p.Direct {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestDirectMessagesRefusedWithoutAMonoforum(t *testing.T) {
	// No direct messages: refused without a call (no expectation set).
	b, _ := mockBridge(t)
	if _, err := b.DirectMessages(t.Context(), Peer{Kind: PeerChannel, ID: 101}); err != ErrNoDirectMessages {
		t.Errorf("no direct messages: %v", err)
	}
	if _, ok := directFrom([]tg.ChatClass{directChannel()}, 200); ok {
		t.Error("found a monoforum that was not in the answer")
	}
	notMono := directMonoforum(0)
	notMono.Monoforum = false
	if _, ok := directFrom([]tg.ChatClass{notMono}, 200); ok {
		t.Error("took an ordinary group for the monoforum")
	}
	if _, err := NewBridge().DirectMessages(t.Context(), peerFromChannel(directChannel())); err != ErrNotConnected {
		t.Errorf("not connected: %v", err)
	}
}
