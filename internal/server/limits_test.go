package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

func TestLoadLimitsFromFile(t *testing.T) {
	tempDir := t.TempDir()

	// Test public channels & X accounts parsing
	publicFile := filepath.Join(tempDir, "channels.txt")
	publicContent := `@MoradVaisiMedia --dns-media-max-size 3000 --dns-audio-max-size 33000 --msg-limit 20
# Some comment
@SimpleChannel --msg-limit 10
x/some_x_account --dns-media-max-size 1000 --msg-limit 5
`
	if err := os.WriteFile(publicFile, []byte(publicContent), 0644); err != nil {
		t.Fatal(err)
	}

	limits, err := loadLimitsFromFile(publicFile, false)
	if err != nil {
		t.Fatalf("loadLimitsFromFile failed: %v", err)
	}

	// 1. Check @MoradVaisiMedia
	morad, ok := limits["moradvaisimedia"]
	if !ok {
		t.Errorf("expected limits for 'moradvaisimedia'")
	} else {
		if morad.MediaSize != 3000*1024 {
			t.Errorf("expected MediaSize 3072000, got %d", morad.MediaSize)
		}
		if morad.AudioSize != 33000*1024 {
			t.Errorf("expected AudioSize 33792000, got %d", morad.AudioSize)
		}
		if morad.MsgLimit != 20 {
			t.Errorf("expected MsgLimit 20, got %d", morad.MsgLimit)
		}
	}

	// 2. Check @SimpleChannel
	simple, ok := limits["simplechannel"]
	if !ok {
		t.Errorf("expected limits for 'simplechannel'")
	} else {
		if simple.MediaSize != -1 {
			t.Errorf("expected MediaSize -1, got %d", simple.MediaSize)
		}
		if simple.MsgLimit != 10 {
			t.Errorf("expected MsgLimit 10, got %d", simple.MsgLimit)
		}
	}

	// 3. Check x/some_x_account
	someX, ok := limits["some_x_account"]
	if !ok {
		t.Errorf("expected limits for 'some_x_account'")
	} else {
		if someX.MediaSize != 1000*1024 {
			t.Errorf("expected MediaSize 1024000, got %d", someX.MediaSize)
		}
		if someX.MsgLimit != 5 {
			t.Errorf("expected MsgLimit 5, got %d", someX.MsgLimit)
		}
	}

	// Test private channels parsing
	privateFile := filepath.Join(tempDir, "private_channels.txt")
	privateContent := `https://t.me/+aBcDeF123-_ --msg-limit 25
`
	if err := os.WriteFile(privateFile, []byte(privateContent), 0644); err != nil {
		t.Fatal(err)
	}

	privLimits, err := loadLimitsFromFile(privateFile, true)
	if err != nil {
		t.Fatalf("loadLimitsFromFile (private) failed: %v", err)
	}

	priv, ok := privLimits["aBcDeF123-_"]
	if !ok {
		t.Errorf("expected limits for 'aBcDeF123-_'")
	} else {
		if priv.MsgLimit != 25 {
			t.Errorf("expected MsgLimit 25, got %d", priv.MsgLimit)
		}
	}
}

func TestPublicReaderChannelMsgLimitOverride(t *testing.T) {
	// Start mock http server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		htmlContent := `
		<html><body>
		<div class="tgme_header_title">Test Title</div>
		<div class="tgme_widget_message" data-post="test/1"><div class="tgme_widget_message_text">one</div></div>
		<div class="tgme_widget_message" data-post="test/2"><div class="tgme_widget_message_text">two</div></div>
		<div class="tgme_widget_message" data-post="test/3"><div class="tgme_widget_message_text">three</div></div>
		<div class="tgme_widget_message" data-post="test/4"><div class="tgme_widget_message_text">four</div></div>
		<div class="tgme_widget_message" data-post="test/5"><div class="tgme_widget_message_text">five</div></div>
		</body></html>
		`
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(htmlContent))
	}))
	defer ts.Close()

	// 2 channels: "overridden" and "default_limit"
	channels := []string{"overridden", "default_limit"}
	feed := NewFeed(channels)

	// Set channel overrides
	limits := map[string]ChannelLimits{
		"overridden": {
			MsgLimit: 2,
		},
	}

	// Global msg limit is 3, but overridden hasMsgLimit = 2
	pr := NewPublicReader(channels, feed, 3, 1, limits)
	pr.baseURLs = []string{ts.URL}

	// Trigger fetch
	pr.fetchAll(context.Background())

	// Assert messages for channel 1 ("overridden"): should have exactly 2 messages
	data1, err := feed.GetBlock(1, 0)
	if err != nil {
		t.Fatalf("feed.GetBlock(1, 0) failed: %v", err)
	}
	decompressed1, err := protocol.DecompressMessages(data1)
	if err != nil {
		t.Fatalf("DecompressMessages channel 1 failed: %v", err)
	}
	msgs1, err := protocol.ParseMessages(decompressed1)
	if err != nil {
		t.Fatalf("ParseMessages channel 1 failed: %v", err)
	}
	if len(msgs1) != 2 {
		t.Errorf("expected overridden channel to have 2 messages, got %d", len(msgs1))
	}

	// Assert messages for channel 2 ("default_limit"): should have exactly 3 messages
	data2, err := feed.GetBlock(2, 0)
	if err != nil {
		t.Fatalf("feed.GetBlock(2, 0) failed: %v", err)
	}
	decompressed2, err := protocol.DecompressMessages(data2)
	if err != nil {
		t.Fatalf("DecompressMessages channel 2 failed: %v", err)
	}
	msgs2, err := protocol.ParseMessages(decompressed2)
	if err != nil {
		t.Fatalf("ParseMessages channel 2 failed: %v", err)
	}
	if len(msgs2) != 3 {
		t.Errorf("expected default channel to have 3 messages, got %d", len(msgs2))
	}
}
