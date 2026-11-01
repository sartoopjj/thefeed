package tgbridge

import (
	"context"

	"github.com/gotd/td/tg"
)

// Sponsored messages are Telegram's ads in channels. The API terms require
// every client that shows channel content to show them and to report views and
// clicks, so they are fetched, shown and reported -- as text, like everything
// else here. They never arrive with history: they are a separate call.
//
// RandomID and report options are opaque bytes Telegram hands out and wants
// back unchanged; JSON carries them as base64.
type Sponsored struct {
	RandomID       []byte `json:"randomId"`
	Title          string `json:"title,omitempty"`
	Text           string `json:"text,omitempty"`
	URL            string `json:"url,omitempty"`
	Button         string `json:"button,omitempty"`
	Recommended    bool   `json:"recommended,omitempty"`
	CanReport      bool   `json:"canReport,omitempty"`
	SponsorInfo    string `json:"sponsorInfo,omitempty"`
	AdditionalInfo string `json:"additionalInfo,omitempty"`
}

// SponsoredReport is one step of reporting an ad. Telegram either asks the
// user to pick a reason (Title + Options) or answers with the outcome.
type SponsoredReport struct {
	Title   string                  `json:"title,omitempty"`
	Options []SponsoredReportOption `json:"options,omitempty"`
	// Done is "reported" or "hidden" once Telegram has taken the report.
	Done string `json:"done,omitempty"`
}

type SponsoredReportOption struct {
	Text   string `json:"text"`
	Option []byte `json:"option"`
}

// WantsSponsored says whether a conversation can carry ads at all. Only
// broadcast channels do; asking anywhere else spends a round trip on an
// answer that is always empty.
func WantsSponsored(p Peer) bool { return p.Kind == PeerChannel }

// SponsoredMessages fetches the ads Telegram wants shown in a channel. An empty
// answer is the common case and not an error.
func (b *Bridge) SponsoredMessages(ctx context.Context, peer Peer) ([]Sponsored, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	if !WantsSponsored(peer) {
		return nil, nil
	}
	res, err := api.MessagesGetSponsoredMessages(ctx, &tg.MessagesGetSponsoredMessagesRequest{
		Peer: peer.inputPeer(),
	})
	if err != nil {
		return nil, err
	}
	return sponsoredFrom(res), nil
}

func sponsoredFrom(res tg.MessagesSponsoredMessagesClass) []Sponsored {
	m, ok := res.(*tg.MessagesSponsoredMessages)
	if !ok {
		return nil
	}
	out := make([]Sponsored, 0, len(m.Messages))
	for _, s := range m.Messages {
		if len(s.RandomID) == 0 {
			continue
		}
		out = append(out, Sponsored{
			RandomID:       s.RandomID,
			Title:          s.Title,
			Text:           s.Message,
			URL:            s.URL,
			Button:         s.ButtonText,
			Recommended:    s.Recommended,
			CanReport:      s.CanReport,
			SponsorInfo:    s.SponsorInfo,
			AdditionalInfo: s.AdditionalInfo,
		})
	}
	return out
}

// ViewSponsored tells Telegram an ad was seen in full; Telegram answers true.
func (b *Bridge) ViewSponsored(ctx context.Context, randomID []byte) (bool, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return false, err
	}
	return api.MessagesViewSponsoredMessage(ctx, randomID)
}

// ClickSponsored tells Telegram the user opened an ad's link; Telegram
// answers true.
func (b *Bridge) ClickSponsored(ctx context.Context, randomID []byte) (bool, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return false, err
	}
	return api.MessagesClickSponsoredMessage(ctx, &tg.MessagesClickSponsoredMessageRequest{
		RandomID: randomID,
	})
}

// ReportSponsored reports an ad. The first call goes with no option; Telegram
// answers with reasons to choose from, and the chosen one is sent back.
func (b *Bridge) ReportSponsored(ctx context.Context, randomID, option []byte) (*SponsoredReport, error) {
	_, api, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	if option == nil {
		option = []byte{}
	}
	res, err := api.MessagesReportSponsoredMessage(ctx, &tg.MessagesReportSponsoredMessageRequest{
		RandomID: randomID, Option: option,
	})
	if err != nil {
		return nil, err
	}
	return sponsoredReportFrom(res), nil
}

func sponsoredReportFrom(res tg.ChannelsSponsoredMessageReportResultClass) *SponsoredReport {
	switch r := res.(type) {
	case *tg.ChannelsSponsoredMessageReportResultChooseOption:
		out := &SponsoredReport{Title: r.Title}
		for _, o := range r.Options {
			out.Options = append(out.Options, SponsoredReportOption{Text: o.Text, Option: o.Option})
		}
		return out
	case *tg.ChannelsSponsoredMessageReportResultAdsHidden:
		return &SponsoredReport{Done: "hidden"}
	default:
		return &SponsoredReport{Done: "reported"}
	}
}
