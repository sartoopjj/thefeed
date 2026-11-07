package tgbridge

import (
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestInviteHash(t *testing.T) {
	for in, want := range map[string]string{
		"https://t.me/+AbCdEfGh12345678":          "AbCdEfGh12345678",
		"t.me/+AbCdEfGh12345678":                  "AbCdEfGh12345678",
		"https://telegram.me/joinchat/AAAAAEkk2W": "AAAAAEkk2W",
		"https://telegram.dog/+AbCdEfGh123":       "AbCdEfGh123",
		"http://www.t.me/+AbCdEfGh123/":           "AbCdEfGh123",
		"tg://join?invite=AbCdEfGh123":            "AbCdEfGh123",
		"+AbCdEfGh123":                            "AbCdEfGh123",
		"  https://t.me/+AbCdEfGh123  ":           "AbCdEfGh123",
	} {
		if got, ok := InviteHash(in); !ok || got != want {
			t.Errorf("InviteHash(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	// Public usernames, other sites and junk are not invites.
	// t.me/+NUMBER is a phone-number contact link, never an invite.
	for _, in := range []string{"https://t.me/+989121234567", "+15551234567", "t.me/+4915112345678",
		"@durov", "https://t.me/durov", "https://example.com/+AbCdEfGh123",
		"https://t.me/+short", "tg://resolve?domain=durov", "t.me/+bad hash!", ""} {
		if h, ok := InviteHash(in); ok {
			t.Errorf("InviteHash(%q) = %q, accepted", in, h)
		}
	}
}

func TestCheckInviteBeforeJoining(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		if req, ok := in.(*tg.MessagesCheckChatInviteRequest); !ok || req.Hash != "AbCdEfGh123" {
			t.Fatalf("sent %#v", in)
		}
	}).ThenResult(&tg.ChatInvite{Title: "Secret club", About: "members only", ParticipantsCount: 321,
		Broadcast: true, Channel: true, RequestNeeded: true, Photo: &tg.PhotoEmpty{}})
	inv, err := b.CheckInvite(t.Context(), "AbCdEfGh123")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Title != "Secret club" || inv.Members != 321 || !inv.Channel || !inv.NeedsRequest || inv.Member != nil {
		t.Fatalf("got %+v", inv)
	}
	// Already a member: the conversation comes back to open directly.
	mock.ExpectFunc(func(bin.Encoder) {}).ThenResult(&tg.ChatInviteAlready{
		Chat: &tg.Channel{ID: 55, AccessHash: 66, Title: "Secret club", Broadcast: true, Photo: &tg.ChatPhotoEmpty{}}})
	inv, err = b.CheckInvite(t.Context(), "AbCdEfGh123")
	if err != nil || inv.Member == nil || inv.Member.ID != 55 || inv.Member.AccessHash != 66 || inv.Peek {
		t.Fatalf("already a member: %+v %v", inv, err)
	}
	// A peek lets the account read before joining: openable, but NOT a member.
	mock.ExpectFunc(func(bin.Encoder) {}).ThenResult(&tg.ChatInvitePeek{Expires: 9,
		Chat: &tg.Channel{ID: 55, AccessHash: 66, Title: "Secret club", Broadcast: true, Photo: &tg.ChatPhotoEmpty{}}})
	inv, err = b.CheckInvite(t.Context(), "AbCdEfGh123")
	if err != nil || inv.Member == nil || !inv.Peek {
		t.Fatalf("peek: %+v %v", inv, err)
	}
}

func TestJoinInvite(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		if req, ok := in.(*tg.MessagesImportChatInviteRequest); !ok || req.Hash != "AbCdEfGh123" {
			t.Fatalf("sent %#v", in)
		}
	}).ThenResult(&tg.MessagesChatInviteJoinResultOk{Updates: &tg.Updates{
		Chats: []tg.ChatClass{&tg.Channel{ID: 55, AccessHash: 66, Title: "Secret club", Broadcast: true, Photo: &tg.ChatPhotoEmpty{}}}}})
	p, err := b.JoinInvite(t.Context(), "AbCdEfGh123")
	if err != nil || p.ID != 55 || p.AccessHash != 66 || p.Kind != PeerChannel {
		t.Fatalf("got %+v %v", p, err)
	}
	// A join Telegram only completes in its own app is refused, not faked.
	mock.ExpectFunc(func(bin.Encoder) {}).ThenResult(&tg.MessagesChatInviteJoinResultWebView{BotID: 1})
	if _, err := b.JoinInvite(t.Context(), "AbCdEfGh123"); !errors.Is(err, ErrInviteNeedsApp) {
		t.Fatalf("web view join: %v", err)
	}
}
