package tgbridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gotd/td/tg"
)

// DialogFilter asks for one page of conversations; every kind is returned and
// the caller groups them.
type DialogFilter struct {
	// Limit caps rows fetched in one call. The tunnel is bandwidth-scarce, so
	// the caller pages rather than pulling everything.
	Limit int
	// Offset* continue a previous page. Zero values start from the top.
	OffsetDate int
	OffsetID   int
	OffsetPeer Peer
}

// DialogPage is one page of conversations plus where to continue from.
type DialogPage struct {
	Dialogs []Dialog `json:"dialogs"`
	// More says another page exists; the Next* values feed the request for it.
	More       bool `json:"more"`
	NextDate   int  `json:"nextDate,omitempty"`
	NextID     int  `json:"nextId,omitempty"`
	NextPeer   Peer `json:"nextPeer,omitempty"`
	TotalKnown int  `json:"totalKnown,omitempty"`
}

const (
	defaultDialogLimit = 50
	// Telegram caps a page at 100 rows, and over the tunnel a page that size is
	// already expensive.
	maxDialogLimit = 100
)

// clampLimit keeps a page size sane at the only place that calls Telegram.
func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// Dialogs lists conversations, newest first. One request only: paging is the
// caller's job, because each page is a tunnel round trip the user waits for.
func (b *Bridge) Dialogs(ctx context.Context, filter DialogFilter) (*DialogPage, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	limit := clampLimit(filter.Limit, defaultDialogLimit, maxDialogLimit)
	req := &tg.MessagesGetDialogsRequest{
		Limit:      limit,
		OffsetPeer: &tg.InputPeerEmpty{},
		OffsetDate: filter.OffsetDate,
		OffsetID:   filter.OffsetID,
	}
	if filter.OffsetPeer.ID != 0 {
		req.OffsetPeer = filter.OffsetPeer.inputPeer()
	}
	res, err := api.MessagesGetDialogs(ctx, req)
	if err != nil {
		return nil, err
	}
	return dialogPageFrom(res, limit, b.notifyDefaults(ctx)), nil
}

// dialogPageFrom maps a reply and works out how to ask for the next page.
func dialogPageFrom(res tg.MessagesDialogsClass, limit int, def NotifyDefaults) *DialogPage {
	page := &DialogPage{Dialogs: dialogsFrom(res, def)}

	var raw []tg.DialogClass
	var msgs []tg.MessageClass
	switch d := res.(type) {
	case *tg.MessagesDialogs:
		raw, msgs = d.Dialogs, d.Messages
	case *tg.MessagesDialogsSlice:
		raw, msgs = d.Dialogs, d.Messages
		page.TotalKnown = d.Count
	default:
		return page
	}
	// Telegram pages by the oldest row, so the cursor comes from the last RAW
	// dialog: an unmapped row must still advance it.
	if len(raw) < limit {
		return page
	}
	last, ok := raw[len(raw)-1].(*tg.Dialog)
	if !ok {
		return page
	}
	page.NextID = last.TopMessage
	for _, m := range msgs {
		if msg, ok := m.(*tg.Message); ok && msg.ID == last.TopMessage {
			page.NextDate = msg.Date
			break
		}
	}
	// The cursor needs the mapped peer for its access hash, keyed by type and
	// id since a user and a channel can share a number.
	key := peerKey(last.Peer)
	for _, d := range page.Dialogs {
		if mappedPeerKey(d.Peer) == key {
			page.NextPeer = d.Peer
			break
		}
	}
	page.More = page.NextID != 0
	return page
}

// mappedPeerKey rebuilds peerKey's value from a mapped Peer, inverting the
// split of Telegram's single "channel" type into channel / supergroup / forum.
func mappedPeerKey(p Peer) string {
	switch p.Kind {
	case PeerChannel, PeerForum:
		return "h" + itoa64(p.ID)
	case PeerGroup:
		// A supergroup is channel-backed and carries an access hash; a legacy
		// group has none and is a PeerChat.
		if p.AccessHash != 0 {
			return "h" + itoa64(p.ID)
		}
		return "c" + itoa64(p.ID)
	default:
		return "u" + itoa64(p.ID)
	}
}

