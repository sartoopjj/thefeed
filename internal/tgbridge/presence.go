package tgbridge

import (
	"context"

	"github.com/gotd/td/tg"
)

// Presence: Telegram's API terms forbid hiding online status or typing, so the
// client reports both as any Telegram app does. Each is one small call.

// SetOnline reports the user online while they are in the Telegram section and
// offline when they leave it. Online lapses on Telegram's side unless renewed.
func (b *Bridge) SetOnline(ctx context.Context, online bool) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	_, err = api.AccountUpdateStatus(ctx, !online)
	return err
}

// SetTyping shows "typing…" to the other side for a few seconds. Telegram
// clears it by itself, and when the message is sent.
func (b *Bridge) SetTyping(ctx context.Context, peer Peer) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	_, err = api.MessagesSetTyping(ctx, &tg.MessagesSetTypingRequest{
		Peer: peer.inputPeer(), Action: &tg.SendMessageTypingAction{},
	})
	return err
}
