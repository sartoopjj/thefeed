package tgbridge

import (
	"context"

	"github.com/gotd/td/tg"
)

// Report is one step of reporting messages. Telegram leads the dialogue: it
// asks for a reason (Title + Options), may ask for a comment (AskComment,
// to be sent back with Option), and finally answers Done.
type Report struct {
	Title           string         `json:"title,omitempty"`
	Options         []ReportOption `json:"options,omitempty"`
	AskComment      bool           `json:"askComment,omitempty"`
	CommentOptional bool           `json:"commentOptional,omitempty"`
	Option          []byte         `json:"option,omitempty"`
	Done            bool           `json:"done,omitempty"`
}

type ReportOption struct {
	Text   string `json:"text"`
	Option []byte `json:"option"`
}

// ReportMessages reports messages to Telegram's moderators. The first call
// goes with no option; each answer says what to send next.
func (b *Bridge) ReportMessages(ctx context.Context, peer Peer, ids []int, option []byte, comment string) (*Report, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	if option == nil {
		option = []byte{}
	}
	res, err := api.MessagesReport(ctx, &tg.MessagesReportRequest{
		Peer: peer.inputPeer(), ID: ids, Option: option, Message: comment,
	})
	if err != nil {
		return nil, err
	}
	return reportFrom(res), nil
}

func reportFrom(res tg.ReportResultClass) *Report {
	switch r := res.(type) {
	case *tg.ReportResultChooseOption:
		out := &Report{Title: r.Title}
		for _, o := range r.Options {
			out.Options = append(out.Options, ReportOption{Text: o.Text, Option: o.Option})
		}
		return out
	case *tg.ReportResultAddComment:
		return &Report{AskComment: true, CommentOptional: r.Optional, Option: r.Option}
	default:
		return &Report{Done: true}
	}
}

// PeerBlock is a person blocked or unblocked, as an update reports it.
type PeerBlock struct {
	Peer    Peer `json:"peer"`
	Blocked bool `json:"blocked"`
}

// BlockedPeer is one row of the blocklist: who, since when (unix time, 0 if
// unknown), and whether the account behind it was deleted.
type BlockedPeer struct {
	Peer    Peer `json:"peer"`
	Date    int  `json:"date,omitempty"`
	Deleted bool `json:"deleted,omitempty"`
}

// Blocked lists the account's blocklist, newest first, at most limit rows,
// and Telegram's count of the whole list.
func (b *Bridge) Blocked(ctx context.Context, limit int) ([]BlockedPeer, int, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, 0, err
	}
	res, err := api.ContactsGetBlocked(ctx, &tg.ContactsGetBlockedRequest{Limit: limit})
	if err != nil {
		return nil, 0, err
	}
	var rows []tg.PeerBlocked
	var users []tg.UserClass
	var chats []tg.ChatClass
	total := 0
	switch r := res.(type) {
	case *tg.ContactsBlocked:
		rows, users, chats, total = r.Blocked, r.Users, r.Chats, len(r.Blocked)
	case *tg.ContactsBlockedSlice:
		rows, users, chats, total = r.Blocked, r.Users, r.Chats, r.Count
	}
	idx := peerIndex(users, chats)
	deleted := map[int64]bool{}
	for _, u := range users {
		if uu, ok := u.(*tg.User); ok && uu.Deleted {
			deleted[uu.ID] = true
		}
	}
	out := make([]BlockedPeer, 0, len(rows))
	for _, row := range rows {
		// People and bots only: those are what Block applies to here.
		pu, ok := row.PeerID.(*tg.PeerUser)
		if !ok {
			continue
		}
		if p, ok := peerOf(row.PeerID, idx); ok {
			out = append(out, BlockedPeer{Peer: p, Date: row.Date, Deleted: deleted[pu.UserID]})
		}
	}
	return out, total, nil
}

// SetBlocked blocks or unblocks a person or bot: a blocked peer can no longer
// message this account. Telegram keeps the list, so it holds on every device.
func (b *Bridge) SetBlocked(ctx context.Context, peer Peer, blocked bool) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	if blocked {
		_, err = api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peer.inputPeer()})
	} else {
		_, err = api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peer.inputPeer()})
	}
	return err
}
