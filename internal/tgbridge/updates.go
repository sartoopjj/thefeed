package tgbridge

import (
	"context"

	"github.com/gotd/td/tg"
)

// Incremental sync.
//
// Asking "what is new" with messages.getDialogs costs the last message of
// EVERY conversation, whether or not it changed -- minutes of queries on a
// degraded link to learn, usually, that nothing happened. Telegram's own
// clients do not work that way, and neither does this:
//
//	updates.getState       five integers. Did anything move at all?
//	updates.getDifference  only what changed since the cursor.
//
// The expensive call is therefore made only when the cheap one says there is
// something to fetch.

// maxBacklog is how far behind the cursor may be before asking for a
// difference stops being worth it. Busy accounts pass this in hours.
const maxBacklog = 1000

// UpdateState is Telegram's cursor: the point in the account's update stream
// that has been seen. Unread is the account-wide count, which arrives free
// with the cursor and needs no message to compute.
type UpdateState struct {
	Pts    int `json:"pts"`
	Qts    int `json:"qts"`
	Date   int `json:"date"`
	Seq    int `json:"seq"`
	Unread int `json:"unread"`
}

// Known reports whether this cursor can be used to ask for a difference.
func (s UpdateState) Known() bool { return s.Pts > 0 && s.Date > 0 }

// PeerMessages is one conversation's share of a difference.
type PeerMessages struct {
	Peer     Peer      `json:"peer"`
	Messages []Message `json:"messages"`
}

// Changes is the answer to "what happened since".
type Changes struct {
	State UpdateState    `json:"state"`
	New   []PeerMessages `json:"new"`
	// Channels are the channels an update named without any message of their
	// own -- joining one elsewhere is reported this way. The caller checks
	// them against what it already has and only then pays for a dialog list.
	Channels []Peer `json:"channels,omitempty"`

	// Reads are "the other side has read up to here" marks. They are what
	// turns one tick into two, and they arrive beside the messages rather
	// than with them.
	Reads []PeerRead `json:"reads,omitempty"`

	// Mutes are notification settings changed elsewhere. Like InReads, a
	// difference is the only thing that reports them: muting a channel on the
	// phone otherwise left it ringing here until the whole list was re-read.
	Mutes []PeerMute `json:"mutes,omitempty"`

	// InReads are "WE have read up to here" marks -- the account reading on
	// another device. Nothing else reports them: a difference carries no
	// conversation rows, so without these a chat read on the phone kept its
	// unread badge here until something paid for the whole list again.
	InReads []PeerInRead `json:"inReads,omitempty"`

	// Gone are conversations this account is no longer in -- a channel left or
	// an account removed from one, on this device or another. Telegram says so
	// in the entity, so a row can be dropped without re-reading the list.
	Gone []Peer `json:"gone,omitempty"`

	// Resync means the cursor is too far behind for Telegram to answer with a
	// difference, so the caller has to fall back to listing conversations.
	Resync bool `json:"resync"`
}

// State reads the cursor. This is the cheap call: five integers, no messages.
func (b *Bridge) State(ctx context.Context) (UpdateState, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return UpdateState{}, err
	}
	st, err := api.UpdatesGetState(ctx)
	if err != nil {
		return UpdateState{}, err
	}
	return UpdateState{
		Pts: st.Pts, Qts: st.Qts, Date: st.Date, Seq: st.Seq, Unread: st.UnreadCount,
	}, nil
}