// dialogsFrom is the pure mapping half, so the shape of Telegram's reply can
// be tested without a network.
func dialogsFrom(res tg.MessagesDialogsClass, def NotifyDefaults) []Dialog {
	var (
		rawDialogs []tg.DialogClass
		users      []tg.UserClass
		chats      []tg.ChatClass
		messages   []tg.MessageClass
	)
	switch d := res.(type) {
	case *tg.MessagesDialogs:
		rawDialogs, users, chats, messages = d.Dialogs, d.Users, d.Chats, d.Messages
	case *tg.MessagesDialogsSlice:
		rawDialogs, users, chats, messages = d.Dialogs, d.Users, d.Chats, d.Messages
	default:
		return nil
	}

	// Index the entities the dialogs refer to by id.
	userByID := make(map[int64]*tg.User, len(users))
	for _, u := range users {
		if uu, ok := u.(*tg.User); ok {
			userByID[uu.ID] = uu
		}
	}
	chatByID := make(map[int64]Peer, len(chats))
	for _, c := range chats {
		switch cc := c.(type) {
		case *tg.Chat:
			chatByID[cc.ID] = peerFromChat(cc)
		case *tg.Channel:
			chatByID[cc.ID] = peerFromChannel(cc)
		}
	}
	// Top message text and time, for the list row. A media-only message has no
	// text but still dates the row, so the two are indexed separately.
	previewByPeer := make(map[string]string, len(messages))
	whenByPeer := make(map[string]int, len(messages))
	for _, m := range messages {
		var (
			key  string
			text string
			date int
		)
		switch msg := m.(type) {
		case *tg.Message:
			key, text, date = peerKey(msg.PeerID), msg.Message, msg.Date
		case *tg.MessageService:
			key, date = peerKey(msg.PeerID), msg.Date
		default:
			continue
		}
		if key == "" {
			continue
		}
		if text != "" {
			if _, seen := previewByPeer[key]; !seen {
				previewByPeer[key] = text
			}
		}
		if date > whenByPeer[key] {
			whenByPeer[key] = date
		}
	}

	out := make([]Dialog, 0, len(rawDialogs))
	for _, raw := range rawDialogs {
		d, ok := raw.(*tg.Dialog)
		if !ok {
			continue
		}
		var peer Peer
		switch p := d.Peer.(type) {
		case *tg.PeerUser:
			u, ok := userByID[p.UserID]
			if !ok {
				continue
			}
			peer = peerFromUser(u)
		case *tg.PeerChat:
			peer, ok = chatByID[p.ChatID]
			if !ok {
				continue
			}
		case *tg.PeerChannel:
			peer, ok = chatByID[p.ChannelID]
			if !ok {
				continue
			}
		default:
			continue
		}
		key := peerKey(d.Peer)
		out = append(out, Dialog{
			Peer:       peer,
			Unread:     d.UnreadCount,
			TopMsg:     d.TopMessage,
			When:       whenByPeer[key],
			ReadOutbox: d.ReadOutboxMaxID,
			ReadInbox:  d.ReadInboxMaxID,
			Pinned:     d.Pinned,
			Preview:    previewByPeer[key],
			Muted:      mutedNow(d.NotifySettings, def.For(peer.Kind)),
		})
	}
	return out
}

// mutedNow reports whether a conversation is silenced right now, given the
// account-wide default for its TYPE as the fallback.
//
// Settings come in two layers: a per-conversation override, and one default
// each for users, groups and channels. A conversation with no override is not
// "audible" -- it follows its type's default, and both of its fields simply
// arrive unset. Muting a type is one tap, so most people never set a
// per-channel override at all.
//
// Answered per call rather than stored: an override is a "muted until" time,
// so a cached flag goes stale on its own.
func mutedNow(ns tg.PeerNotifySettings, def bool) bool {
	// Silent is the other half of the override: Telegram expresses "no sound"
	// two ways, and a conversation silenced through this one has no
	// mute-until at all.
	if silent, ok := ns.GetSilent(); ok && silent {
		return true
	}
	if until, ok := ns.GetMuteUntil(); ok {
		return int64(until) > time.Now().Unix()
	}
	return def
}

// notifyDefaults returns the account-wide defaults, fetching them once per
// connection. Three small calls, and only the first time: they change when the
// user changes them, which arrives as an account-wide UpdateNotifySettings and
// drops the cache (see forgetNotifyDefaults).
func (b *Bridge) notifyDefaults(ctx context.Context) NotifyDefaults {
	b.mu.Lock()
	if b.notifyOK {
		def := b.notifyDef
		b.mu.Unlock()
		return def
	}
	// A recent attempt failed; do not spend three more calls yet.
	if !b.notifyAt.IsZero() && time.Since(b.notifyAt) < notifyRetryAfter {
		b.mu.Unlock()
		return NotifyDefaults{}
	}
	b.mu.Unlock()

	_, api, err := b.snapshot()
	if err != nil {
		return NotifyDefaults{}
	}
	var failed bool
	read := func(peer tg.InputNotifyPeerClass) bool {
		ns, e := api.AccountGetNotifySettings(ctx, peer)
		if e != nil || ns == nil {
			failed = true
			return false
		}
		// No fallback behind a DEFAULT: it is the bottom of the chain.
		return mutedNow(*ns, false)
	}
	def := NotifyDefaults{
		Users:      read(&tg.InputNotifyUsers{}),
		Chats:      read(&tg.InputNotifyChats{}),
		Broadcasts: read(&tg.InputNotifyBroadcasts{}),
	}

	return b.rememberNotifyDefaults(def, failed, time.Now())
}

