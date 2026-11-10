package tgbridge

import (
	"reflect"
	"testing"

	"github.com/gotd/td/tg"
)

// A channel or person can hold several usernames (collectible ones among
// them). Telegram then leaves the plain username empty and lists them all
// instead, so a channel like that showed no @id at all. The first active one
// is the main one; the other active ones are kept too. No extra call: they
// come in the same entity.
func TestUsernamesFromTheList(t *testing.T) {
	list := []tg.Username{
		{Username: "durov", Active: true, Editable: true},
		{Username: "rove", Active: true},
		{Username: "old", Active: false}, // not in use
		{Username: "paul", Active: true},
	}
	c := &tg.Channel{ID: 1, AccessHash: 1, Title: "c", Broadcast: true}
	c.SetUsernames(list)
	p := peerFromChannel(c)
	if p.Username != "durov" || !reflect.DeepEqual(p.Usernames, []string{"rove", "paul"}) {
		t.Fatalf("channel: %q %v", p.Username, p.Usernames)
	}
	u := &tg.User{ID: 2, FirstName: "x"}
	u.SetUsernames(list)
	if p := peerFromUser(u); p.Username != "durov" || len(p.Usernames) != 2 {
		t.Fatalf("user: %q %v", p.Username, p.Usernames)
	}
	// A plain username alone, as before: nothing else.
	plain := &tg.Channel{ID: 3, AccessHash: 1, Title: "c", Username: "news"}
	if p := peerFromChannel(plain); p.Username != "news" || p.Usernames != nil {
		t.Fatalf("plain: %q %v", p.Username, p.Usernames)
	}
	// Both: the plain one is the main one and is not repeated.
	both := &tg.Channel{ID: 4, AccessHash: 1, Title: "c", Username: "rove"}
	both.SetUsernames(list)
	if p := peerFromChannel(both); p.Username != "rove" || !reflect.DeepEqual(p.Usernames, []string{"durov", "paul"}) {
		t.Fatalf("both: %q %v", p.Username, p.Usernames)
	}
	// Only inactive ones: none to show.
	gone := &tg.Channel{ID: 5, AccessHash: 1, Title: "c"}
	gone.SetUsernames([]tg.Username{{Username: "old"}})
	if p := peerFromChannel(gone); p.Username != "" || p.Usernames != nil {
		t.Fatalf("inactive: %q %v", p.Username, p.Usernames)
	}
}