// Changes asks for everything new since from. An unmoved account answers with
// updates.differenceEmpty, which carries no messages at all.
func (b *Bridge) Changes(ctx context.Context, from UpdateState) (*Changes, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	if !from.Known() {
		return &Changes{Resync: true}, nil
	}
	req := &tg.UpdatesGetDifferenceRequest{Pts: from.Pts, Qts: from.Qts, Date: from.Date}
	// Cap the backlog. Past this, Telegram answers "too long" -- a few bytes --
	// instead of streaming every message missed while the app was closed. One
	// conversation list then rebuilds the state far more cheaply than reading
	// a week of channel posts the user will never scroll back to.
	req.SetPtsTotalLimit(maxBacklog)
	diff, err := api.UpdatesGetDifference(ctx, req)
	if err != nil {
		return nil, err
	}

	switch d := diff.(type) {
	case *tg.UpdatesDifferenceEmpty:
		// The common answer, and the whole point: nothing moved.
		next := from
		next.Date, next.Seq = d.Date, d.Seq
		return &Changes{State: next}, nil

	case *tg.UpdatesDifference:
		h := otherUpdates(d.OtherUpdates, d.Users, d.Chats, b.notifyDefaults(ctx))
		if h.DefaultsChanged {
			b.forgetNotifyDefaults()
		}
		return &Changes{
			State: UpdateState{
				Pts: d.State.Pts, Qts: d.State.Qts, Date: d.State.Date,
				Seq: d.State.Seq, Unread: d.State.UnreadCount,
			},
			New:      groupByPeer(append(d.NewMessages, h.Msgs...), d.Users, d.Chats),
			Channels: h.Channels,
			Reads:    h.Reads,
			InReads:  h.InReads,
			Mutes:    h.Mutes,
			Gone:     h.Gone,
		}, nil

	case *tg.UpdatesDifferenceSlice:
		// Partial: the cursor advances to the intermediate state and the
		// caller may ask again for the rest.
		h := otherUpdates(d.OtherUpdates, d.Users, d.Chats, b.notifyDefaults(ctx))
		if h.DefaultsChanged {
			b.forgetNotifyDefaults()
		}
		return &Changes{
			State: UpdateState{
				Pts: d.IntermediateState.Pts, Qts: d.IntermediateState.Qts,
				Date: d.IntermediateState.Date, Seq: d.IntermediateState.Seq,
				Unread: d.IntermediateState.UnreadCount,
			},
			New:      groupByPeer(append(d.NewMessages, h.Msgs...), d.Users, d.Chats),
			Channels: h.Channels,
			Reads:    h.Reads,
			InReads:  h.InReads,
			Mutes:    h.Mutes,
			Gone:     h.Gone,
		}, nil

	case *tg.UpdatesDifferenceTooLong:
		// Too far behind to describe as a difference. Say so rather than
		// pretending nothing happened.
		return &Changes{State: UpdateState{Pts: d.Pts}, Resync: true}, nil
	}
	return &Changes{State: from}, nil
}

// PeerRead is how far one conversation's outgoing messages have been read.
type PeerRead struct {
	Peer  Peer `json:"peer"`
	MaxID int  `json:"maxId"`
}

// PeerInRead is how far THIS account has read a conversation, with the number
// of messages Telegram says are still unread in it. Telegram sends it whenever
// the reading happened on another device, and the count is its own, not a
// subtraction we could do here.
type PeerInRead struct {
	Peer  Peer `json:"peer"`
	MaxID int  `json:"maxId"`
	// Unread is Telegram's still-unread count for the conversation. Zero is a
	// real answer -- "nothing left" -- which is the whole point of carrying it.
	Unread int `json:"unread"`
}

// PeerMute is one conversation's notification setting as Telegram now has it.
type PeerMute struct {
	Peer  Peer `json:"peer"`
	Muted bool `json:"muted"`
}

// harvest is everything one difference's OtherUpdates list yields. A struct
// rather than a row of unnamed returns: these are six different things and
// the compiler cannot tell two []Peer apart at a call site.
type harvest struct {
	Msgs     []tg.MessageClass
	Channels []Peer
	Reads    []PeerRead
	InReads  []PeerInRead
	Mutes    []PeerMute
	Gone     []Peer
	// DefaultsChanged marks an account-wide notification change (the per-type
	// defaults, not one conversation). The cached defaults are stale after it.
	DefaultsChanged bool
}

