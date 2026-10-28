package tgbridge

import (
	"github.com/gotd/td/telegram/thumbnail"
	"github.com/gotd/td/tg"
)

// PeerKind is what a dialog is; the UI lists each kind separately.
type PeerKind string

const (
	PeerUser    PeerKind = "user"
	PeerBot     PeerKind = "bot"
	PeerGroup   PeerKind = "group"
	PeerChannel PeerKind = "channel"
	PeerForum   PeerKind = "forum" // a group with topics
)

// Peer identifies one conversation. AccessHash is required to address a
// channel or user again later, so it travels with the id.
type Peer struct {
	Kind PeerKind `json:"kind"`
	// Strings on the wire: these exceed 2^53 and JavaScript corrupts them as
	// numbers, which Telegram then rejects as an invalid peer.
	ID         int64  `json:"id,string"`
	AccessHash int64  `json:"accessHash,string"`
	Title      string `json:"title"`
	Username   string `json:"username,omitempty"`
	// ReadOnly means Telegram will not accept a message here: a broadcast
	// channel without post rights, or a group that bans sending. Absent means
	// writable, so a peer built without this stays as it was.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Thumb is the conversation's picture as a tiny JPEG, about 40px on its
	// longest side.
	//
	// It costs NOTHING to have. Telegram puts a stripped thumbnail inside the
	// user and chat objects that come with the conversation list, and
	// messages.getDialogs has no flag to leave it out -- so these bytes cross
	// the link whether or not anything reads them, and until now they were
	// read and thrown away. The real photo is a separate download from a media
	// data centre: five to fifteen kilobytes for the smallest size, which over
	// this transport is tens of round trips PER avatar. That is why only this
	// one is taken.
	Thumb []byte `json:"thumb,omitempty"`
}

// Dialog is one row in the chat list.
type Dialog struct {
	Peer   Peer `json:"peer"`
	Unread int  `json:"unread"`
	TopMsg int  `json:"topMsg,omitempty"`
	// When is the top message's unix time, for the list's stamp.
	When int `json:"when,omitempty"`
	// ReadOutbox is the highest message id of OURS the other side has read.
	// It is what turns a sent message's single tick into a double one.
	ReadOutbox int `json:"readOutbox,omitempty"`
	// ReadInbox is the highest message id WE have read, as Telegram knows it
	// -- the account's read mark, shared by every device on it. It is where
	// the "new messages" line goes, and the only honest source for it: a
	// local guess would draw the line in a different place on each device.
	ReadInbox int    `json:"readInbox,omitempty"`
	Pinned    bool   `json:"pinned,omitempty"`
	Preview   string `json:"preview,omitempty"`
	// Muted mirrors Telegram's own notification setting for this
	// conversation. A muted chat still counts unread -- it just must not
	// interrupt, which is the entire reason someone mutes a busy channel.
	Muted bool `json:"muted,omitempty"`
	// NotifyUnknown marks a row built from a difference rather than from the
	// conversation list. A difference carries messages, never notification
	// settings, so Muted above is not an answer for such a row -- it is the
	// zero value. Without this the cache could only ever guess "audible",
	// which is the wrong way to guess: it makes a silenced channel ring.
	NotifyUnknown bool `json:"notifyUnknown,omitempty"`
}

// Message is one message in a conversation, flattened for the UI.
type Message struct {
	ID      int    `json:"id"`
	Date    int    `json:"date"`
	Service bool   `json:"service,omitempty"`
	Text    string `json:"text"`
	Out     bool   `json:"out,omitempty"`
	FromID  int64  `json:"fromId,string,omitempty"`
	// From is the sender's name, resolved from the reply's own entities.
	From string `json:"from,omitempty"`
	// Media names the attachment, since none of it is downloaded. MediaName
	// is a file name or a sticker's emoji.
	Media     string `json:"media,omitempty"`
	MediaName string `json:"mediaName,omitempty"`
	MediaSize int64  `json:"mediaSize,omitempty"`
	// Buttons are a bot's inline keyboard, listed but not pressed.
	Buttons []Button `json:"buttons,omitempty"`
	// ReplyTo is the message this one answers, and ReplyQuote the part of it
	// the sender quoted. Both ride along with the message already, so showing
	// a reply costs nothing extra.
	ReplyTo    int    `json:"replyTo,omitempty"`
	ReplyQuote string `json:"replyQuote,omitempty"`
	// Views and Reactions are what a channel post carries besides its text.
	// Shaped like the Mirror's (telemirror.Reaction) so the two sections show
	// the same thing the same way.
	Views     int        `json:"views,omitempty"`
	Reactions []Reaction `json:"reactions,omitempty"`
}

