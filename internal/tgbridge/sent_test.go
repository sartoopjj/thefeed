package tgbridge

import (
	"testing"

	"github.com/gotd/td/tg"
)

// A send's answer carries the new message's id AND Telegram's time for it, in
// every shape the answer comes in. The time is what orders the list: the
// device's own clock is often wrong, and a chat written to from a phone set a
// day ahead sat on top of the list until the next list read.
func TestSentMessageCarriesTelegramsTime(t *testing.T) {
	newMsg := &tg.Message{ID: 41, Date: 1_800_000_100, PeerID: &tg.PeerUser{UserID: 1}}
	for name, tc := range map[string]struct {
		ups      tg.UpdatesClass
		id, date int
	}{
		"short sent (private chat)": {&tg.UpdateShortSentMessage{ID: 40, Date: 1_800_000_000}, 40, 1_800_000_000},
		"updates with the message": {&tg.Updates{Date: 1_800_000_200, Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: 41, RandomID: 7}, &tg.UpdateNewMessage{Message: newMsg}}}, 41, 1_800_000_100},
		// Someone else's message rides along in the same answer, before and
		// after ours: ours is the one with our random id, and its time.
		"a message from them alongside": {&tg.Updates{Date: 1_800_000_200, Updates: []tg.UpdateClass{
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 39, Date: 1_700_000_000, PeerID: &tg.PeerUser{UserID: 1}}},
			&tg.UpdateMessageID{ID: 41, RandomID: 7},
			&tg.UpdateNewMessage{Message: newMsg},
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 42, Date: 1_900_000_000, PeerID: &tg.PeerUser{UserID: 1}}}}}, 41, 1_800_000_100},
		// No id update at all: our own (outgoing) message, not theirs.
		"no id update": {&tg.Updates{Date: 1_800_000_200, Updates: []tg.UpdateClass{
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 50, Date: 1_700_000_000, PeerID: &tg.PeerUser{UserID: 1}}},
			&tg.UpdateNewMessage{Message: &tg.Message{ID: 51, Out: true, Date: 1_800_000_050, PeerID: &tg.PeerUser{UserID: 1}}}}}, 51, 1_800_000_050},
		"channel message": {&tg.Updates{Date: 1_800_000_200, Updates: []tg.UpdateClass{
			&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 9, Date: 1_800_000_150, PeerID: &tg.PeerChannel{ChannelID: 2}}}}}, 9, 1_800_000_150},
		// Only the id: the container's own time is Telegram's clock too.
		"id only":  {&tg.Updates{Date: 1_800_000_300, Updates: []tg.UpdateClass{&tg.UpdateMessageID{ID: 42}}}, 42, 1_800_000_300},
		"combined": {&tg.UpdatesCombined{Date: 1_800_000_400, Updates: []tg.UpdateClass{&tg.UpdateMessageID{ID: 43}}}, 43, 1_800_000_400},
		"short":    {&tg.UpdateShort{Date: 1_800_000_500, Update: &tg.UpdateMessageID{ID: 44}}, 44, 1_800_000_500},
		// Says nothing: neither is known, and the caller reads it back.
		"bare": {&tg.UpdatesTooLong{}, 0, 0},
	} {
		got := sentMessage(tc.ups, 7)
		if got.ID != tc.id || got.Date != tc.date {
			t.Errorf("%s: got id %d date %d, want id %d date %d", name, got.ID, got.Date, tc.id, tc.date)
		}
	}
}

// Telegram lists pinned conversations first, in the user's pin order. That
// order is kept as PinOrder, so the list can show it: sorted by time instead,
// the pins came out in a different order than on the phone. No extra call --
// the order is already in the answer.
func TestDialogsKeepPinOrder(t *testing.T) {
	dlg := func(id int64, pinned bool) *tg.Dialog {
		return &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: id}, Pinned: pinned, TopMessage: int(id)}
	}
	ch := func(id int64) *tg.Channel { return &tg.Channel{ID: id, AccessHash: 1, Title: "c", Broadcast: true} }
	res := &tg.MessagesDialogs{
		// Pinned as 3, then 1; then an ordinary chat.
		Dialogs: []tg.DialogClass{dlg(3, true), dlg(1, true), dlg(2, false)},
		Chats:   []tg.ChatClass{ch(1), ch(2), ch(3)},
	}
	got := dialogsFrom(res, NotifyDefaults{})
	order := map[int64]int{}
	for _, d := range got {
		order[d.Peer.ID] = d.PinOrder
	}
	if order[3] != 1 || order[1] != 2 || order[2] != 0 {
		t.Fatalf("pin order = %v, want 3→1, 1→2, 2→0", order)
	}
}