// otherUpdates mines the updates a difference carries beside its message box.
//
// NewMessages is only the account's own box: private chats and basic groups.
// Every CHANNEL post arrives here instead, as updateNewChannelMessage, so
// ignoring this list meant no channel ever changed through a difference --
// a channel joined on another device simply never appeared.
//
// Also reports the channels named without a message of their own, which is how
// joining one is announced when it has not posted since.
func otherUpdates(ups []tg.UpdateClass, users []tg.UserClass, chats []tg.ChatClass, def NotifyDefaults) harvest {
	var out harvest
	seen := map[int64]bool{}
	idx := peerIndex(users, chats)
	left := leftChannels(chats)
	for _, up := range ups {
		switch u := up.(type) {
		case *tg.UpdateNewChannelMessage:
			out.Msgs = append(out.Msgs, u.Message)
		case *tg.UpdateNewMessage:
			out.Msgs = append(out.Msgs, u.Message)
		case *tg.UpdateEditChannelMessage:
			// An edit keeps its id, so the merge replaces the old text.
			out.Msgs = append(out.Msgs, u.Message)
		case *tg.UpdateEditMessage:
			out.Msgs = append(out.Msgs, u.Message)
		case *tg.UpdateChannel:
			if seen[u.ChannelID] {
				continue
			}
			seen[u.ChannelID] = true
			// Left or thrown out: the entity says so outright, so the row goes
			// without paying for the whole conversation list to find out.
			if p, ok := left[u.ChannelID]; ok {
				out.Gone = append(out.Gone, p)
				continue
			}
			if p, ok := idx["n"+itoa64(u.ChannelID)]; ok {
				out.Channels = append(out.Channels, p)
			}
		case *tg.UpdateReadHistoryOutbox:
			// The other side read OUR messages: the second tick. Without this
			// it only ever arrived by asking for the conversation's row
			// outright, which happens for a short while after sending and
			// then stops -- so a message read later stayed on one tick.
			if p, ok := peerOf(u.Peer, idx); ok {
				out.Reads = append(out.Reads, PeerRead{Peer: p, MaxID: u.MaxID})
			}
		case *tg.UpdateReadChannelOutbox:
			if p, ok := idx["n"+itoa64(u.ChannelID)]; ok {
				out.Reads = append(out.Reads, PeerRead{Peer: p, MaxID: u.MaxID})
			}
		case *tg.UpdateReadHistoryInbox:
			// WE read it, somewhere else. A difference carries no conversation
			// rows, so this update is the ONLY thing that can bring an unread
			// badge down after the chat was opened on the phone: without it the
			// row here kept its count until something paid for the whole list.
			//
			// A topic read is not a chat read, so one is skipped: a forum's
			// unread count covers the whole forum and a single topic going
			// quiet says nothing about the rest.
			if _, ok := u.GetTopMsgID(); ok {
				continue
			}
			if p, ok := peerOf(u.Peer, idx); ok {
				out.InReads = append(out.InReads, PeerInRead{
					Peer: p, MaxID: u.MaxID, Unread: u.StillUnreadCount,
				})
			}
		case *tg.UpdateNotifySettings:
			// Muting a channel on another device. Only a per-peer setting is
			// usable: notifyUsers / notifyChats / notifyBroadcasts are the
			// account-wide defaults for a whole class of conversation, and
			// applying one of those to every cached row would silence chats
			// the user had deliberately left audible.
			np, ok := u.Peer.(*tg.NotifyPeer)
			if !ok {
				// An account-wide default changed. It cannot be applied to
				// rows one by one -- a conversation with its own override
				// ignores it -- so the cached defaults are dropped and the
				// next list read picks the new ones up.
				out.DefaultsChanged = true
				continue
			}
			if p, ok := peerOf(np.Peer, idx); ok {
				out.Mutes = append(out.Mutes, PeerMute{Peer: p, Muted: mutedNow(u.NotifySettings, def.For(p.Kind))})
			}
		case *tg.UpdateReadChannelInbox:
			// No topic field on this one: a channel's read mark is the whole
			// channel, and a forum topic read arrives as readHistoryInbox.
			if p, ok := idx["n"+itoa64(u.ChannelID)]; ok {
				out.InReads = append(out.InReads, PeerInRead{
					Peer: p, MaxID: u.MaxID, Unread: u.StillUnreadCount,
				})
			}
		}
	}
	return out
}

// leftChannels indexes the channels an update's entities say this account is
// no longer in. channelForbidden means removed or banned; a plain channel with
// the left flag means it was left, here or on another device.
func leftChannels(chats []tg.ChatClass) map[int64]Peer {
	out := map[int64]Peer{}
	for _, c := range chats {
		switch cc := c.(type) {
		case *tg.ChannelForbidden:
			out[cc.ID] = Peer{Kind: PeerChannel, ID: cc.ID, AccessHash: cc.AccessHash, Title: cc.Title}
		case *tg.Channel:
			if cc.Left {
				out[cc.ID] = peerFromChannel(cc)
			}
		}
	}
	return out
}