// Reaction is one emoji and how many people picked it. Chosen marks the one
// this account picked, so the UI can highlight it and a second tap can mean
// "take it back" rather than "react again".
type Reaction struct {
	Emoji  string `json:"emoji"`
	Count  int    `json:"count"`
	Chosen bool   `json:"chosen,omitempty"`
}

// Button is one key of an inline keyboard; URL is set only for the kind that
// leads somewhere.
type Button struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
	Row  int    `json:"row"`
}

// buttonsOf flattens an inline keyboard. Only the inline kind is carried: a
// reply keyboard is a suggestion for the text box, not part of the message.
func buttonsOf(markup tg.ReplyMarkupClass) []Button {
	inline, ok := markup.(*tg.ReplyInlineMarkup)
	if !ok {
		return nil
	}
	var out []Button
	for i, row := range inline.Rows {
		for _, b := range row.Buttons {
			switch v := b.(type) {
			case *tg.KeyboardButtonURL:
				out = append(out, Button{Text: v.Text, URL: v.URL, Row: i})
			case *tg.KeyboardButtonURLAuth:
				out = append(out, Button{Text: v.Text, URL: v.URL, Row: i})
			default:
				if t, ok := b.(interface{ GetText() string }); ok {
					out = append(out, Button{Text: t.GetText(), Row: i})
				}
			}
		}
	}
	return out
}

// Media kinds. Short keys: the UI translates them.
const (
	MediaPhoto     = "photo"
	MediaVideo     = "video"
	MediaVideoNote = "videonote"
	MediaVoice     = "voice"
	MediaAudio     = "audio"
	MediaSticker   = "sticker"
	MediaGIF       = "gif"
	MediaFile      = "file"
	MediaGeo       = "geo"
	MediaContact   = "contact"
	MediaPoll      = "poll"
	MediaGame      = "game"
	MediaInvoice   = "invoice"
	MediaDice      = "dice"
	MediaStory     = "story"
	MediaOther     = "other"
)

// mediaOf names an attachment. A link preview is not one: the link is already
// in the text.
func mediaOf(m tg.MessageMediaClass) (kind, name string, size int64) {
	switch v := m.(type) {
	case *tg.MessageMediaPhoto:
		return MediaPhoto, "", 0
	case *tg.MessageMediaDocument:
		doc, ok := v.Document.AsNotEmpty()
		if !ok {
			return MediaFile, "", 0
		}
		return documentKind(doc)
	case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive, *tg.MessageMediaVenue:
		return MediaGeo, "", 0
	case *tg.MessageMediaContact:
		return MediaContact, v.FirstName, 0
	case *tg.MessageMediaPoll:
		return MediaPoll, v.Poll.Question.Text, 0
	case *tg.MessageMediaGame:
		return MediaGame, v.Game.Title, 0
	case *tg.MessageMediaInvoice:
		return MediaInvoice, v.Title, 0
	case *tg.MessageMediaDice:
		return MediaDice, v.Emoticon, 0
	case *tg.MessageMediaStory:
		return MediaStory, "", 0
	case *tg.MessageMediaWebPage:
		return "", "", 0
	case *tg.MessageMediaEmpty:
		return "", "", 0
	}
	return MediaOther, "", 0
}

