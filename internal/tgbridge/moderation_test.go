package tgbridge

import (
	"bytes"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

var abuser = Peer{Kind: PeerUser, ID: 4242, AccessHash: 99}

// The whole dialogue, as Telegram leads it: reasons, then a comment, then done.
// Each step sends back exactly what the previous answer handed out.
func TestReportWalksTelegramsDialogue(t *testing.T) {
	b, mock := mockBridge(t)
	expect := func(option []byte, comment string) func(bin.Encoder) {
		return func(in bin.Encoder) {
			req, ok := in.(*tg.MessagesReportRequest)
			if !ok {
				t.Fatalf("sent %T", in)
			}
			p, ok := req.Peer.(*tg.InputPeerUser)
			if !ok || p.UserID != 4242 || p.AccessHash != 99 {
				t.Errorf("reported %#v", req.Peer)
			}
			if len(req.ID) != 1 || req.ID[0] != 7 {
				t.Errorf("reported messages %v", req.ID)
			}
			if !bytes.Equal(req.Option, option) || req.Message != comment {
				t.Errorf("sent option %q comment %q, want %q %q", req.Option, req.Message, option, comment)
			}
		}
	}
	mock.ExpectFunc(expect([]byte{}, "")).ThenResult(&tg.ReportResultChooseOption{
		Title:   "Why?",
		Options: []tg.MessageReportOption{{Text: "Spam", Option: []byte("s")}, {Text: "Violence", Option: []byte("v")}},
	})
	mock.ExpectFunc(expect([]byte("v"), "")).ThenResult(&tg.ReportResultAddComment{Optional: true, Option: []byte("v2")})
	mock.ExpectFunc(expect([]byte("v2"), "threats")).ThenResult(&tg.ReportResultReported{})

	r, err := b.ReportMessages(t.Context(), abuser, []int{7}, nil, "")
	if err != nil || r.Title != "Why?" || len(r.Options) != 2 || r.Options[1].Text != "Violence" || r.Done {
		t.Fatalf("step 1: %+v %v", r, err)
	}
	r, err = b.ReportMessages(t.Context(), abuser, []int{7}, r.Options[1].Option, "")
	if err != nil || !r.AskComment || !r.CommentOptional || string(r.Option) != "v2" || r.Done {
		t.Fatalf("step 2: %+v %v", r, err)
	}
	r, err = b.ReportMessages(t.Context(), abuser, []int{7}, r.Option, "threats")
	if err != nil || !r.Done {
		t.Fatalf("step 3: %+v %v", r, err)
	}
}

func TestSetBlockedBlocksAndUnblocks(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.ContactsBlockRequest)
		if !ok {
			t.Fatalf("sent %T", in)
		}
		if p, ok := req.ID.(*tg.InputPeerUser); !ok || p.UserID != 4242 || p.AccessHash != 99 {
			t.Errorf("blocked %#v", req.ID)
		}
		if req.MyStoriesFrom {
			t.Error("blocked from stories only, not from messaging")
		}
	}).ThenTrue()
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.ContactsUnblockRequest)
		if !ok {
			t.Fatalf("sent %T", in)
		}
		if p, ok := req.ID.(*tg.InputPeerUser); !ok || p.UserID != 4242 {
			t.Errorf("unblocked %#v", req.ID)
		}
	}).ThenTrue()
	if err := b.SetBlocked(t.Context(), abuser, true); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBlocked(t.Context(), abuser, false); err != nil {
		t.Fatal(err)
	}
}

// Blocking and unblocking on another device reach us in the difference we
// already ask for, so no query is spent to notice them.
func TestBlockUpdatesFromOtherDevices(t *testing.T) {
	users := []tg.UserClass{&tg.User{ID: 4242, AccessHash: 99, FirstName: "Known"}}
	h := otherUpdates([]tg.UpdateClass{
		&tg.UpdatePeerBlocked{PeerID: &tg.PeerUser{UserID: 4242}, Blocked: false},
		&tg.UpdatePeerBlocked{PeerID: &tg.PeerUser{UserID: 777}, Blocked: true},
		// Stories only: they can still message, so nothing changes here.
		&tg.UpdatePeerBlocked{PeerID: &tg.PeerUser{UserID: 555}, BlockedMyStoriesFrom: true},
	}, users, nil, NotifyDefaults{})
	if len(h.Blocks) != 2 {
		t.Fatalf("got %d block changes, want 2 (stories-only is not one): %+v", len(h.Blocks), h.Blocks)
	}
	if b := h.Blocks[0]; b.Blocked || b.Peer.ID != 4242 || b.Peer.AccessHash != 99 {
		t.Errorf("unblock of a known user: %+v", b)
	}
	if b := h.Blocks[1]; !b.Blocked || b.Peer.ID != 777 || b.Peer.Kind != PeerUser {
		t.Errorf("block without the entity: %+v", b)
	}
}

func TestBlockedListReadsTelegramsList(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.ContactsGetBlockedRequest)
		if !ok {
			t.Fatalf("sent %T", in)
		}
		if req.MyStoriesFrom || req.Limit != 100 {
			t.Errorf("asked %+v", req)
		}
	}).ThenResult(&tg.ContactsBlockedSlice{
		Count: 250,
		Blocked: []tg.PeerBlocked{
			{PeerID: &tg.PeerUser{UserID: 4242}, Date: 1700000000},
			{PeerID: &tg.PeerUser{UserID: 5}, Date: 1600000000},
			{PeerID: &tg.PeerChannel{ChannelID: 9}}, // not something Block applies to here
		},
		Users: []tg.UserClass{
			&tg.User{ID: 4242, AccessHash: 99, FirstName: "Abuser", Username: "abuser"},
			&tg.User{ID: 5, AccessHash: 3, Deleted: true},
		},
	})
	rows, total, err := b.Blocked(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 250 || len(rows) != 2 {
		t.Fatalf("got %+v total %d", rows, total)
	}
	if r := rows[0]; r.Peer.ID != 4242 || r.Peer.AccessHash != 99 || r.Peer.Title == "" || r.Peer.Username != "abuser" || r.Date != 1700000000 || r.Deleted {
		t.Errorf("row 1: %+v", r)
	}
	// A deleted account has no name: the UI says so instead of a bare id.
	if r := rows[1]; !r.Deleted || r.Peer.AccessHash != 3 {
		t.Errorf("row 2: %+v", r)
	}
}
