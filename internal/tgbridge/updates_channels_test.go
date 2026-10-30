package tgbridge

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// A channel post never appears in a difference's NewMessages: that list is the
// account's own message box (private chats and basic groups). Channels report
// through OtherUpdates instead, so dropping that list meant no channel ever
// changed through a difference -- which is why a channel joined on another
// device never showed up here.
func TestOtherUpdatesCarriesChannelPosts(t *testing.T) {
	chats := []tg.ChatClass{&tg.Channel{ID: 7, AccessHash: 42, Title: "News Channel"}}
	ups := []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: &tg.Message{
			ID: 101, PeerID: &tg.PeerChannel{ChannelID: 7}, Message: "a post",
		}},
		// Not a message, and not a channel we have to look up: it is our own
		// read mark, which rides in InReads.
		&tg.UpdateReadChannelInbox{ChannelID: 7},
	}
	h := otherUpdates(ups, nil, chats, NotifyDefaults{})
	msgs, chans := h.Msgs, h.Channels
	if len(msgs) != 1 {
		t.Fatalf("want the channel post, got %d messages", len(msgs))
	}
	if len(chans) != 0 {
		t.Errorf("no updateChannel here, so nothing to re-check: %+v", chans)
	}

	groups := groupByPeer(msgs, nil, chats)
	if len(groups) != 1 {
		t.Fatalf("want one conversation, got %d", len(groups))
	}
	if groups[0].Peer.ID != 7 || groups[0].Peer.Kind != PeerChannel {
		t.Errorf("peer = %+v, want channel 7", groups[0].Peer)
	}
	if groups[0].Peer.AccessHash != 42 {
		t.Errorf("access hash lost: %+v — the channel would be unaddressable", groups[0].Peer)
	}
	if len(groups[0].Messages) != 1 || groups[0].Messages[0].ID != 101 {
		t.Errorf("messages = %+v", groups[0].Messages)
	}
}

// Joining a channel that has not posted since is announced as updateChannel
// with no message at all. It has to be reported so the caller can notice a
// channel it has never seen -- the only thing worth fetching the whole list
// for.
func TestOtherUpdatesReportsJoinedChannels(t *testing.T) {
	chats := []tg.ChatClass{&tg.Channel{ID: 9, AccessHash: 5, Title: "New One"}}
	ups := []tg.UpdateClass{
		&tg.UpdateChannel{ChannelID: 9},
		&tg.UpdateChannel{ChannelID: 9}, // repeated: reported once
	}
	h := otherUpdates(ups, nil, chats, NotifyDefaults{})
	msgs, chans := h.Msgs, h.Channels
	if len(msgs) != 0 {
		t.Errorf("no posts here, got %d", len(msgs))
	}
	if len(chans) != 1 {
		t.Fatalf("want the channel reported once, got %+v", chans)
	}
	if chans[0].ID != 9 || chans[0].AccessHash != 5 {
		t.Errorf("peer = %+v, want channel 9 with its hash", chans[0])
	}
}

// A channel we were told about but cannot address is worse than silence: it
// would send the caller off to rebuild the list on every poll for ever.
func TestOtherUpdatesSkipsUnresolvableChannels(t *testing.T) {
	chans := otherUpdates([]tg.UpdateClass{&tg.UpdateChannel{ChannelID: 11}}, nil, nil, NotifyDefaults{}).Channels
	if len(chans) != 0 {
		t.Fatalf("a channel with no entity must be skipped, got %+v", chans)
	}
}

// An edit keeps the message id, so it replaces the text rather than arriving
// as a second message.
func TestOtherUpdatesCarriesEdits(t *testing.T) {
	chats := []tg.ChatClass{&tg.Channel{ID: 7, AccessHash: 42}}
	msgs := otherUpdates([]tg.UpdateClass{
		&tg.UpdateEditChannelMessage{Message: &tg.Message{
			ID: 101, PeerID: &tg.PeerChannel{ChannelID: 7}, Message: "fixed",
		}},
	}, nil, chats, NotifyDefaults{}).Msgs
	groups := groupByPeer(msgs, nil, chats)
	if len(groups) != 1 || len(groups[0].Messages) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].Messages[0].Text != "fixed" {
		t.Errorf("text = %q, want the edited text", groups[0].Messages[0].Text)
	}
}