// documentKind tells apart what Telegram models as one "document" — sticker,
// video, voice, file — by its attributes.
func documentKind(doc *tg.Document) (kind, name string, size int64) {
	size = doc.Size
	kind = MediaFile
	animated := false
	for _, a := range doc.Attributes {
		switch at := a.(type) {
		case *tg.DocumentAttributeSticker:
			return MediaSticker, at.Alt, size
		case *tg.DocumentAttributeAnimated:
			animated = true
		case *tg.DocumentAttributeVideo:
			if at.RoundMessage {
				kind = MediaVideoNote
			} else if kind != MediaSticker {
				kind = MediaVideo
			}
		case *tg.DocumentAttributeAudio:
			if at.Voice {
				kind = MediaVoice
			} else {
				kind = MediaAudio
				if at.Title != "" {
					name = at.Title
				}
			}
		case *tg.DocumentAttributeFilename:
			if name == "" {
				name = at.FileName
			}
		}
	}
	// An animated document with no sound is a GIF, whatever else it claims.
	if animated && (kind == MediaVideo || kind == MediaFile) {
		kind = MediaGIF
	}
	return kind, name, size
}

// peerFromUser maps a tg.User. A bot is listed apart from a person: the UI
// shows them in different places.
func peerFromUser(u *tg.User) Peer {
	kind := PeerUser
	if u.Bot {
		kind = PeerBot
	}
	title := u.FirstName
	if u.LastName != "" {
		if title != "" {
			title += " "
		}
		title += u.LastName
	}
	if title == "" {
		title = u.Username
	}
	return Peer{
		Kind: kind, ID: u.ID, AccessHash: u.AccessHash,
		Title: title, Username: u.Username, Thumb: userThumb(u),
	}
}

// peerFromChat maps a legacy (basic) group.
func peerFromChat(c *tg.Chat) Peer {
	readOnly := c.Left || c.Deactivated
	if b, ok := c.GetDefaultBannedRights(); ok && b.SendMessages && !c.Creator {
		readOnly = true
	}
	return Peer{Kind: PeerGroup, ID: c.ID, Title: c.Title, ReadOnly: readOnly, Thumb: chatThumb(c.Photo)}
}

// peerFromChannel maps a channel, supergroup or forum, which Telegram models
// as one type with flags.
func peerFromChannel(c *tg.Channel) Peer {
	kind := PeerChannel
	if c.Megagroup {
		kind = PeerGroup
		if c.Forum {
			kind = PeerForum
		}
	}
	return Peer{
		Kind: kind, ID: c.ID, AccessHash: c.AccessHash,
		Title: c.Title, Username: c.Username, ReadOnly: channelReadOnly(c),
		Thumb: chatThumb(c.Photo),
	}
}

// channelReadOnly reports whether Telegram would refuse a message. A broadcast
// channel takes one only from someone who may post; a group refuses one from a
// member it has banned, or from anyone when sending is banned by default.
func channelReadOnly(c *tg.Channel) bool {
	if c.Left {
		return true
	}
	admin, isAdmin := c.GetAdminRights()
	if c.Broadcast {
		return !c.Creator && !(isAdmin && admin.PostMessages)
	}
	if b, ok := c.GetBannedRights(); ok && b.SendMessages {
		return true
	}
	if b, ok := c.GetDefaultBannedRights(); ok && b.SendMessages {
		// An admin is not subject to the default restrictions.
		return !c.Creator && !isAdmin
	}
	return false
}

// inputPeer converts a Peer back into something the API can address.
func (p Peer) inputPeer() tg.InputPeerClass {
	switch p.Kind {
	case PeerChannel, PeerForum:
		return &tg.InputPeerChannel{ChannelID: p.ID, AccessHash: p.AccessHash}
	case PeerGroup:
		// A supergroup carries an access hash; a legacy group does not.
		if p.AccessHash != 0 {
			return &tg.InputPeerChannel{ChannelID: p.ID, AccessHash: p.AccessHash}
		}
		return &tg.InputPeerChat{ChatID: p.ID}
	default:
		return &tg.InputPeerUser{UserID: p.ID, AccessHash: p.AccessHash}
	}
}

