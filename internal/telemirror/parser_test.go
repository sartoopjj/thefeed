package telemirror

import (
	"strings"
	"testing"
)

const sampleChannel = `<!DOCTYPE html><html><body>
<div class="tgme_channel_info">
  <div class="tgme_channel_info_header">
    <i class="tgme_page_photo_image"><img src="https://cdn4-telegram-org.translate.goog/file/abc.jpg"/></i>
    <div class="tgme_channel_info_header_title"><span dir="auto">Sample Channel</span></div>
    <div class="tgme_channel_info_header_username"><a href="https://t-me.translate.goog/sample">@sample</a></div>
  </div>
  <div class="tgme_channel_info_description">channel <b>description</b> with <a href="https://example.com">link</a></div>
  <div class="tgme_channel_info_counter"><span class="counter_value">12.3K</span> <span class="counter_type">subscribers</span></div>
</div>

<div class="tgme_widget_message_wrap">
  <div class="tgme_widget_message" data-post="sample/123">
    <a class="tgme_widget_message_owner_name" href="#"><span dir="auto">Sample Channel</span></a>
    <div class="tgme_widget_message_text">first <b>post</b> body</div>
    <a class="tgme_widget_message_photo_wrap" href="https://t-me.translate.goog/sample/123" style="background-image:url('https://cdn4.translate.goog/photo.jpg')"></a>
    <div class="tgme_widget_message_footer">
      <span class="tgme_widget_message_views">1.2K</span>
      <a class="tgme_widget_message_date" href="#"><time datetime="2026-04-30T12:34:56+00:00">Apr 30</time></a>
      <span class="tgme_widget_message_meta">edited</span>
    </div>
  </div>
</div>

<div class="tgme_widget_message_wrap">
  <div class="tgme_widget_message" data-post="sample/124">
    <div class="tgme_widget_message_text">second post</div>
    <a class="tgme_widget_message_video_player" href="https://t-me.translate.goog/sample/124">
      <i class="tgme_widget_message_video_thumb" style="background-image:url(https://cdn4.translate.goog/vid.jpg)"></i>
      <time class="message_video_duration">0:42</time>
    </a>
  </div>
</div>
</body></html>`

func TestParseHTMLChannelHeader(t *testing.T) {
	ch, _, err := ParseHTML(sampleChannel)
	if err != nil {
		t.Fatalf("ParseHTML: %v", err)
	}
	if ch.Title != "Sample Channel" {
		t.Errorf("title = %q, want %q", ch.Title, "Sample Channel")
	}
	if ch.Username != "sample" {
		t.Errorf("username = %q, want %q", ch.Username, "sample")
	}
	if !strings.Contains(ch.Description, "description") {
		t.Errorf("description = %q, missing 'description'", ch.Description)
	}
	if ch.Photo == "" {
		t.Errorf("photo url empty")
	}
	if !strings.Contains(ch.Subscribers, "12.3K") {
		t.Errorf("subscribers = %q, missing '12.3K'", ch.Subscribers)
	}
}

