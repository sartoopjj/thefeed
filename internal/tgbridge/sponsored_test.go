package tgbridge

import (
	"bytes"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
)

// mockBridge is a connected bridge whose every RPC goes to mock.
func mockBridge(t *testing.T) (*Bridge, *tgmock.Mock) {
	t.Helper()
	mock := tgmock.New(t)
	b := NewBridge()
	b.client = new(telegram.Client)
	b.api = tg.NewClient(mock)
	t.Cleanup(func() {
		if !mock.AllWereMet() {
			t.Error("an expected RPC was never made")
		}
	})
	return b, mock
}

var adChannel = Peer{Kind: PeerChannel, ID: 1001, AccessHash: 77}

func TestSponsoredOnlyAskedInBroadcastChannels(t *testing.T) {
	for kind, want := range map[PeerKind]bool{
		PeerChannel: true, PeerGroup: false, PeerForum: false, PeerUser: false, PeerBot: true,
	} {
		if got := WantsSponsored(Peer{Kind: kind}); got != want {
			t.Errorf("%s: WantsSponsored = %v", kind, got)
		}
	}
	// No expectation set: any RPC here fails the test.
	b, _ := mockBridge(t)
	if ads, err := b.SponsoredMessages(t.Context(), Peer{Kind: PeerGroup, ID: 5}); err != nil || ads != nil {
		t.Fatalf("group: %v %v", ads, err)
	}
}

func TestSponsoredFetchKeepsWhatTheUIShows(t *testing.T) {
	b, mock := mockBridge(t)
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesGetSponsoredMessagesRequest)
		if !ok {
			t.Fatalf("sent %T", in)
		}
		p, ok := req.Peer.(*tg.InputPeerChannel)
		if !ok || p.ChannelID != 1001 || p.AccessHash != 77 {
			t.Errorf("asked about %#v", req.Peer)
		}
	}).ThenResult(&tg.MessagesSponsoredMessages{Messages: []tg.SponsoredMessage{
		{RandomID: []byte{1, 2}, Title: "Shop", Message: "Buy", URL: "https://t.me/shop",
			ButtonText: "Open", CanReport: true, SponsorInfo: "Shop Ltd", AdditionalInfo: "ad id 7"},
		{RandomID: nil, Title: "unidentifiable, cannot be reported back"},
		{RandomID: []byte{3}, Title: "Bot", Recommended: true},
	}})
	ads, err := b.SponsoredMessages(t.Context(), adChannel)
	if err != nil {
		t.Fatal(err)
	}
	if len(ads) != 2 {
		t.Fatalf("got %d ads, want 2 (the one with no id dropped)", len(ads))
	}
	a := ads[0]
	if !bytes.Equal(a.RandomID, []byte{1, 2}) || a.Title != "Shop" || a.Text != "Buy" ||
		a.URL != "https://t.me/shop" || a.Button != "Open" || !a.CanReport ||
		a.SponsorInfo != "Shop Ltd" || a.AdditionalInfo != "ad id 7" || a.Recommended {
		t.Errorf("first ad: %+v", a)
	}
	if !ads[1].Recommended {
		t.Error("recommended flag lost")
	}
}

func TestSponsoredEmptyAnswerIsNotAnError(t *testing.T) {
	b, mock := mockBridge(t)
	mock.Expect().ThenResult(&tg.MessagesSponsoredMessagesEmpty{})
	ads, err := b.SponsoredMessages(t.Context(), adChannel)
	if err != nil || len(ads) != 0 {
		t.Fatalf("%v %v", ads, err)
	}
}

func TestSponsoredViewAndClickSendTheAdsID(t *testing.T) {
	b, mock := mockBridge(t)
	id := []byte{9, 8, 7}
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesViewSponsoredMessageRequest)
		if !ok || !bytes.Equal(req.RandomID, id) {
			t.Errorf("view sent %#v", in)
		}
	}).ThenTrue()
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesClickSponsoredMessageRequest)
		if !ok || !bytes.Equal(req.RandomID, id) || req.Media || req.Fullscreen {
			t.Errorf("click sent %#v", in)
		}
	}).ThenTrue()
	if ok, err := b.ViewSponsored(t.Context(), id); err != nil || !ok {
		t.Fatalf("view: %v %v", ok, err)
	}
	if ok, err := b.ClickSponsored(t.Context(), id); err != nil || !ok {
		t.Fatalf("click: %v %v", ok, err)
	}
}

// Reporting is a dialogue: no option first, then the reason the user picked.
func TestSponsoredReportFlow(t *testing.T) {
	b, mock := mockBridge(t)
	id := []byte{4}
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesReportSponsoredMessageRequest)
		if !ok || !bytes.Equal(req.RandomID, id) || req.Option == nil || len(req.Option) != 0 {
			t.Errorf("first report sent %#v", in)
		}
	}).ThenResult(&tg.ChannelsSponsoredMessageReportResultChooseOption{
		Title: "Why?", Options: []tg.SponsoredMessageReportOption{{Text: "Spam", Option: []byte{0x51}}},
	})
	mock.ExpectFunc(func(in bin.Encoder) {
		req, ok := in.(*tg.MessagesReportSponsoredMessageRequest)
		if !ok || !bytes.Equal(req.Option, []byte{0x51}) {
			t.Errorf("second report sent %#v", in)
		}
	}).ThenResult(&tg.ChannelsSponsoredMessageReportResultReported{})
	mock.Expect().ThenResult(&tg.ChannelsSponsoredMessageReportResultAdsHidden{})

	r, err := b.ReportSponsored(t.Context(), id, nil)
	if err != nil || r.Title != "Why?" || len(r.Options) != 1 || r.Options[0].Text != "Spam" || r.Done != "" {
		t.Fatalf("first step: %+v %v", r, err)
	}
	r, err = b.ReportSponsored(t.Context(), id, r.Options[0].Option)
	if err != nil || r.Done != "reported" {
		t.Fatalf("second step: %+v %v", r, err)
	}
	r, err = b.ReportSponsored(t.Context(), id, []byte{1})
	if err != nil || r.Done != "hidden" {
		t.Fatalf("hidden: %+v %v", r, err)
	}
}

func TestSponsoredNeedsAConnection(t *testing.T) {
	b := NewBridge()
	ctx := t.Context()
	if _, err := b.SponsoredMessages(ctx, adChannel); err != ErrNotConnected {
		t.Errorf("SponsoredMessages: %v", err)
	}
	if _, err := b.ViewSponsored(ctx, []byte{1}); err != ErrNotConnected {
		t.Errorf("ViewSponsored: %v", err)
	}
	if _, err := b.ClickSponsored(ctx, []byte{1}); err != ErrNotConnected {
		t.Errorf("ClickSponsored: %v", err)
	}
	if _, err := b.ReportSponsored(ctx, []byte{1}, nil); err != ErrNotConnected {
		t.Errorf("ReportSponsored: %v", err)
	}
}
