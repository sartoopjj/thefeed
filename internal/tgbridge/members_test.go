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
	if got := infoFromFull(&tg.MessagesChatFull{FullChat: &tg.ChannelFull{ID: 9}}); got != (ChannelInfo{}) {
		t.Fatalf("hidden count, no description = %+v", got)
	}
	if got := infoFromFull(&tg.MessagesChatFull{FullChat: &tg.ChatFull{ID: 5, About: "x"}}); got != (ChannelInfo{}) {
		t.Fatalf("not a channel = %+v", got)
	}
}