func TestParseHTMLPosts(t *testing.T) {
	_, posts, err := ParseHTML(sampleChannel)
	if err != nil {
		t.Fatalf("ParseHTML: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("posts = %d, want 2", len(posts))
	}

	p1 := posts[0]
	if p1.ID != "sample/123" {
		t.Errorf("post 1 id = %q", p1.ID)
	}
	if !strings.Contains(p1.Text, "first") {
		t.Errorf("post 1 text = %q", p1.Text)
	}
	if len(p1.Media) != 1 || p1.Media[0].Type != "photo" {
		t.Errorf("post 1 media = %+v, want one photo", p1.Media)
	}
	if p1.Media[0].Thumb == "" {
		t.Errorf("post 1 photo thumb missing")
	}
	if p1.Views != "1.2K" {
		t.Errorf("post 1 views = %q", p1.Views)
	}
	if !p1.Edited {
		t.Errorf("post 1 should be marked edited")
	}
	if p1.Time.IsZero() {
		t.Errorf("post 1 time not parsed")
	}

	p2 := posts[1]
	if p2.ID != "sample/124" {
		t.Errorf("post 2 id = %q", p2.ID)
	}
	if len(p2.Media) != 1 || p2.Media[0].Type != "video" {
		t.Errorf("post 2 media = %+v, want one video", p2.Media)
	}
	if p2.Media[0].Duration != "0:42" {
		t.Errorf("post 2 duration = %q", p2.Media[0].Duration)
	}
	if p2.Media[0].Thumb == "" {
		t.Errorf("post 2 video thumb missing")
	}
}

// Telegram embeds the reply preview INSIDE the message wrapper, BEFORE
// the actual message body. Both the snippet and the body use the
// `tgme_widget_message_text` class. The parser must pick the body, not
// the snippet, and must surface the reply metadata as Post.Reply.
const replyPostHTML = `<!DOCTYPE html><html><body>
<div class="tgme_widget_message_wrap">
  <div class="tgme_widget_message" data-post="sample/200">
    <a class="tgme_widget_message_reply" href="https://t.me/sample/198">
      <span class="tgme_widget_message_author_name">Original Author</span>
      <div class="tgme_widget_message_text">quoted snippet of original</div>
    </a>
    <div class="tgme_widget_message_text">actual reply body</div>
  </div>
</div>
</body></html>`

func TestParseHTMLReply(t *testing.T) {
	_, posts, err := ParseHTML(replyPostHTML)
	if err != nil {
		t.Fatalf("ParseHTML: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(posts))
	}
	p := posts[0]
	if !strings.Contains(p.Text, "actual reply body") {
		t.Errorf("Post.Text = %q, want main body, not snippet", p.Text)
	}
	if strings.Contains(p.Text, "quoted snippet") {
		t.Errorf("Post.Text leaked the reply snippet: %q", p.Text)
	}
	if p.Reply == nil {
		t.Fatalf("Post.Reply nil; want populated reply preview")
	}
	if p.Reply.Author != "Original Author" {
		t.Errorf("Reply.Author = %q", p.Reply.Author)
	}
	if !strings.Contains(p.Reply.Text, "quoted snippet") {
		t.Errorf("Reply.Text = %q", p.Reply.Text)
	}
	if p.Reply.URL != "https://t.me/sample/198" {
		t.Errorf("Reply.URL = %q", p.Reply.URL)
	}
}

const forwardPostHTML = `<!DOCTYPE html><html><body>
<div class="tgme_widget_message_wrap">
  <div class="tgme_widget_message" data-post="sample/201">
    <div class="tgme_widget_message_forwarded_from">
      Forwarded from <a class="tgme_widget_message_forwarded_from_name" href="https://t.me/source">Source Channel</a>
    </div>
    <div class="tgme_widget_message_text">forwarded post body</div>
  </div>
</div>
</body></html>`

func TestParseHTMLForward(t *testing.T) {
	_, posts, err := ParseHTML(forwardPostHTML)
	if err != nil {
		t.Fatalf("ParseHTML: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(posts))
	}
	p := posts[0]
	if p.Forward == nil {
		t.Fatalf("Post.Forward nil; want populated forward header")
	}
	if p.Forward.Author != "Source Channel" {
		t.Errorf("Forward.Author = %q", p.Forward.Author)
	}
	if p.Forward.URL != "https://t.me/source" {
		t.Errorf("Forward.URL = %q", p.Forward.URL)
	}
	if !strings.Contains(p.Text, "forwarded post body") {
		t.Errorf("Post.Text = %q", p.Text)
	}
}

const pollPostHTML = `<!DOCTYPE html><html><body>
<div class="tgme_widget_message_wrap">
  <div class="tgme_widget_message" data-post="sample/202">
    <div class="tgme_widget_message_poll">
      <div class="tgme_widget_message_poll_question">Favourite colour?</div>
      <div class="tgme_widget_message_poll_type">Public vote</div>
      <div class="tgme_widget_message_poll_option">
        <div class="tgme_widget_message_poll_option_text">Red</div>
      </div>
      <div class="tgme_widget_message_poll_option">
        <div class="tgme_widget_message_poll_option_text">Blue</div>
      </div>
      <div class="tgme_widget_message_poll_option">
        <div class="tgme_widget_message_poll_option_text">Green</div>
      </div>
    </div>
  </div>
</div>
</body></html>`

func TestParseHTMLPollOptions(t *testing.T) {
	_, posts, err := ParseHTML(pollPostHTML)
	if err != nil {
		t.Fatalf("ParseHTML: %v", err)
	}
	if len(posts) != 1 || len(posts[0].Media) != 1 {
		t.Fatalf("posts/media wrong shape: posts=%d media=%d", len(posts), len(posts[0].Media))
	}
	m := posts[0].Media[0]
	if m.Type != "poll" {
		t.Errorf("Media.Type = %q, want poll", m.Type)
	}
	if m.Title != "Favourite colour?" {
		t.Errorf("Media.Title = %q", m.Title)
	}
	if m.Subtitle != "Public vote" {
		t.Errorf("Media.Subtitle = %q", m.Subtitle)
	}
	wantOpts := []string{"Red", "Blue", "Green"}
	if len(m.Options) != len(wantOpts) {
		t.Fatalf("Options = %v, want %v", m.Options, wantOpts)
	}
	for i, want := range wantOpts {
		if m.Options[i] != want {
			t.Errorf("Options[%d] = %q, want %q", i, m.Options[i], want)
		}
	}
}

func TestDecodeTranslateHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"t-me", "t.me"},
		{"cdn4-telegram-org", "cdn4.telegram.org"},
		{"my--domain-com", "my-domain.com"},
		{"a--b--c-d", "a-b-c.d"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, c := range cases {
		if got := decodeTranslateHost(c.in); got != c.want {
			t.Errorf("decodeTranslateHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRewriteTranslateLink(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			"https://t-me.translate.goog/sample/123",
			"https://t.me/sample/123",
		},
		{
			"https://example-com.translate.goog/path?_x_tr_sl=auto&_x_tr_tl=en&q=keep",
			"https://example.com/path?q=keep",
		},
		{
			"https://my--domain-com.translate.goog/x",
			"https://my-domain.com/x",
		},
		// Untouched: not a translate.goog URL.
		{"https://example.com/foo", "https://example.com/foo"},
		// Untouched: t.me special paths preserve query params.
		{
			"https://t.me/proxy?server=10.0.0.1&port=443&secret=ee00000000000000000000000000000000",
			"https://t.me/proxy?server=10.0.0.1&port=443&secret=ee00000000000000000000000000000000",
		},
		{
			"https://t.me/socks?server=10.0.0.2&port=1080&user=testuser&pass=testpass",
			"https://t.me/socks?server=10.0.0.2&port=1080&user=testuser&pass=testpass",
		},
		// Translate proxy link for t.me/proxy: preserves all non-_x_tr params.
		{
			"https://t-me.translate.goog/proxy?server=10.0.0.1&port=443&secret=ee00000000000000000000000000000000&_x_tr_sl=auto&_x_tr_tl=fa",
			"https://t.me/proxy?port=443&secret=ee00000000000000000000000000000000&server=10.0.0.1",
		},
		// Untouched: empty.
		{"", ""},
		// Tracking-only query string collapses to no query string.
		{
			"https://t-me.translate.goog/sample?_x_tr_pto=wapp",
			"https://t.me/sample",
		},
		// Wrapper form: translate.google.com/website?u=<original>
		{
			"https://translate.google.com/website?sl=auto&tl=fa&hl=en&client=webapp&u=https://seup.shop/f/rdzq",
			"https://seup.shop/f/rdzq",
		},
		// Wrapper form, /translate path also works.
		{
			"https://translate.google.com/translate?sl=en&tl=fa&u=https://example.com/page",
			"https://example.com/page",
		},
		// Wrapper form pointing at a goog inline-proxy URL — recurse.
		{
			"https://translate.google.com/website?u=https://t-me.translate.goog/networkti",
			"https://t.me/networkti",
		},
		// Dangerous schemes collapse to empty (structured fields flow here).
		{"javascript:alert(1)", ""},
		{"  JavaScript:alert(1)", ""},
		{"data:text/html,<script>alert(1)</script>", ""},
	}
	for _, c := range cases {
		if got := rewriteTranslateLink(c.in); got != c.want {
			t.Errorf("rewriteTranslateLink(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizePostHTML(t *testing.T) {
	in := `Check <a href="https://t-me.translate.goog/sample/123">this post</a> ` +
		`and <a href="https://example-com.translate.goog/?_x_tr_sl=auto">this site</a>. ` +
		`<img src="https://cdn-translate.goog/img.jpg"> stays.`
	got := sanitizePostHTML(in)
	if !strings.Contains(got, `href="https://t.me/sample/123"`) {
		t.Errorf("translate.goog href not rewritten: %q", got)
	}
	if !strings.Contains(got, `href="https://example.com/"`) {
		t.Errorf("example-com.translate.goog href not rewritten or query not stripped: %q", got)
	}
	if !strings.Contains(got, `src="https://cdn-translate.goog/img.jpg"`) {
		t.Errorf("img src should NOT be rewritten (we serve images via the proxy): %q", got)
	}
	// Plain HTML round-trips without the sanitizer damaging it.
	plain := "no proxy links here, just <b>text</b>"
	if got := sanitizePostHTML(plain); got != plain {
		t.Errorf("plain html mutated: %q", got)
	}
}

// Channel posts are fully attacker-controlled HTML; script vectors must be
// stripped before the client splices them into the DOM. Mirrors a real-world
// case where a channel ships links with onclick="return confirm(...)".
func TestSanitizePostHTMLStripsScripts(t *testing.T) {
	in := `ok <a href="https://t.me/x" target="_blank" rel="noopener" ` +
		`onclick="return confirm('Open this link?')">link</a> ` +
		`<a href="javascript:alert(1)">x</a> ` +
		`<img src="https://e.com/a.jpg" onerror="alert(2)">`
	got := sanitizePostHTML(in)
	if strings.Contains(strings.ToLower(got), "onclick") {
		t.Errorf("onclick handler survived: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "onerror") {
		t.Errorf("onerror handler survived: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "javascript:") {
		t.Errorf("javascript: href survived: %q", got)
	}
	if !strings.Contains(got, `href="https://t.me/x"`) {
		t.Errorf("safe href was dropped: %q", got)
	}
	if !strings.Contains(got, `src="https://e.com/a.jpg"`) {
		t.Errorf("img src should remain (served via proxy): %q", got)
	}
}

func TestSanitizePostHTMLStitchesSplitURL(t *testing.T) {
	// Google Translate splits proxy URLs at & boundaries:
	// <a href="https://t.me/proxy?server=10.0.0.1">...</a>&port=443&secret=ee0000
	cases := []struct {
		desc string
		in   string
		want []string
	}{
		{
			"split at first &",
			`<a href="https://t.me/proxy?server=10.0.0.1">https://t.me/proxy?server=10.0.0.1</a>&amp;port=443&amp;secret=ee00000000000000000000000000000000`,
			[]string{"server=10.0.0.1", "port=443", "secret=ee00000000000000000000000000000000"},
		},
		{
			"split with translate host",
			`<a href="https://t-me.translate.goog/proxy?server=10.0.0.1&amp;_x_tr_sl=auto">https://t.me/proxy?server=10.0.0.1</a>&amp;port=443&amp;secret=ee00000000000000000000000000000000`,
			[]string{"server=10.0.0.1", "port=443", "secret=ee00000000000000000000000000000000"},
		},
		{
			"no trailing params — unchanged",
			`<a href="https://example.com/page?x=1">link</a> some text`,
			[]string{`href="https://example.com/page?x=1"`},
		},
	}
	for _, c := range cases {
		got := sanitizePostHTML(c.in)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in %q", c.desc, w, got)
			}
		}
	}
}

func TestSanitizePostHTMLProxyLinkComplete(t *testing.T) {
	in := `<a href="https://t-me.translate.goog/proxy?server=10.0.0.1&amp;port=443&amp;secret=ee00000000000000000000000000000000&amp;_x_tr_sl=auto&amp;_x_tr_tl=fa">https://t.me/proxy?server=10.0.0.1&amp;port=443&amp;secret=ee00000000000000000000000000000000</a>`
	got := sanitizePostHTML(in)
	for _, want := range []string{"server=10.0.0.1", "port=443", "secret=ee00000000000000000000000000000000"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output: %q", want, got)
		}
	}
	if strings.Contains(got, "_x_tr_") {
		t.Errorf("translate params leaked: %q", got)
	}
	if strings.Contains(got, "translate.goog") {
		t.Errorf("translate.goog host leaked: %q", got)
	}
}

func TestSanitizePostHTMLPreservesQueryParams(t *testing.T) {
	cases := []struct {
		desc string
		in   string
		want string
	}{
		{
			"proxy link keeps all query params",
			`<a href="https://t.me/proxy?server=10.0.0.1&amp;port=443&amp;secret=ee00000000000000000000000000000000">Proxy</a>`,
			`server=10.0.0.1`,
		},
		{
			"proxy link keeps secret param",
			`<a href="https://t.me/proxy?server=10.0.0.1&amp;port=443&amp;secret=ee00000000000000000000000000000000">Proxy</a>`,
			`secret=ee00000000000000000000000000000000`,
		},
		{
			"socks link keeps query params",
			`<a href="https://t.me/socks?server=10.0.0.2&amp;port=1080&amp;user=testuser&amp;pass=testpass">SOCKS</a>`,
			`server=10.0.0.2`,
		},
		{
			"regular t.me channel link preserved",
			`<a href="https://t.me/somechannel/123">post</a>`,
			`href="https://t.me/somechannel/123"`,
		},
		{
			"translate proxy link rewritten, proxy query kept",
			`<a href="https://t-me.translate.goog/proxy?server=10.0.0.3&amp;port=443&amp;_x_tr_sl=auto">Proxy</a>`,
			`server=10.0.0.3`,
		},
	}
	for _, c := range cases {
		got := sanitizePostHTML(c.in)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want substring %q", c.desc, got, c.want)
		}
	}
}

// Telegram's embed HTML double-encodes & as &amp;amp; in proxy hrefs.
// After html.Parse decodes one level the href still contains &amp;
// which breaks url.Parse. The sanitizer must unescape before rewriting.
func TestSanitizePostHTMLDoubleEncodedAmpersand(t *testing.T) {
	// Simulates what html.Render produces from Telegram's double-encoded markup:
	// raw source had &amp;amp;port → html.Parse yields &amp;port → html.Render
	// re-encodes to &amp;amp;port in the serialised HTML we feed to sanitizePostHTML.
	in := `<a href="https://t-me.translate.goog/proxy?server=10.0.0.1&amp;amp;port=443&amp;amp;secret=ee00000000000000000000000000000000&amp;_x_tr_sl=auto&amp;_x_tr_tl=fa">پروکسی</a>`
	got := sanitizePostHTML(in)
	for _, want := range []string{"server=10.0.0.1", "port=443", "secret=ee00000000000000000000000000000000"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output: %q", want, got)
		}
	}
	if strings.Contains(got, "_x_tr_") {
		t.Errorf("translate params leaked: %q", got)
	}
	if strings.Contains(got, "translate.goog") {
		t.Errorf("translate.goog host leaked: %q", got)
	}
}

func TestParseHTMLRewritesPostLinks(t *testing.T) {
	const sample = `<!DOCTYPE html><html><body>
<div class="tgme_widget_message_wrap">
  <div class="tgme_widget_message" data-post="sample/300">
    <div class="tgme_widget_message_text">
      Visit <a href="https://t-me.translate.goog/networkti">@networkti</a> and
      <a href="https://example-com.translate.goog/article?_x_tr_sl=auto&_x_tr_tl=en">read</a>,
      and try <a href="https://translate.google.com/website?sl=auto&tl=fa&u=https://seup.shop/f/rdzq">this site</a>.
    </div>
  </div>
</div>
</body></html>`
	_, posts, err := ParseHTML(sample)
	if err != nil {
		t.Fatalf("ParseHTML: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(posts))
	}
	if !strings.Contains(posts[0].Text, `href="https://t.me/networkti"`) {
		t.Errorf("Post.Text didn't rewrite t-me link: %q", posts[0].Text)
	}
	if !strings.Contains(posts[0].Text, `href="https://seup.shop/f/rdzq"`) {
		t.Errorf("Post.Text didn't unwrap translate.google.com/website wrapper: %q", posts[0].Text)
	}
	if strings.Contains(posts[0].Text, "translate.goog") {
		t.Errorf("translate.goog leaked in Post.Text: %q", posts[0].Text)
	}
	if strings.Contains(posts[0].Text, "translate.google.com") {
		t.Errorf("translate.google.com wrapper leaked in Post.Text: %q", posts[0].Text)
	}
	if strings.Contains(posts[0].Text, "_x_tr_") {
		t.Errorf("_x_tr_ tracking params leaked: %q", posts[0].Text)
	}
}

// Google Translate sometimes injects toolbar / language-picker images
// inside the channel header. The avatar parser must skip those and
// pick the real one in `.tgme_page_photo_image` regardless of order.
func TestParseHTMLAvatarPicksRealImg(t *testing.T) {
	const sample = `<!DOCTYPE html><html><body>
<div class="tgme_channel_info_header">
  <img src="https://translate.google.com/static/icon.svg" alt="Translate">
  <i class="tgme_page_photo_image"><img src="https://cdn4-telegram-org.translate.goog/file/real-avatar.jpg"></i>
</div>
</body></html>`
	ch, _, err := ParseHTML(sample)
	if err != nil {
		t.Fatalf("ParseHTML: %v", err)
	}
	if !strings.Contains(ch.Photo, "real-avatar.jpg") {
		t.Errorf("ch.Photo = %q, want the real avatar URL", ch.Photo)
	}
}

func TestParseHTMLEmpty(t *testing.T) {
	ch, posts, err := ParseHTML("<html></html>")
	if err != nil {
		t.Fatalf("ParseHTML: %v", err)
	}
	if ch == nil {
		t.Fatal("channel nil")
	}
	if len(posts) != 0 {
		t.Errorf("posts = %d, want 0", len(posts))
	}
}

func TestSanitizeUsername(t *testing.T) {
	cases := []struct{ in, want string }{
		{"@VahidOnline", "VahidOnline"},
		{"  @VahidOnline  ", "VahidOnline"},
		{"https://t.me/networkti", "networkti"},
		{"t.me/s/networkti", "networkti"},
		{"networkti?embed=1", "networkti"},
		{"bad name with spaces", "badnamewithspaces"},
		{"خبر", ""},
		{"", ""},
		{strings.Repeat("a", 64), strings.Repeat("a", 32)},
	}
	for _, c := range cases {
		if got := SanitizeUsername(c.in); got != c.want {
			t.Errorf("SanitizeUsername(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsDefault(t *testing.T) {
	for _, want := range DefaultChannels {
		if !IsDefault(want) {
			t.Errorf("IsDefault(%q) = false", want)
		}
		if !IsDefault(strings.ToLower(want)) {
			t.Errorf("IsDefault(%q) case-sensitive miss", strings.ToLower(want))
		}
	}
	if IsDefault("not_a_default") {
		t.Errorf("IsDefault returned true for unknown channel")
	}
}

func TestStoreAddRemove(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.Add("@MyChan"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Adding a default is a no-op (success, no duplicate).
	if err := s.Add(DefaultChannels[0]); err != nil {
		t.Errorf("Add default: %v", err)
	}
	list := s.List()
	if len(list) < len(DefaultChannels)+1 {
		t.Fatalf("list = %v, want defaults + MyChan", list)
	}
	// Defaults come first.
	for i, want := range DefaultChannels {
		if list[i] != want {
			t.Errorf("list[%d] = %q, want %q", i, list[i], want)
		}
	}
	// MyChan present and at the end.
	if list[len(list)-1] != "MyChan" {
		t.Errorf("last entry = %q, want MyChan", list[len(list)-1])
	}

	if err := s.Remove("MyChan"); err != nil {
		t.Errorf("Remove: %v", err)
	}
	list = s.List()
	for _, u := range list {
		if strings.EqualFold(u, "MyChan") {
			t.Errorf("MyChan still present after Remove: %v", list)
		}
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)

	if r, _ := c.Get("nope"); r != nil {
		t.Errorf("Get on missing returned non-nil: %+v", r)
	}

	res := &FetchResult{
		Channel: Channel{Username: "test", Title: "Test"},
		Posts:   []Post{{ID: "test/1", Text: "hi"}},
	}
	if err := c.Put("test", res); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, fresh := c.Get("test")
	if got == nil {
		t.Fatal("Get returned nil after Put")
	}
	if !fresh {
		t.Errorf("expected fresh entry right after Put")
	}
	if got.Channel.Title != "Test" || len(got.Posts) != 1 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	c.Clear()
	if got, _ := c.Get("test"); got != nil {
		t.Errorf("Get after Clear returned %+v", got)
	}
}

// Defaults are re-added by List() on every call, so a removal only sticks if
// it is recorded. Users asked to be able to drop the bundled channels.
func TestStoreRemoveDefault(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	target := DefaultChannels[0]

	if err := s.Remove(target); err != nil {
		t.Fatalf("Remove default: %v", err)
	}
	for _, u := range s.List() {
		if strings.EqualFold(u, target) {
			t.Fatalf("%q still listed after Remove: %v", target, s.List())
		}
	}
	// The other defaults are untouched.
	for _, d := range DefaultChannels[1:] {
		if !containsFold(s.List(), d) {
			t.Errorf("Remove(%q) also dropped %q", target, d)
		}
	}
	// Survives a reload — the removal is on disk, not in memory.
	if got := NewStore(dir).List(); containsFold(got, target) {
		t.Errorf("%q came back after reload: %v", target, got)
	}
	// Re-adding restores it, in its original position.
	if err := s.Add(target); err != nil {
		t.Fatalf("Add default back: %v", err)
	}
	if got := s.List(); len(got) == 0 || got[0] != target {
		t.Errorf("after re-add, list[0] = %v, want %q", got, target)
	}
	// Removing twice is a no-op, not a duplicate hidden entry.
	if err := s.Remove(target); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.Remove(target); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if containsFold(s.List(), target) {
		t.Errorf("%q listed after double Remove", target)
	}
}

// A user channel and a hidden default must not interfere.
func TestStoreHiddenDefaultKeepsUserChannels(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Add("MyChan"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Remove(DefaultChannels[0]); err != nil {
		t.Fatalf("Remove default: %v", err)
	}
	got := s.List()
	if !containsFold(got, "MyChan") {
		t.Errorf("user channel lost: %v", got)
	}
	if containsFold(got, DefaultChannels[0]) {
		t.Errorf("hidden default still listed: %v", got)
	}
}

// A default can also sit in the user list (e.g. a release promotes a channel
// someone had already added). List() would then re-add it from the user loop,
// leaving it permanently unremovable.
func TestRemoveDefaultAlsoInUserList(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	target := DefaultChannels[0]
	if err := s.saveFileLocked(subsFile{Channels: []string{target, "MyChan"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(target); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := s.List(); containsFold(got, target) {
		t.Errorf("%q still listed after Remove: %v", target, got)
	}
	if got := s.List(); !containsFold(got, "MyChan") {
		t.Errorf("unrelated user channel lost: %v", got)
	}
	// And it stays gone across a reload.
	if got := NewStore(dir).List(); containsFold(got, target) {
		t.Errorf("%q returned after reload: %v", target, got)
	}
}

// Post markup reaches us over a connection the censor sits on, and the
// renderer splices it into the UI's origin with innerHTML. The old sanitizer
// stripped on* handlers and javascript: hrefs and kept every ELEMENT, so
// <iframe srcdoc="..."> went straight through -- and a srcdoc frame inserted
// via innerHTML executes with the parent's origin. No click, no handler
// attribute, just render. From there: the recovery seed, the passphrase, the
// chat history.
func TestSanitizePostHTMLDropsExecutableElements(t *testing.T) {
	cases := []struct {
		desc, in string
	}{
		{"srcdoc frame", `a<iframe srcdoc="&lt;script&gt;alert(1)&lt;/script&gt;"></iframe>b`},
		{"plain frame", `a<iframe src="https://evil.example/"></iframe>b`},
		{"script", `a<script>alert(1)</script>b`},
		{"object", `a<object data="https://evil.example/x.swf"></object>b`},
		{"embed", `a<embed src="https://evil.example/x">b`},
		{"svg animate", `a<svg><animate onbegin="alert(1)"></svg>b`},
		{"form action", `a<form action="javascript:alert(1)"><input></form>b`},
		{"meta refresh", `a<meta http-equiv="refresh" content="0;url=https://evil.example">b`},
		{"base", `a<base href="https://evil.example/">b`},
		{"style", `a<style>body{display:none}</style>b`},
	}
	for _, c := range cases {
		got := strings.ToLower(sanitizePostHTML(c.in))
		for _, bad := range []string{"<iframe", "<script", "<object", "<embed", "<svg",
			"<form", "<input", "<meta", "<base", "<style", "srcdoc", "onbegin"} {
			if strings.Contains(got, bad) {
				t.Errorf("%s: %q survived in %q", c.desc, bad, got)
			}
		}
		// The surrounding text is still there: this sanitises, it does not
		// blank the post.
		if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
			t.Errorf("%s: the post's own text was lost: %q", c.desc, got)
		}
	}
}

// An unknown tag loses the tag, never the words inside it.
func TestSanitizePostHTMLUnwrapsUnknownTags(t *testing.T) {
	got := sanitizePostHTML(`before<marquee>the text</marquee>after`)
	if strings.Contains(strings.ToLower(got), "<marquee") {
		t.Errorf("unknown tag kept: %q", got)
	}
	if !strings.Contains(got, "the text") {
		t.Errorf("text inside an unknown tag was dropped: %q", got)
	}
}

// style= cannot execute, but it can lay an invisible element over the page.
func TestSanitizePostHTMLDropsStyleAttribute(t *testing.T) {
	got := sanitizePostHTML(`<span style="position:fixed;inset:0;z-index:99">x</span>`)
	if strings.Contains(strings.ToLower(got), "style=") {
		t.Errorf("style attribute survived: %q", got)
	}
	if !strings.Contains(got, "x") {
		t.Errorf("content lost: %q", got)
	}
}