// rememberNotifyDefaults records the outcome of a defaults read and returns
// what callers should use.
//
// A call that did not answer is NOT an answer of "nothing is muted". Caching
// one would silence the whole mechanism for the rest of the connection and
// look exactly like having no defaults at all -- the very bug this exists to
// fix, but with nothing on screen or in the log to show for it. So a failure
// is only spaced out, never cached: the updates loop asks on every poll, and
// an unreachable server must not cost three failed calls each time round.
func (b *Bridge) rememberNotifyDefaults(def NotifyDefaults, failed bool, now time.Time) NotifyDefaults {
	b.mu.Lock()
	defer b.mu.Unlock()
	if failed {
		b.notifyAt = now
		return NotifyDefaults{}
	}
	b.notifyDef, b.notifyOK = def, true
	return def
}

// forgetNotifyDefaults drops the cached defaults so the next read re-fetches.
func (b *Bridge) forgetNotifyDefaults() {
	b.mu.Lock()
	b.notifyOK = false
	b.mu.Unlock()
}

// notifyRetryAfter spaces out retries of the account-wide defaults after a
// failed read, so a connection that cannot reach them does not spend three
// calls on every poll.
const notifyRetryAfter = 2 * time.Minute

// NotifyDefaults is the account-wide notification setting for each
// conversation type -- what a conversation without its own override follows.
type NotifyDefaults struct {
	Users      bool
	Chats      bool
	Broadcasts bool
}

// For returns the default that applies to a conversation of this kind.
func (n NotifyDefaults) For(k PeerKind) bool {
	switch k {
	case PeerChannel:
		return n.Broadcasts
	case PeerGroup, PeerForum:
		return n.Chats
	default:
		return n.Users
	}
}

// peerKey is a stable map key for a tg peer reference.
func peerKey(p tg.PeerClass) string {
	switch v := p.(type) {
	case *tg.PeerUser:
		return "u" + itoa64(v.UserID)
	case *tg.PeerChat:
		return "c" + itoa64(v.ChatID)
	case *tg.PeerChannel:
		return "h" + itoa64(v.ChannelID)
	}
	return ""
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

const (
	defaultHistoryLimit = 15
	maxHistoryLimit     = 100
)

// History fetches messages newest first. offsetID pages backwards from the
// oldest id held; minID tops a cached conversation up with only what is newer.
func (b *Bridge) History(ctx context.Context, peer Peer, limit, offsetID, minID int) ([]Message, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	limit = clampLimit(limit, defaultHistoryLimit, maxHistoryLimit)
	res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     peer.inputPeer(),
		Limit:    limit,
		OffsetID: offsetID,
		MinID:    minID,
	})
	if err != nil {
		return nil, err
	}
	return messagesFrom(res), nil
}

// DeleteChat removes the conversation from the account. revoke also deletes it
// for the other side where Telegram allows that.
func (b *Bridge) DeleteChat(ctx context.Context, peer Peer, revoke bool) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	switch peer.Kind {
	case PeerChannel, PeerForum:
		_, err = api.ChannelsLeaveChannel(ctx, &tg.InputChannel{
			ChannelID: peer.ID, AccessHash: peer.AccessHash,
		})
		return err
	}
	_, err = api.MessagesDeleteHistory(ctx, &tg.MessagesDeleteHistoryRequest{
		Peer: peer.inputPeer(), Revoke: revoke, JustClear: false,
	})
	return err
}

// PeerState re-reads one conversation's row: unread count and read mark. One
// row rather than a page, for a delivery tick.
func (b *Bridge) PeerState(ctx context.Context, peer Peer) (Dialog, error) {
	rows, err := b.PeerStates(ctx, []Peer{peer})
	if err != nil {
		return Dialog{}, err
	}
	if len(rows) == 0 {
		return Dialog{}, errors.New("tgbridge: peer not found")
	}
	return rows[0], nil
}

// MaxPeerStates is how many conversations one PeerStates call may ask about.
// Telegram takes a vector, so a hundred rows cost the same single round trip
// as one -- which is what makes it affordable over a link measured in queries
// per second.
const MaxPeerStates = 100

