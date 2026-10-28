package tgbridge

import (
	"context"

	"github.com/gotd/td/tg"
)

// Views and reactions move long after a post is written, and history only ever
// answers with messages. A post read in its first minute would otherwise keep
// the counts it had then for ever: no reactions, a handful of views.
//
// Both counters are fetched WITHOUT the message bodies -- getMessagesViews and
// getMessagesReactions carry only numbers -- so bringing a whole screen of
// posts up to date costs a fraction of re-reading the page over DNS.
type MessageStats struct {
	ID        int        `json:"id"`
	Views     int        `json:"views,omitempty"`
	Reactions []Reaction `json:"reactions,omitempty"`
}

// maxStatIDs is the backstop, not the budget. The caller asks about a
// screenful; this only stops a malformed request from turning into an answer
// that takes minutes to carry over DNS.
const maxStatIDs = 40

// MessageStatsFor re-reads the view and reaction counts for messages already
// held. Partial answers are normal and fine: a message Telegram says nothing
// about keeps the counts it has.
// what selects which counter to read. They are two different Telegram methods
// and therefore two round trips, so the caller asks for them separately: one
// progress bar per request, each able to say what it is fetching.
const (
	StatViews     = "views"
	StatReactions = "reactions"
)

func (b *Bridge) MessageStatsFor(ctx context.Context, peer Peer, ids []int, what string) ([]MessageStats, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > maxStatIDs {
		ids = ids[len(ids)-maxStatIDs:]
	}
	byID := make(map[int]*MessageStats, len(ids))
	order := make([]int, 0, len(ids))
	at := func(id int) *MessageStats {
		if s, ok := byID[id]; ok {
			return s
		}
		s := &MessageStats{ID: id}
		byID[id] = s
		order = append(order, id)
		return s
	}

	// Views, for the only thing that has them. Asking in a private chat buys
	// an error for the price of a DNS round trip, so it is not asked there --
	// reactions below still are, because those a private chat does have.
	var views *tg.MessagesMessageViews
	if what != StatReactions && (peer.Kind == PeerChannel || peer.Kind == PeerForum) {
		// Increment stays false: this is a read of a counter, not a claim that
		// the user just opened the post. The history fetch already told
		// Telegram what was read.
		views, err = api.MessagesGetMessagesViews(ctx, &tg.MessagesGetMessagesViewsRequest{
			Peer: peer.inputPeer(), ID: ids, Increment: false,
		})
		if err != nil {
			// Not worth failing the whole refresh for: reactions may still
			// come back, and a message keeps the count it already has.
			views = nil
		}
	}
	if views != nil {
		// The answer is positional, one entry per requested id.
		for i, v := range views.Views {
			if i >= len(ids) {
				break
			}
			if n, ok := v.GetViews(); ok && n > 0 {
				at(ids[i]).Views = n
			}
		}
	}

	if what != StatViews {
		ups, err := api.MessagesGetMessagesReactions(ctx, &tg.MessagesGetMessagesReactionsRequest{
			Peer: peer.inputPeer(), ID: ids,
		})
		if err != nil {
			if len(order) == 0 {
				return nil, err
			}
		} else {
			for id, rs := range reactionUpdates(ups) {
				at(id).Reactions = rs
			}
		}
	}

	out := make([]MessageStats, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// SendReaction sets or clears this account's reaction on one message and
// returns the message's new counts, so the UI can settle on what Telegram
// actually recorded rather than on what it guessed.
//
// An empty emoji removes the reaction, which is how Telegram models "tap the
// one you already chose".
func (b *Bridge) SendReaction(ctx context.Context, peer Peer, msgID int, emoji string) ([]Reaction, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	req := &tg.MessagesSendReactionRequest{Peer: peer.inputPeer(), MsgID: msgID}
	if emoji != "" {
		req.SetReaction([]tg.ReactionClass{&tg.ReactionEmoji{Emoticon: emoji}})
	}
	ups, err := api.MessagesSendReaction(ctx, req)
	if err != nil {
		return nil, err
	}
	if rs, ok := reactionUpdates(ups)[msgID]; ok {
		return rs, nil
	}
	return nil, nil
}

// reactionUpdates pulls per-message reaction counts out of an Updates answer.
// Both getMessagesReactions and sendReaction report this way.
func reactionUpdates(ups tg.UpdatesClass) map[int][]Reaction {
	out := map[int][]Reaction{}
	var list []tg.UpdateClass
	switch u := ups.(type) {
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	case *tg.UpdateShort:
		list = []tg.UpdateClass{u.Update}
	}
	for _, up := range list {
		if r, ok := up.(*tg.UpdateMessageReactions); ok {
			out[r.MsgID] = reactionCounts(r.Reactions)
		}
	}
	return out
}

// JoinChannel subscribes this account to a channel.
//
// Never implied by opening one: reading a channel through a link and joining
// it are different acts, and only the second one tells Telegram -- and anyone
// who can see the member list -- that this account follows it. So it is its
// own call, made only when the user asks for it.
func (b *Bridge) JoinChannel(ctx context.Context, peer Peer) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	_, err = api.ChannelsJoinChannel(ctx, &tg.InputChannel{
		ChannelID: peer.ID, AccessHash: peer.AccessHash,
	})
	return err
}
