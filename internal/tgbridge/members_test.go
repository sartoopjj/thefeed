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

// The fallback reads the count from a channel's full info.
func TestMembersFromFullChannel(t *testing.T) {
	full := &tg.ChannelFull{ID: 9}
	full.SetParticipantsCount(777)
	if n := membersFromFull(&tg.MessagesChatFull{FullChat: full}); n != 777 {
		t.Fatalf("members = %d, want 777", n)
	}
	// A channel that hides its count says nothing: 0, not an error.
	if n := membersFromFull(&tg.MessagesChatFull{FullChat: &tg.ChannelFull{ID: 9}}); n != 0 {
		t.Fatalf("hidden count = %d", n)
	}
	if n := membersFromFull(&tg.MessagesChatFull{FullChat: &tg.ChatFull{ID: 5}}); n != 0 {
		t.Fatalf("not a channel = %d", n)
	}
}
