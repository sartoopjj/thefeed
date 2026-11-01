package tgbridge

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestForwardSendsOneCallAndReturnsTheCopy(t *testing.T) {
	b, mock := mockBridge(t)
	fwd := tg.MessageFwdHeader{Date: 1}
	fwd.SetFromID(&tg.PeerChannel{ChannelID: 100})
	copyMsg := &tg.Message{ID: 77, Date: 5, Message: "post", Out: true, PeerID: &tg.PeerUser{UserID: 8}}
	copyMsg.SetFwdFrom(fwd)
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesForwardMessagesRequest)
		if !ok {
			t.Fatalf("sent %T", in)
		}
		if f, ok := req.FromPeer.(*tg.InputPeerChannel); !ok || f.ChannelID != 100 || f.AccessHash != 1 {
			t.Errorf("from %#v", req.FromPeer)
		}
		if to, ok := req.ToPeer.(*tg.InputPeerUser); !ok || to.UserID != 8 || to.AccessHash != 9 {
			t.Errorf("to %#v", req.ToPeer)
		}
		if req.DropAuthor || req.DropMediaCaptions || req.Silent {
			t.Errorf("options set without asking: %+v", req)
		}
		if len(req.ID) != 1 || req.ID[0] != 5 || len(req.RandomID) != 1 || req.RandomID[0] != 123 {
			t.Errorf("ids %v random %v", req.ID, req.RandomID)
		}
	}).ThenResult(&tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: 77, RandomID: 123},
			&tg.UpdateNewMessage{Message: copyMsg},
		},
		Chats: []tg.ChatClass{&tg.Channel{ID: 100, AccessHash: 1, Title: "Space Watch", Photo: &tg.ChatPhotoEmpty{}}},
	})
	got, err := b.ForwardMessages(t.Context(),
		Peer{Kind: PeerChannel, ID: 100, AccessHash: 1}, []int{5}, []int64{123},
		Peer{Kind: PeerUser, ID: 8, AccessHash: 9}, ForwardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 77 || !got[0].Forwarded || got[0].Fwd != "Space Watch" || !got[0].Out {
		t.Errorf("copy: %+v", got)
	}
}

func TestForwardOptionsRideTheSameCall(t *testing.T) {
	for _, opt := range []ForwardOptions{{HideSender: true}, {HideCaptions: true}, {Silent: true}} {
		b, mock := mockBridge(t)
		want := opt
		mock.ExpectFunc(func(in bin.Encoder) {
			req := in.(*tg.MessagesForwardMessagesRequest)
			if req.DropAuthor != (want.HideSender || want.HideCaptions) || req.DropMediaCaptions != want.HideCaptions || req.Silent != want.Silent {
				t.Errorf("%+v sent as drop_author=%v drop_captions=%v silent=%v",
					want, req.DropAuthor, req.DropMediaCaptions, req.Silent)
			}
		}).ThenResult(&tg.Updates{})
		if _, err := b.ForwardMessages(t.Context(), Peer{Kind: PeerUser, ID: 1, AccessHash: 1}, []int{1}, []int64{1},
			Peer{Kind: PeerUser, ID: 2, AccessHash: 2}, opt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestForwardNeedsAConnection(t *testing.T) {
	if _, err := NewBridge().ForwardMessages(t.Context(), Peer{Kind: PeerUser, ID: 1}, []int{1}, []int64{1},
		Peer{Kind: PeerUser, ID: 2}, ForwardOptions{}); err != ErrNotConnected {
		t.Errorf("not connected: %v", err)
	}
}

// The header names the source if Telegram sent it along, else whatever name a
// hidden sender or a channel's signature gives; with none it is still marked.
func TestForwardedFromName(t *testing.T) {
	names := map[string]string{"u5": "Alex"}
	known := tg.MessageFwdHeader{}
	known.SetFromID(&tg.PeerUser{UserID: 5})
	hidden := tg.MessageFwdHeader{}
	hidden.SetFromName("Mia")
	signed := tg.MessageFwdHeader{}
	signed.SetFromID(&tg.PeerChannel{ChannelID: 9})
	signed.SetPostAuthor("Editor")
	for _, c := range []struct {
		h    tg.MessageFwdHeader
		want string
	}{{known, "Alex"}, {hidden, "Mia"}, {signed, "Editor"}, {tg.MessageFwdHeader{}, ""}} {
		if got := fwdName(c.h, names); got != c.want {
			t.Errorf("%+v: %q, want %q", c.h, got, c.want)
		}
	}
	m := &tg.Message{ID: 1, Message: "x", PeerID: &tg.PeerUser{UserID: 1}}
	m.SetFwdFrom(tg.MessageFwdHeader{})
	if got, _ := messageFrom(m, nil); !got.Forwarded {
		t.Error("a forward with no name lost its mark")
	}
	plain, _ := messageFrom(&tg.Message{ID: 2, Message: "x", PeerID: &tg.PeerUser{UserID: 1}}, nil)
	if plain.Forwarded {
		t.Error("an ordinary message is marked forwarded")
	}
}

// Content protection arrives on the conversation and on single messages.
func TestProtectedContent(t *testing.T) {
	if p := peerFromChannel(&tg.Channel{ID: 1, Noforwards: true, Photo: &tg.ChatPhotoEmpty{}}); !p.Protected {
		t.Error("channel")
	}
	if p := peerFromChat(&tg.Chat{ID: 1, Noforwards: true, Photo: &tg.ChatPhotoEmpty{}}); !p.Protected {
		t.Error("group")
	}
	if p := peerFromChannel(&tg.Channel{ID: 1, Photo: &tg.ChatPhotoEmpty{}}); p.Protected {
		t.Error("unprotected channel marked")
	}
	m, _ := messageFrom(&tg.Message{ID: 1, Noforwards: true, PeerID: &tg.PeerUser{UserID: 1}}, nil)
	if !m.Protected {
		t.Error("message")
	}
}
