package tgbridge

import (
	"reflect"
	"testing"

	"github.com/gotd/td/tg"
)

// Both kinds of delete arrive in a difference, and both must reach the caller:
// the account's own box by id alone, a channel's with the channel named.
func TestDeletesAreHarvested(t *testing.T) {
	h := otherUpdates([]tg.UpdateClass{
		&tg.UpdateDeleteMessages{Messages: []int{4, 5}},
		&tg.UpdateDeleteChannelMessages{ChannelID: 77, Messages: []int{9}},
		&tg.UpdateDeleteMessages{Messages: nil},
	}, nil, nil, NotifyDefaults{})
	want := []Deletion{{IDs: []int{4, 5}}, {ChannelID: 77, IDs: []int{9}}}
	if !reflect.DeepEqual(h.Deleted, want) {
		t.Fatalf("got %+v, want %+v", h.Deleted, want)
	}
}

// A self-destructing message carries when it goes, so it can be dropped
// locally even if no delete update arrives.
func TestSelfDestructingMessageCarriesItsEnd(t *testing.T) {
	msg := &tg.Message{ID: 3, Date: 1000, Message: "gone soon"}
	msg.SetTTLPeriod(60)
	got, ok := messageFrom(msg, nil)
	if !ok || got.Expires != 1060 {
		t.Fatalf("expires = %d", got.Expires)
	}
	plain, _ := messageFrom(&tg.Message{ID: 4, Date: 1000, Message: "stays"}, nil)
	if plain.Expires != 0 {
		t.Fatalf("an ordinary message expires at %d", plain.Expires)
	}
}

// The deletes a difference carries reach the caller, from a whole difference
// and from a partial one alike.
func TestChangesCarryDeletes(t *testing.T) {
	ups := []tg.UpdateClass{&tg.UpdateDeleteChannelMessages{ChannelID: 77, Messages: []int{9}}}
	for name, diff := range map[string]tg.UpdatesDifferenceClass{
		"difference": &tg.UpdatesDifference{OtherUpdates: ups, State: tg.UpdatesState{Pts: 2, Date: 2}},
		"slice":      &tg.UpdatesDifferenceSlice{OtherUpdates: ups, IntermediateState: tg.UpdatesState{Pts: 2, Date: 2}},
	} {
		b, mock := mockBridge(t)
		b.notifyOK = true
		mock.Expect().ThenResult(diff)
		ch, err := b.Changes(t.Context(), UpdateState{Pts: 1, Date: 1})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := []Deletion{{ChannelID: 77, IDs: []int{9}}}; !reflect.DeepEqual(ch.Deleted, want) {
			t.Errorf("%s: deleted %+v", name, ch.Deleted)
		}
	}
}