// The second tick. "The other side read up to here" arrives beside the
// messages, not with them, so dropping OtherUpdates meant a message read after
// the short post-send polling window stayed on one tick for ever.
func TestOtherUpdatesCarriesReadReceipts(t *testing.T) {
	users := []tg.UserClass{&tg.User{ID: 3, AccessHash: 8, FirstName: "Ada"}}
	chats := []tg.ChatClass{&tg.Channel{ID: 7, AccessHash: 42}}
	reads := otherUpdates([]tg.UpdateClass{
		&tg.UpdateReadHistoryOutbox{Peer: &tg.PeerUser{UserID: 3}, MaxID: 55},
		&tg.UpdateReadChannelOutbox{ChannelID: 7, MaxID: 90},
		// Someone reading THEIR OWN inbox says nothing about our messages.
		&tg.UpdateReadHistoryInbox{Peer: &tg.PeerUser{UserID: 3}, MaxID: 999},
	}, users, chats, NotifyDefaults{}).Reads
	if len(reads) != 2 {
		t.Fatalf("want both outbox marks, got %+v", reads)
	}
	if reads[0].Peer.ID != 3 || reads[0].MaxID != 55 {
		t.Errorf("private read = %+v", reads[0])
	}
	if reads[1].Peer.ID != 7 || reads[1].MaxID != 90 {
		t.Errorf("channel read = %+v", reads[1])
	}
	if reads[0].Peer.AccessHash != 8 {
		t.Errorf("access hash lost: %+v", reads[0].Peer)
	}
}

// A channel left on another device. Telegram says so in the entity, so the row
// can go without re-reading the whole conversation list -- and it has to go,
// or a channel the user left stays listed here for ever.
func TestOtherUpdatesReportsLeftChannels(t *testing.T) {
	for _, tc := range []struct {
		name string
		chat tg.ChatClass
	}{
		{"left", &tg.Channel{ID: 9, AccessHash: 5, Title: "Left Channel", Left: true}},
		{"removed", &tg.ChannelForbidden{ID: 9, AccessHash: 5, Title: "Left Channel"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := otherUpdates(
				[]tg.UpdateClass{&tg.UpdateChannel{ChannelID: 9}}, nil, []tg.ChatClass{tc.chat}, NotifyDefaults{})
			chans, gone := h.Channels, h.Gone
			if len(gone) != 1 || gone[0].ID != 9 {
				t.Fatalf("want the channel reported as gone, got %+v", gone)
			}
			// Never both: a channel being left must not also trigger the
			// expensive "we have never seen this one" list rebuild.
			if len(chans) != 0 {
				t.Errorf("leaving must not ask for a resync: %+v", chans)
			}
		})
	}
}

// A channel we are still in must not be dropped because something else about
// it changed -- a rename or a new photo also arrives as updateChannel.
func TestOtherUpdatesKeepsChannelsStillJoined(t *testing.T) {
	gone := otherUpdates([]tg.UpdateClass{&tg.UpdateChannel{ChannelID: 9}}, nil,
		[]tg.ChatClass{&tg.Channel{ID: 9, AccessHash: 5, Title: "renamed"}}, NotifyDefaults{}).Gone
	if len(gone) != 0 {
		t.Fatalf("still a member, must not be dropped: %+v", gone)
	}
}

