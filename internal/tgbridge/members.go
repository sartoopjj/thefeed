package tgbridge

import (
	"context"
	"errors"

	"github.com/gotd/td/tg"
)

// MemberCount reads a channel's or supergroup's member count from its full
// info: one call, for when the entity came without one. 0 means the channel
// does not show it.
func (b *Bridge) MemberCount(ctx context.Context, peer Peer) (int, error) {
	if !peer.ChannelBased() || peer.Direct {
		return 0, errors.New("tgbridge: member count is read for channels only")
	}
	_, api, err := b.snapshot()
	if err != nil {
		return 0, err
	}
	full, err := api.ChannelsGetFullChannel(ctx, &tg.InputChannel{
		ChannelID: peer.ID, AccessHash: peer.AccessHash,
	})
	if err != nil {
		return 0, err
	}
	return membersFromFull(full), nil
}

func membersFromFull(full *tg.MessagesChatFull) int {
	if cf, ok := full.FullChat.(*tg.ChannelFull); ok {
		n, _ := cf.GetParticipantsCount()
		return n
	}
	return 0
}
