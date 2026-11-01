package tgbridge

import (
	"context"
	"errors"

	"github.com/gotd/td/tg"
)

// ErrNoDirectMessages means the channel does not take direct messages, or
// Telegram did not name the conversation that carries them.
var ErrNoDirectMessages = errors.New("tgbridge: channel takes no direct messages")

// DirectMessages returns the conversation for writing to a channel's admins
// (Telegram's "monoforum"). One call: it arrives with the channel's full info.
// Nothing needs joining -- a message sent to it reaches the admins.
func (b *Bridge) DirectMessages(ctx context.Context, channel Peer) (Peer, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return Peer{}, err
	}
	if channel.DirectID == 0 {
		return Peer{}, ErrNoDirectMessages
	}
	full, err := api.ChannelsGetFullChannel(ctx, &tg.InputChannel{
		ChannelID: channel.ID, AccessHash: channel.AccessHash,
	})
	if err != nil {
		return Peer{}, err
	}
	if p, ok := directFrom(full.Chats, channel.DirectID); ok {
		return p, nil
	}
	return Peer{}, ErrNoDirectMessages
}

func directFrom(chats []tg.ChatClass, id int64) (Peer, bool) {
	for _, c := range chats {
		if ch, ok := c.(*tg.Channel); ok && ch.ID == id && ch.Monoforum {
			return peerFromChannel(ch), true
		}
	}
	return Peer{}, false
}