// Reading a chat on the phone. Telegram reports it as an inbox read mark with
// the number of messages it still considers unread, and that update is the ONLY
// thing in a difference that can bring an unread badge down -- a difference
// carries messages and marks, never conversation rows. Dropping it is why
// several channels marked read on another device still showed new messages
// here.
func TestOtherUpdatesCarriesOurOwnReads(t *testing.T) {
	users := []tg.UserClass{&tg.User{ID: 3, AccessHash: 8, FirstName: "Ada"}}
	chats := []tg.ChatClass{&tg.Channel{ID: 7, AccessHash: 42}}
	in := otherUpdates([]tg.UpdateClass{
		&tg.UpdateReadHistoryInbox{Peer: &tg.PeerUser{UserID: 3}, MaxID: 55, StillUnreadCount: 0},
		&tg.UpdateReadChannelInbox{ChannelID: 7, MaxID: 90, StillUnreadCount: 4},
		// The other side reading OUR messages is the second tick, not our mark.
		&tg.UpdateReadHistoryOutbox{Peer: &tg.PeerUser{UserID: 3}, MaxID: 999},
	}, users, chats, NotifyDefaults{}).InReads
	if len(in) != 2 {
		t.Fatalf("want both of our own read marks, got %+v", in)
	}
	if in[0].Peer.ID != 3 || in[0].MaxID != 55 || in[0].Unread != 0 {
		t.Errorf("private read = %+v, want up to 55 with nothing left", in[0])
	}
	if in[0].Peer.AccessHash != 8 {
		t.Errorf("access hash lost: %+v", in[0].Peer)
	}
	// Zero is an answer here, not a missing field: it is how a badge clears.
	if in[1].Peer.ID != 7 || in[1].MaxID != 90 || in[1].Unread != 4 {
		t.Errorf("channel read = %+v, want up to 90 with 4 left", in[1])
	}
}

// A forum topic read says nothing about the rest of the forum, so it must not
// be taken for a whole-conversation read: that would clear a badge covering
// topics the user has not looked at.
func TestOtherUpdatesSkipsTopicReads(t *testing.T) {
	users := []tg.UserClass{&tg.User{ID: 3, AccessHash: 8}}
	up := &tg.UpdateReadHistoryInbox{Peer: &tg.PeerUser{UserID: 3}, MaxID: 55}
	up.SetTopMsgID(12)
	if in := otherUpdates([]tg.UpdateClass{up}, users, nil, NotifyDefaults{}).InReads; len(in) != 0 {
		t.Fatalf("a topic read must not pass as a chat read: %+v", in)
	}
}

// A read mark for a peer we cannot address is unusable: there is no row to
// apply it to, and inventing one would put an unaddressable conversation in
// the list.
func TestOtherUpdatesSkipsUnresolvableReads(t *testing.T) {
	in := otherUpdates([]tg.UpdateClass{
		&tg.UpdateReadHistoryInbox{Peer: &tg.PeerUser{UserID: 404}, MaxID: 5},
		&tg.UpdateReadChannelInbox{ChannelID: 404, MaxID: 5},
	}, nil, nil, NotifyDefaults{}).InReads
	if len(in) != 0 {
		t.Fatalf("want nothing for peers with no entity, got %+v", in)
	}
}

// Muting a channel on the phone. Telegram reports it as a per-peer notify
// settings update, and a difference carries no conversation rows, so without
// this the channel kept ringing here until the whole list was re-read.
func TestOtherUpdatesCarriesMutes(t *testing.T) {
	users := []tg.UserClass{&tg.User{ID: 3, AccessHash: 8}}
	chats := []tg.ChatClass{&tg.Channel{ID: 7, AccessHash: 42}}
	muted := tg.PeerNotifySettings{}
	muted.SetMuteUntil(int(time.Now().Add(time.Hour).Unix()))
	unmuted := tg.PeerNotifySettings{}
	unmuted.SetMuteUntil(0)

	got := otherUpdates([]tg.UpdateClass{
		&tg.UpdateNotifySettings{
			Peer: &tg.NotifyPeer{Peer: &tg.PeerChannel{ChannelID: 7}}, NotifySettings: muted,
		},
		&tg.UpdateNotifySettings{
			Peer: &tg.NotifyPeer{Peer: &tg.PeerUser{UserID: 3}}, NotifySettings: unmuted,
		},
	}, users, chats, NotifyDefaults{}).Mutes
	if len(got) != 2 {
		t.Fatalf("want both settings, got %+v", got)
	}
	if got[0].Peer.ID != 7 || !got[0].Muted {
		t.Errorf("channel = %+v, want channel 7 muted", got[0])
	}
	// Unmuting is as real as muting: it has to travel too, or a channel
	// un-silenced on the phone stays silent here for ever.
	if got[1].Peer.ID != 3 || got[1].Muted {
		t.Errorf("user = %+v, want user 3 unmuted", got[1])
	}
}

