package server

import (
	"strings"
	"testing"
)

func TestParseXRSSMessages(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
  <item>
    <title>first</title>
    <link>https://x.com/test/status/1930000000000000001</link>
    <guid>https://x.com/test/status/1930000000000000001</guid>
    <description><![CDATA[<p>Hello <b>world</b></p>]]></description>
    <pubDate>Mon, 30 Mar 2026 04:45:00 +0000</pubDate>
  </item>
  <item>
    <title>second</title>
    <link>https://x.com/test/status/1930000000000000002</link>
    <guid>https://x.com/test/status/1930000000000000002</guid>
    <description><![CDATA[]]></description>
    <pubDate>Mon, 30 Mar 2026 04:46:00 +0000</pubDate>
  </item>
</channel></rss>`)

	msgs, _, err := parseXRSSMessages(body, "test")
	if err != nil {
		t.Fatalf("parseXRSSMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Text != "Hello world" {
		t.Fatalf("msgs[0].Text = %q, want %q", msgs[0].Text, "Hello world")
	}
	if msgs[1].Text != "second" {
		t.Fatalf("msgs[1].Text = %q, want %q", msgs[1].Text, "second")
	}
}

func TestParseXRSSMessages_MediaOnlyFallback(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
  <item>
    <title></title>
    <link>https://x.com/test/status/1930000000000000003</link>
    <guid>https://x.com/test/status/1930000000000000003</guid>
    <description><![CDATA[]]></description>
    <pubDate>Mon, 30 Mar 2026 04:47:00 +0000</pubDate>
  </item>
</channel></rss>`)

	msgs, _, err := parseXRSSMessages(body, "test")
	if err != nil {
		t.Fatalf("parseXRSSMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Text == "" {
		t.Fatalf("expected non-empty fallback text")
	}
}

