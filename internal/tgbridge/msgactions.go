package tgbridge

import (
	"context"

	"github.com/gotd/td/tg"
)

// ChannelBased says whether a conversation numbers its messages itself -- a
// channel or supergroup -- rather than in the account's own box (private
// chats, bots, basic groups). Deleting works differently for the two.
func (p Peer) ChannelBased() bool {
	switch p.Kind {
	case PeerChannel, PeerForum:
		return true
	case PeerGroup:
		return p.AccessHash != 0
	}
	return false
}

// EditText replaces the text of one of this account's messages. Telegram
// refuses after its edit window, or for a message that is not ours.
func (b *Bridge) EditText(ctx context.Context, peer Peer, msgID int, text string) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	req := &tg.MessagesEditMessageRequest{Peer: peer.inputPeer(), ID: msgID}
	req.SetMessage(text)
	_, err = api.MessagesEditMessage(ctx, req)
	return err
}

// DeleteMessages deletes messages. In a channel or supergroup a delete is
// always for everyone; in the account's own box, revoke deletes for the other
// side too, and without it only for this account.
func (b *Bridge) DeleteMessages(ctx context.Context, peer Peer, ids []int, revoke bool) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	if peer.ChannelBased() {
		_, err = api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: peer.ID, AccessHash: peer.AccessHash}, ID: ids,
		})
		return err
	}
	_, err = api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{Revoke: revoke, ID: ids})
	return err
}

// ForwardOptions are Telegram's own forward choices. Each is a flag on the
// same call, so none costs a query.
type ForwardOptions struct {
	// HideSender sends a plain copy, without "Forwarded from".
	HideSender bool `json:"hideSender"`
	// HideCaptions drops the text under media; it implies HideSender.
	HideCaptions bool `json:"hideCaptions"`
	// Silent delivers without a notification sound.
	Silent bool `json:"silent"`
}

// ForwardMessages forwards messages to another conversation, under Telegram's
// "Forwarded from" header. It returns the copies as Telegram's answer carries
// them, so the target needs no read-back; the answer may also name none.
func (b *Bridge) ForwardMessages(ctx context.Context, from Peer, ids []int, randomIDs []int64, to Peer, opt ForwardOptions) ([]Message, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	ups, err := api.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		FromPeer: from.inputPeer(), ID: ids, RandomID: randomIDs, ToPeer: to.inputPeer(),
		DropAuthor: opt.HideSender || opt.HideCaptions, DropMediaCaptions: opt.HideCaptions, Silent: opt.Silent,
	})
	if err != nil {
		return nil, err
	}
	return newMessagesIn(ups), nil
}

// newMessagesIn lifts the new messages out of a send's answer.
func newMessagesIn(ups tg.UpdatesClass) []Message {
	var list []tg.UpdateClass
	var users []tg.UserClass
	var chats []tg.ChatClass
	switch u := ups.(type) {
	case *tg.Updates:
		list, users, chats = u.Updates, u.Users, u.Chats
	case *tg.UpdatesCombined:
		list, users, chats = u.Updates, u.Users, u.Chats
	default:
		return nil
	}
	names := senderNames(users, chats)
	var out []Message
	for _, up := range list {
		var m tg.MessageClass
		switch t := up.(type) {
		case *tg.UpdateNewMessage:
			m = t.Message
		case *tg.UpdateNewChannelMessage:
			m = t.Message
		default:
			continue
		}
		if msg, ok := messageFrom(m, names); ok {
			out = append(out, msg)
		}
	}
	return out
}