// notifyUsers / notifyChats / notifyBroadcasts are the account-wide DEFAULTS
// for a whole class of conversation. Applying one of those to every cached row
// would silence chats the user deliberately left audible.
func TestOtherUpdatesSkipsAccountWideNotifyDefaults(t *testing.T) {
	chats := []tg.ChatClass{&tg.Channel{ID: 7, AccessHash: 42}}
	on := tg.PeerNotifySettings{}
	on.SetMuteUntil(int(time.Now().Add(time.Hour).Unix()))
	for _, np := range []tg.NotifyPeerClass{
		&tg.NotifyBroadcasts{}, &tg.NotifyChats{}, &tg.NotifyUsers{},
	} {
		got := otherUpdates([]tg.UpdateClass{
			&tg.UpdateNotifySettings{Peer: np, NotifySettings: on},
		}, nil, chats, NotifyDefaults{}).Mutes
		if len(got) != 0 {
			t.Errorf("%T must not mute individual rows: %+v", np, got)
		}
	}
}

// Telegram expresses "no sound" two ways. A conversation silenced through the
// silent flag carries no mute-until at all, so reading only mute-until
// reported channels the user had silenced on their phone as audible here.
func TestMutedNowReadsBothFlags(t *testing.T) {
	silent := tg.PeerNotifySettings{}
	silent.SetSilent(true)
	if !mutedNow(silent, false) {
		t.Error("silent must count as muted")
	}
	forever := tg.PeerNotifySettings{}
	forever.SetMuteUntil(int(time.Now().Add(100 * time.Hour).Unix()))
	if !mutedNow(forever, false) {
		t.Error("muted until a future time must count as muted")
	}
	// An expired mute is not a mute: the conversation is audible again.
	expired := tg.PeerNotifySettings{}
	expired.SetMuteUntil(int(time.Now().Add(-time.Hour).Unix()))
	if mutedNow(expired, false) {
		t.Error("an expired mute must not silence the row")
	}
	if mutedNow(tg.PeerNotifySettings{}, false) {
		t.Error("no setting at all is not a mute")
	}
	// Explicitly not silent, no mute: audible.
	loud := tg.PeerNotifySettings{}
	loud.SetSilent(false)
	loud.SetMuteUntil(0)
	if mutedNow(loud, false) {
		t.Error("explicitly audible must stay audible")
	}
}

// The same for a private chat, which is where an edit is a correction to
// something said to you -- and the edit date is what lets the screen replace
// the copy it already holds.
func TestOtherUpdatesCarriesPrivateEdits(t *testing.T) {
	users := []tg.UserClass{&tg.User{ID: 3, AccessHash: 8, FirstName: "Ada"}}
	m := &tg.Message{ID: 41, PeerID: &tg.PeerUser{UserID: 3}, Message: "how are you"}
	m.SetEditDate(1700)
	msgs := otherUpdates([]tg.UpdateClass{&tg.UpdateEditMessage{Message: m}}, users, nil, NotifyDefaults{}).Msgs
	groups := groupByPeer(msgs, users, nil)
	if len(groups) != 1 || len(groups[0].Messages) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	got := groups[0].Messages[0]
	if got.ID != 41 || got.Text != "how are you" || got.EditDate != 1700 {
		t.Fatalf("edit arrived as %+v", got)
	}
}
