package tgbridge

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/gotd/td/tg"
)

// ErrInviteNeedsApp is a join Telegram will only complete in its own app
// (a bot verification page). There is nothing to show it in here.
var ErrInviteNeedsApp = errors.New("tgbridge: this invite needs the Telegram app")

// Invite is what a private invite link leads to, read before joining.
type Invite struct {
	Title        string `json:"title"`
	About        string `json:"about,omitempty"`
	Members      int    `json:"members,omitempty"`
	Channel      bool   `json:"channel,omitempty"` // a broadcast channel, not a group
	NeedsRequest bool   `json:"needsRequest,omitempty"`
	Thumb        []byte `json:"thumb,omitempty"`
	// Member is set when the conversation can be opened without joining:
	// the account is in it, or (Peek) may read it for a while before deciding.
	Member *Peer `json:"member,omitempty"`
	// Peek: a preview Telegram allows before joining; not a membership.
	Peek bool `json:"peek,omitempty"`
}

var inviteHashRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// phoneRe is a phone number: t.me/+NUMBER is a contact link, not an invite.
var phoneRe = regexp.MustCompile(`^[0-9]+$`)

func validInviteHash(h string) bool { return inviteHashRe.MatchString(h) && !phoneRe.MatchString(h) }

// InviteHash pulls the hash out of an invite link: t.me/+HASH,
// t.me/joinchat/HASH (any of Telegram's link hosts), tg://join?invite=HASH,
// or the bare hash. ok is false for anything else, a public @username
// included.
func InviteHash(link string) (string, bool) {
	s := strings.TrimSpace(link)
	if strings.HasPrefix(s, "+") {
		return s[1:], validInviteHash(s[1:])
	}
	if strings.HasPrefix(s, "tg://") {
		u, err := url.Parse(s)
		if err != nil || u.Host != "join" {
			return "", false
		}
		h := u.Query().Get("invite")
		return h, validInviteHash(h)
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", false
	}
	switch strings.TrimPrefix(strings.ToLower(u.Host), "www.") {
	case "t.me", "telegram.me", "telegram.dog":
	default:
		return "", false
	}
	path := strings.Trim(u.Path, "/")
	var h string
	switch {
	case strings.HasPrefix(path, "+"):
		h = path[1:]
	case strings.HasPrefix(path, "joinchat/"):
		h = strings.TrimPrefix(path, "joinchat/")
	default:
		return "", false
	}
	return h, validInviteHash(h)
}

// CheckInvite reads what an invite link leads to, without joining.
func (b *Bridge) CheckInvite(ctx context.Context, hash string) (*Invite, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	res, err := api.MessagesCheckChatInvite(ctx, hash)
	if err != nil {
		return nil, err
	}
	switch r := res.(type) {
	case *tg.ChatInviteAlready:
		return inviteFromChat(r.Chat), nil
	case *tg.ChatInvitePeek:
		inv := inviteFromChat(r.Chat)
		if inv != nil {
			inv.Peek = true
		}
		return inv, nil
	case *tg.ChatInvite:
		return &Invite{
			Title: r.Title, About: r.About, Members: r.ParticipantsCount,
			Channel: r.Broadcast, NeedsRequest: r.RequestNeeded, Thumb: photoThumb(r.Photo),
		}, nil
	}
	return nil, errors.New("tgbridge: unexpected invite answer")
}

// JoinInvite joins through an invite link and returns the conversation.
// A link that needs an admin's approval fails with INVITE_REQUEST_SENT,
// which means the request went out.
func (b *Bridge) JoinInvite(ctx context.Context, hash string) (Peer, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return Peer{}, err
	}
	res, err := api.MessagesImportChatInvite(ctx, hash)
	if err != nil {
		return Peer{}, err
	}
	ok, isOK := res.(*tg.MessagesChatInviteJoinResultOk)
	if !isOK {
		return Peer{}, ErrInviteNeedsApp
	}
	var chats []tg.ChatClass
	switch u := ok.Updates.(type) {
	case *tg.Updates:
		chats = u.Chats
	case *tg.UpdatesCombined:
		chats = u.Chats
	}
	for _, c := range chats {
		if inv := inviteFromChat(c); inv != nil && inv.Member != nil {
			return *inv.Member, nil
		}
	}
	return Peer{}, errors.New("tgbridge: joined, but Telegram did not name the conversation")
}

func inviteFromChat(c tg.ChatClass) *Invite {
	var p Peer
	switch cc := c.(type) {
	case *tg.Channel:
		p = peerFromChannel(cc)
	case *tg.Chat:
		p = peerFromChat(cc)
	default:
		return nil
	}
	return &Invite{Title: p.Title, Thumb: p.Thumb, Channel: p.Kind == PeerChannel, Member: &p}
}

// photoThumb is the tiny preview Telegram sends inside a photo, if any.
func photoThumb(p tg.PhotoClass) []byte {
	ph, ok := p.(*tg.Photo)
	if !ok {
		return nil
	}
	for _, s := range ph.Sizes {
		if st, ok := s.(*tg.PhotoStrippedSize); ok {
			return expandThumb(st.Bytes)
		}
	}
	return nil
}