// messageFrom flattens a tg.MessageClass. A service message is kept and marked
// so the caller's "newer than" cursor can move past it.
func messageFrom(m tg.MessageClass, names map[string]string) (Message, bool) {
	if svc, ok := m.(*tg.MessageService); ok {
		return Message{ID: svc.ID, Date: svc.Date, Out: svc.Out, Service: true}, true
	}
	msg, ok := m.(*tg.Message)
	if !ok {
		return Message{}, false
	}
	out := Message{ID: msg.ID, Date: msg.Date, Text: msg.Message, Out: msg.Out}
	if media, has := msg.GetMedia(); has {
		out.Media, out.MediaName, out.MediaSize = mediaOf(media)
	}
	if markup, has := msg.GetReplyMarkup(); has {
		out.Buttons = buttonsOf(markup)
	}
	if v, ok := msg.GetViews(); ok {
		out.Views = v
	}
	out.Reactions = reactionsOf(msg)
	if h, ok := msg.GetReplyTo(); ok {
		if hdr, ok := h.(*tg.MessageReplyHeader); ok {
			out.ReplyTo, _ = hdr.GetReplyToMsgID()
			out.ReplyQuote, _ = hdr.GetQuoteText()
		}
	}
	if from, ok := msg.GetFromID(); ok {
		switch f := from.(type) {
		case *tg.PeerUser:
			out.FromID = f.UserID
		case *tg.PeerChannel:
			out.FromID = f.ChannelID
		case *tg.PeerChat:
			out.FromID = f.ChatID
		}
		// Keyed by type AND id: a user and a channel can share a number.
		out.From = names[peerKey(from)]
	}
	return out, true
}

// senderNames indexes the entities a reply carries, by the same key peerKey
// produces, so a message's sender can be named without another request.
// reactionsOf flattens Telegram's reaction counts. Custom (premium) emoji have
// no unicode form, so they are counted under a single placeholder rather than
// dropped -- a post with only custom reactions should not look like it has
// none.
func reactionsOf(msg *tg.Message) []Reaction {
	rs, ok := msg.GetReactions()
	if !ok {
		return nil
	}
	return reactionCounts(rs)
}

// reactionCounts is the same flattening for counts that arrive on their own,
// in an update rather than attached to a message.
func reactionCounts(rs tg.MessageReactions) []Reaction {
	out := make([]Reaction, 0, len(rs.Results))
	for _, r := range rs.Results {
		emoji := "\u2b50"
		if e, ok := r.Reaction.(*tg.ReactionEmoji); ok {
			emoji = e.Emoticon
		}
		_, chosen := r.GetChosenOrder()
		out = append(out, Reaction{Emoji: emoji, Count: r.Count, Chosen: chosen})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func senderNames(users []tg.UserClass, chats []tg.ChatClass) map[string]string {
	names := make(map[string]string, len(users)+len(chats))
	for _, u := range users {
		if uu, ok := u.(*tg.User); ok {
			names["u"+itoa64(uu.ID)] = peerFromUser(uu).Title
		}
	}
	for _, c := range chats {
		switch cc := c.(type) {
		case *tg.Chat:
			names["c"+itoa64(cc.ID)] = cc.Title
		case *tg.Channel:
			names["h"+itoa64(cc.ID)] = cc.Title
		}
	}
	return names
}

// userThumb / chatThumb lift the stripped thumbnail Telegram already sent and
// expand it into a real JPEG.
//
// The stripped form is the image with a fixed JPEG header removed -- a hundred
// or two hundred bytes on the wire. The header itself is a constant that lives
// in gotd and never travels, so expanding costs nothing but the bytes already
// received. Anything unreadable is simply dropped: a conversation with no
// picture falls back to its initial, which is what it did before.
func userThumb(u *tg.User) []byte {
	p, ok := u.GetPhoto()
	if !ok {
		return nil
	}
	up, ok := p.(*tg.UserProfilePhoto)
	if !ok {
		return nil
	}
	raw, ok := up.GetStrippedThumb()
	if !ok {
		return nil
	}
	return expandThumb(raw)
}

func chatThumb(p tg.ChatPhotoClass) []byte {
	cp, ok := p.(*tg.ChatPhoto)
	if !ok {
		return nil
	}
	raw, ok := cp.GetStrippedThumb()
	if !ok {
		return nil
	}
	return expandThumb(raw)
}

func expandThumb(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	jpg, err := thumbnail.Expand(raw)
	if err != nil {
		return nil
	}
	return jpg
}
