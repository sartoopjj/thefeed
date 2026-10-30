package web

import (
	"testing"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

func addr(b byte) [protocol.AddressSize]byte {
	var a [protocol.AddressSize]byte
	a[0] = b
	return a
}

// Unreadable ciphertext must still be acknowledged.
//
// The inbox slot is held by the envelope, not by whether we could open it.
// Anyone who knows an address can post a well-formed envelope full of noise;
// if the watermark only counted messages that decrypted, a few throwaway
// identities would fill the inbox and lock out every real sender for the full
// 72-hour retention.
func TestUnreadableEnvelopesStillRaiseTheAckWatermark(t *testing.T) {
	flooder, friend := addr(1), addr(2)
	msgs := []client.ChatIncoming{
		{From: friend, Seq: 4, Text: "hello"},
		{From: flooder, Seq: 1, Undecryptable: true},
		{From: flooder, Seq: 2, Undecryptable: true},
		{From: flooder, Seq: 3, Undecryptable: true},
	}

	got := ackWatermarks(msgs)
	fl := client.ChatAddressString(flooder)
	if got[fl] != 3 {
		t.Errorf("watermark for the flooder = %d, want 3 -- the garbage is never "+
			"acknowledged, so it holds the inbox until it expires", got[fl])
	}
	if fr := client.ChatAddressString(friend); got[fr] != 4 {
		t.Errorf("watermark for a real sender = %d, want 4", got[fr])
	}
}

// An inbox that is ENTIRELY unreadable is the actual flood, and the one case
// where nothing is stored and nothing is rendered -- so it is the case most
// easily left unacknowledged.
func TestAnInboxOfNothingButGarbageIsStillAcknowledged(t *testing.T) {
	a := addr(7)
	msgs := []client.ChatIncoming{
		{From: a, Seq: 9, Undecryptable: true},
		{From: a, Seq: 11, Undecryptable: true},
		{From: a, Seq: 10, Undecryptable: true},
	}
	if got := ackWatermarks(msgs)[client.ChatAddressString(a)]; got != 11 {
		t.Errorf("watermark = %d, want 11 (the highest seq, in any order)", got)
	}
}

func TestAckWatermarksOfNothing(t *testing.T) {
	if got := ackWatermarks(nil); len(got) != 0 {
		t.Errorf("got %v, want no watermarks", got)
	}
}