// PeerStates returns the authoritative rows for specific conversations:
// notification settings, unread counts and read marks, as Telegram holds them.
// It is the cheap way to repair rows the cache had to invent, because a
// difference names conversations without describing them.
func (b *Bridge) PeerStates(ctx context.Context, peers []Peer) ([]Dialog, error) {
	if len(peers) == 0 {
		return nil, nil
	}
	if len(peers) > MaxPeerStates {
		peers = peers[:MaxPeerStates]
	}
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	in := make([]tg.InputDialogPeerClass, 0, len(peers))
	for _, p := range peers {
		in = append(in, &tg.InputDialogPeer{Peer: p.inputPeer()})
	}
	res, err := api.MessagesGetPeerDialogs(ctx, in)
	if err != nil {
		return nil, err
	}
	return dialogsFrom(&tg.MessagesDialogs{
		Dialogs:  res.Dialogs,
		Messages: res.Messages,
		Users:    res.Users,
		Chats:    res.Chats,
	}, b.notifyDefaults(ctx)), nil
}

// MarkRead clears the unread count up to maxID. Telegram models this per peer
// type: channels have their own method.
func (b *Bridge) MarkRead(ctx context.Context, peer Peer, maxID int) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	switch peer.Kind {
	case PeerChannel, PeerForum:
		_, err = api.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
			Channel: &tg.InputChannel{ChannelID: peer.ID, AccessHash: peer.AccessHash},
			MaxID:   maxID,
		})
		return err
	case PeerGroup:
		// A supergroup is a channel underneath; a legacy group is not.
		if peer.AccessHash != 0 {
			_, err = api.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
				Channel: &tg.InputChannel{ChannelID: peer.ID, AccessHash: peer.AccessHash},
				MaxID:   maxID,
			})
			return err
		}
	}
	_, err = api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
		Peer:  peer.inputPeer(),
		MaxID: maxID,
	})
	return err
}

// ResolveUsername finds a peer by @username so a new conversation can be
// started with someone who is not in the dialog list yet.
func (b *Bridge) ResolveUsername(ctx context.Context, username string) (Peer, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return Peer{}, err
	}
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return Peer{}, errors.New("tgbridge: empty username")
	}
	res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return Peer{}, err
	}
	for _, c := range res.Chats {
		switch ch := c.(type) {
		case *tg.Channel:
			return peerFromChannel(ch), nil
		case *tg.Chat:
			return peerFromChat(ch), nil
		}
	}
	for _, u := range res.Users {
		if user, ok := u.(*tg.User); ok {
			return peerFromUser(user), nil
		}
	}
	return Peer{}, errors.New("tgbridge: username not found")
}

// messagesFrom is the pure mapping half of History.
func messagesFrom(res tg.MessagesMessagesClass) []Message {
	var (
		raw   []tg.MessageClass
		users []tg.UserClass
		chats []tg.ChatClass
	)
	switch m := res.(type) {
	case *tg.MessagesMessages:
		raw, users, chats = m.Messages, m.Users, m.Chats
	case *tg.MessagesMessagesSlice:
		raw, users, chats = m.Messages, m.Users, m.Chats
	case *tg.MessagesChannelMessages:
		raw, users, chats = m.Messages, m.Users, m.Chats
	default:
		return nil
	}
	names := senderNames(users, chats)
	out := make([]Message, 0, len(raw))
	for _, m := range raw {
		if msg, ok := messageFrom(m, names); ok {
			out = append(out, msg)
		}
	}
	return out
}

// SendText sends a plain text message. randomID deduplicates a retry: Telegram
// drops a repeat with the same id, which matters on a lossy tunnel. replyTo,
// when set, answers that message -- twelve bytes more on the wire.
// The id Telegram assigned comes back in the answer, so the caller does not
// have to read the message back to learn it. Over DNS that saved round trip is
// most of the wait: the whole send was two fetches where one would do.
func (b *Bridge) SendText(ctx context.Context, peer Peer, text string, randomID int64, replyTo int) (int, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return 0, err
	}
	req := &tg.MessagesSendMessageRequest{
		Peer:     peer.inputPeer(),
		Message:  text,
		RandomID: randomID,
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	ups, err := api.MessagesSendMessage(ctx, req)
	if err != nil {
		return 0, err
	}
	return sentMessageID(ups), nil
}

// sentMessageID digs the assigned id out of a send's answer. Zero means
// Telegram did not say, which is not an error -- the message was accepted
// either way, and the caller falls back to reading it back.
func sentMessageID(ups tg.UpdatesClass) int {
	var list []tg.UpdateClass
	switch u := ups.(type) {
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	case *tg.UpdateShortSentMessage:
		// The common answer for a plain text message to a private chat.
		return u.ID
	case *tg.UpdateShort:
		list = []tg.UpdateClass{u.Update}
	}
	for _, up := range list {
		switch t := up.(type) {
		case *tg.UpdateMessageID:
			return t.ID
		case *tg.UpdateNewMessage:
			if m, ok := t.Message.(*tg.Message); ok {
				return m.ID
			}
		case *tg.UpdateNewChannelMessage:
			if m, ok := t.Message.(*tg.Message); ok {
				return m.ID
			}
		}
	}
	return 0
}