func TestParseXRSSMessages_AlternateIDFormat(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
  <item>
    <title>alt format</title>
    <link>https://nitter.net/i/web/statuses/1930000000000000004</link>
    <guid>https://nitter.net/i/web/statuses/1930000000000000004</guid>
    <description><![CDATA[hello]]></description>
    <pubDate>Mon, 30 Mar 2026 04:48:00 +0000</pubDate>
  </item>
</channel></rss>`)

	msgs, _, err := parseXRSSMessages(body, "test")
	if err != nil {
		t.Fatalf("parseXRSSMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].ID == 0 {
		t.Fatalf("expected non-zero parsed ID")
	}
}

func TestCombineDisplayChannels(t *testing.T) {
	got := combineDisplayChannels([]string{"tg1", "tg2"}, []string{"userA", "userB"})
	want := []string{"tg1", "tg2", "x/userA", "x/userB"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNormalizeXRSSInstances_Defaults(t *testing.T) {
	got := normalizeXRSSInstances("")
	if len(got) < 1 {
		t.Fatalf("expected defaults, got empty list")
	}
}

func TestNormalizeXRSSInstances_ValidationAndDedup(t *testing.T) {
	got := normalizeXRSSInstances(" https://nitter.net,ftp://bad.example,http://nitter.net,https://nitter.net/path,https://nitter.net ")
	want := []string{"https://nitter.net", "http://nitter.net"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStripXHTML_Retweet(t *testing.T) {
	// Nitter encodes retweets with an RT @handle: prefix inside a paragraph.
	src := `<p>RT <a href="/OrigUser">@OrigUser</a>: This is the original tweet text.</p>`
	got := stripXHTML(src)
	const want = "--------- Repost from @OrigUser ---------\nThis is the original tweet text."
	if got != want {
		t.Fatalf("stripXHTML retweet:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestStripXHTML_RetweetByFormat(t *testing.T) {
	src := `<p>RT by Morad Vaisi (@RezaVaisi): This is reposted content.</p>`
	got := stripXHTML(src)
	const want = "--------- Repost from @RezaVaisi ---------\nThis is reposted content."
	if got != want {
		t.Fatalf("stripXHTML retweet by format:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestStripXHTML_QuoteTweet(t *testing.T) {
	// Nitter wraps the quoted tweet in a <blockquote>.
	src := `<p>My own comment on this.</p><blockquote><p><a href="/QuotedUser">Quoted User</a> (@QuotedUser)<br>The quoted tweet text here.</p></blockquote>`
	got := stripXHTML(src)
	const want = "My own comment on this.\n\n--------- Quote from @QuotedUser ---------\nQuoted User (@QuotedUser)\nThe quoted tweet text here."
	if got != want {
		t.Fatalf("stripXHTML quote tweet:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestParseXRSSMessages_Retweet(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
  <item>
    <title>RT @SomeUser: The original tweet content.</title>
    <link>https://nitter.net/account/status/1930000000000000010</link>
    <guid>https://nitter.net/account/status/1930000000000000010</guid>
    <description><![CDATA[<p>RT <a href="/SomeUser">@SomeUser</a>: The original tweet content.</p>]]></description>
    <pubDate>Mon, 30 Mar 2026 05:00:00 +0000</pubDate>
  </item>
</channel></rss>`)

	msgs, _, err := parseXRSSMessages(body, "myaccount")
	if err != nil {
		t.Fatalf("parseXRSSMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	text := msgs[0].Text
	if !strings.HasPrefix(text, "--------- Repost from @SomeUser ---------") {
		t.Fatalf("retweet not annotated; got: %q", text)
	}
}

func TestParseXRSSMessages_RetweetByFormat(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
  <item>
    <title>RT by Morad Vaisi (@RezaVaisi): Reposted article text.</title>
    <link>https://nitter.net/account/status/1930000000000000011</link>
    <guid>https://nitter.net/account/status/1930000000000000011</guid>
    <description><![CDATA[<p>RT by Morad Vaisi (@RezaVaisi): Reposted article text.</p>]]></description>
    <pubDate>Mon, 30 Mar 2026 05:00:30 +0000</pubDate>
  </item>
</channel></rss>`)

	msgs, _, err := parseXRSSMessages(body, "myaccount")
	if err != nil {
		t.Fatalf("parseXRSSMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	text := msgs[0].Text
	if !strings.HasPrefix(text, "--------- Repost from @RezaVaisi ---------") {
		t.Fatalf("retweet by format not annotated; got: %q", text)
	}
}

func TestParseXRSSMessages_QuoteTweet(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
  <item>
    <title>Quoting someone</title>
    <link>https://nitter.net/account/status/1930000000000000020</link>
    <guid>https://nitter.net/account/status/1930000000000000020</guid>
    <description><![CDATA[<p>My commentary here.</p><blockquote><p><a href="/QuotedPerson">Quoted Person</a> (@QuotedPerson)<br>The original quoted text.</p></blockquote>]]></description>
    <pubDate>Mon, 30 Mar 2026 05:01:00 +0000</pubDate>
  </item>
</channel></rss>`)

	msgs, _, err := parseXRSSMessages(body, "account")
	if err != nil {
		t.Fatalf("parseXRSSMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	text := msgs[0].Text
	if !strings.Contains(text, "--------- Quote from @QuotedPerson ---------") {
		t.Fatalf("quote not annotated with divider; got: %q", text)
	}
	if !strings.HasPrefix(text, "My commentary here.") {
		t.Fatalf("own text not at start; got: %q", text)
	}
}

func TestParseXRSSMessages_PureRetweet(t *testing.T) {
	// Pure retweet: link points to IranIntlTV, but the feed is for RezaVaisi.
	// Nitter does NOT add any RT prefix — the only signal is the username mismatch.
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
  <item>
    <title>Original content by Iran Intl</title>
    <link>https://nitter.net/IranIntlTV/status/2041909367206015289</link>
    <guid>https://nitter.net/IranIntlTV/status/2041909367206015289</guid>
    <description><![CDATA[<p>What follows is a list of fifty-two senior officials...</p><p>✍️ <a href="/RezaVaisi">@RezaVaisi</a></p><p>content.iranintl.com/senior-…</p>]]></description>
    <pubDate>Mon, 30 Mar 2026 06:00:00 +0000</pubDate>
  </item>
</channel></rss>`)

	msgs, _, err := parseXRSSMessages(body, "RezaVaisi")
	if err != nil {
		t.Fatalf("parseXRSSMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	text := msgs[0].Text
	if !strings.HasPrefix(text, "--------- Repost from @IranIntlTV ---------") {
		t.Fatalf("pure retweet not annotated; got: %q", text)
	}
	if !strings.Contains(text, "fifty-two senior officials") {
		t.Fatalf("original content missing; got: %q", text)
	}
}

func TestIsReadableText(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"Hello world", true},
		{"سلام دنیا", true},
		{"", false},
		{"\x00\x01\x02\x03\x04\x05", false},
		{"Y;\x80\x81 $ \x82) \x83\x84", false},
		{"abc\x00\x01\x02\x03\x04\x05\x06\x07\x08", false},
		{"Hello\nWorld\t!", true},
	}
	for _, tt := range tests {
		got := isReadableText(tt.input)
		if got != tt.want {
			t.Errorf("isReadableText(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
