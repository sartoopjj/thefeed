package tgbridge

import (
	"testing"

	"github.com/gotd/td/tg"
)

// A basic group always carries its member count, and the list costs nothing
// extra for it.
func TestMembersFromBasicGroup(t *testing.T) {
	p := peerFromChat(&tg.Chat{ID: 5, Title: "g", ParticipantsCount: 42})
	if p.Members != 42 {
		t.Fatalf("members = %d, want 42", p.Members)
	}
}

// A channel carries one only sometimes; absent stays 0 ("not known"), never a
// count of nobody.
func TestMembersFromChannel(t *testing.T) {
	c := &tg.Channel{ID: 9, AccessHash: 1, Title: "c", Broadcast: true}
	if p := peerFromChannel(c); p.Members != 0 {
		t.Fatalf("absent count became %d", p.Members)
	}
	c.SetParticipantsCount(12345)
	if p := peerFromChannel(c); p.Members != 12345 {
		t.Fatalf("members = %d, want 12345", p.Members)
	}
}

// The fallback reads the count from a channel's full info, and the
// description with it: the same answer, so never a second call.
func TestMembersFromFullChannel(t *testing.T) {
	full := &tg.ChannelFull{ID: 9, About: "News, every hour.\nhttps://example.org"}
	full.SetParticipantsCount(777)
	if got := infoFromFull(&tg.MessagesChatFull{FullChat: full}); got.Members != 777 || got.About != full.About {
		t.Fatalf("info = %+v", got)
	}
	// A channel that hides its count says nothing: 0, not an error.
	if got := infoFromFull(&tg.MessagesChatFull{FullChat: &tg.ChannelFull{ID: 9}}); got != (PeerInfo{}) {
		t.Fatalf("hidden count, no description = %+v", got)
	}
	if got := infoFromFull(&tg.MessagesChatFull{FullChat: &tg.ChatFull{ID: 5, About: "x"}}); got != (PeerInfo{}) {
		t.Fatalf("not a channel = %+v", got)
	}
}

// Who has a description to read: channels, supergroups, people and bots; not
// basic groups (theirs would be another call shape) nor direct messages.
func TestHasAbout(t *testing.T) {
	for _, c := range []struct {
		p    Peer
		want bool
	}{
		{Peer{Kind: PeerChannel, ID: 1, AccessHash: 1}, true},
		{Peer{Kind: PeerForum, ID: 1, AccessHash: 1}, true},
		{Peer{Kind: PeerGroup, ID: 1, AccessHash: 1}, true}, // supergroup
		{Peer{Kind: PeerUser, ID: 1, AccessHash: 1}, true},
		{Peer{Kind: PeerBot, ID: 1, AccessHash: 1}, true},
		{Peer{Kind: PeerGroup, ID: 1}, false}, // basic group
		{Peer{Kind: PeerChannel, ID: 1, AccessHash: 1, Direct: true}, false},
	} {
		if got := c.p.HasAbout(); got != c.want {
			t.Errorf("%+v: HasAbout %v, want %v", c.p, got, c.want)
		}
	}
}
