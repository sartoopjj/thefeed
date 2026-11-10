package tgbridge

import (
	"context"
	"errors"

	"github.com/gotd/td/tg"
)

// ChannelInfo is what a channel's full info adds to its entity: the member
// count (0: the channel does not show it) and its description.
type ChannelInfo struct {
	Members int
	About   string
}

// ChannelInfo reads a channel's or supergroup's full info: one call, for a
// member count the entity came without or a description (the entity never
// has one).
func (b *Bridge) ChannelInfo(ctx context.Context, peer Peer) (ChannelInfo, error) {
	if !peer.ChannelBased() || peer.Direct {
		return ChannelInfo{}, errors.New("tgbridge: full info is read for channels only")
	}
	_, api, err := b.snapshot()
	if err != nil {
		return ChannelInfo{}, err
	}
	full, err := api.ChannelsGetFullChannel(ctx, &tg.InputChannel{
		ChannelID: peer.ID, AccessHash: peer.AccessHash,
	})
	if err != nil {
		return ChannelInfo{}, err
	}
	return infoFromFull(full), nil
}

func infoFromFull(full *tg.MessagesChatFull) ChannelInfo {
	if cf, ok := full.FullChat.(*tg.ChannelFull); ok {
		n, _ := cf.GetParticipantsCount()
		return ChannelInfo{Members: n, About: cf.About}
	}
	return ChannelInfo{}
}