// peerOf resolves a bare peer reference against the entities that came with
// the difference.
func peerOf(p tg.PeerClass, idx map[string]Peer) (Peer, bool) {
	switch t := p.(type) {
	case *tg.PeerUser:
		v, ok := idx["u"+itoa64(t.UserID)]
		return v, ok
	case *tg.PeerChat:
		v, ok := idx["c"+itoa64(t.ChatID)]
		return v, ok
	case *tg.PeerChannel:
		v, ok := idx["n"+itoa64(t.ChannelID)]
		return v, ok
	}
	return Peer{}, false
}

// groupByPeer turns a flat list of new messages into one entry per
// conversation, resolving each peer from the users and chats that came with
// the difference.
func groupByPeer(msgs []tg.MessageClass, users []tg.UserClass, chats []tg.ChatClass) []PeerMessages {
	if len(msgs) == 0 {
		return nil
	}
	names := senderNames(users, chats)
	peers := peerIndex(users, chats)

	order := make([]string, 0, len(msgs))
	byKey := map[string]*PeerMessages{}
	for _, raw := range msgs {
		p, ok := peerOfMessage(raw, peers)
		if !ok {
			continue
		}
		m, ok := messageFrom(raw, names)
		if !ok {
			continue
		}
		key := string(p.Kind) + ":" + itoa64(p.ID)
		pm, seen := byKey[key]
		if !seen {
			pm = &PeerMessages{Peer: p}
			byKey[key] = pm
			order = append(order, key)
		}
		pm.Messages = append(pm.Messages, m)
	}
	out := make([]PeerMessages, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// peerIndex maps Telegram's numeric ids to the peers they describe.
func peerIndex(users []tg.UserClass, chats []tg.ChatClass) map[string]Peer {
	idx := make(map[string]Peer, len(users)+len(chats))
	for _, u := range users {
		if uu, ok := u.(*tg.User); ok {
			idx["u"+itoa64(uu.ID)] = peerFromUser(uu)
		}
	}
	for _, c := range chats {
		switch cc := c.(type) {
		case *tg.Chat:
			idx["c"+itoa64(cc.ID)] = peerFromChat(cc)
		case *tg.Channel:
			idx["n"+itoa64(cc.ID)] = peerFromChannel(cc)
		}
	}
	return idx
}

// peerOfMessage finds which conversation a message belongs to. A message we
// sent is filed under whom it was sent TO, which is what the list shows.
func peerOfMessage(m tg.MessageClass, idx map[string]Peer) (Peer, bool) {
	var to tg.PeerClass
	switch mm := m.(type) {
	case *tg.Message:
		to = mm.PeerID
	case *tg.MessageService:
		to = mm.PeerID
	default:
		return Peer{}, false
	}
	switch p := to.(type) {
	case *tg.PeerUser:
		v, ok := idx["u"+itoa64(p.UserID)]
		return v, ok
	case *tg.PeerChat:
		v, ok := idx["c"+itoa64(p.ChatID)]
		return v, ok
	case *tg.PeerChannel:
		v, ok := idx["n"+itoa64(p.ChannelID)]
		return v, ok
	}
	return Peer{}, false
}

// muteForever is the "until" Telegram clients use for an indefinite mute: far
// enough ahead that it never lapses, without overflowing a 32-bit time.
const muteForever = 2147483647

// SetMuted turns notifications for one conversation on or off, through
// Telegram's own setting rather than a local flag -- so it matches what every
// other client shows, and follows the account to a new device.
func (b *Bridge) SetMuted(ctx context.Context, peer Peer, muted bool) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	settings := tg.InputPeerNotifySettings{}
	if muted {
		settings.SetMuteUntil(muteForever)
	} else {
		// Zero means "not muted", which is different from leaving the field
		// out: left out, Telegram keeps whatever was there.
		settings.SetMuteUntil(0)
	}
	_, err = api.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{
		Peer:     &tg.InputNotifyPeer{Peer: peer.inputPeer()},
		Settings: settings,
	})
	return err
}
