package tgbridge

import (
	"context"
	"errors"

	"github.com/gotd/td/tg"
)

// PeerInfo is what a conversation's full info adds to its entity: a
// channel's member count (0: not shown, or not a channel) and the
// description -- a channel's or group's, or a person's or bot's bio.
// The entity never carries the description.
type PeerInfo struct {
	Members int
	About   string
}

// HasAbout says whether a conversation has a description to read: channels,
// groups (basic or super), people and bots. A channel's direct messages have
// none.
func (p Peer) HasAbout() bool {
	if p.Direct {
		return false
	}
	switch p.Kind {
	case PeerChannel, PeerForum, PeerGroup, PeerUser, PeerBot:
		return true
	}
	return false
}

// PeerInfo reads a conversation's full info: one call. For a channel it
// brings the member count the entity may have come without, and the
// description; for a basic group, its description (its count comes free with
// the list); for a person or bot, the bio.
func (b *Bridge) PeerInfo(ctx context.Context, peer Peer) (PeerInfo, error) {
	if !peer.HasAbout() {
		return PeerInfo{}, errors.New("tgbridge: no full info to read for this conversation")
	}
	_, api, err := b.snapshot()
	if err != nil {
		return PeerInfo{}, err
	}
	if peer.Kind == PeerUser || peer.Kind == PeerBot {
		full, err := api.UsersGetFullUser(ctx, &tg.InputUser{UserID: peer.ID, AccessHash: peer.AccessHash})
		if err != nil {
			return PeerInfo{}, err
		}
		return PeerInfo{About: full.FullUser.About}, nil
	}
	if !peer.ChannelBased() {
		full, err := api.MessagesGetFullChat(ctx, peer.ID)
		if err != nil {
			return PeerInfo{}, err
		}
		return infoFromFull(full), nil
	}
	full, err := api.ChannelsGetFullChannel(ctx, &tg.InputChannel{
		ChannelID: peer.ID, AccessHash: peer.AccessHash,
	})
	if err != nil {
		return PeerInfo{}, err
	}
	return infoFromFull(full), nil
}

func infoFromFull(full *tg.MessagesChatFull) PeerInfo {
	switch cf := full.FullChat.(type) {
	case *tg.ChannelFull:
		n, _ := cf.GetParticipantsCount()
		return PeerInfo{Members: n, About: cf.About}
	case *tg.ChatFull:
		return PeerInfo{About: cf.About}
	}
	return PeerInfo{}
}
