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
	}
	for _, c := range cases {
		if got := rewriteTranslateLink(c.in); got != c.want {
			t.Errorf("rewriteTranslateLink(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRewriteTranslateLinksInHTML(t *testing.T) {
	in := `Check <a href="https://t-me.translate.goog/sample/123">this post</a> ` +
		`and <a href="https://example-com.translate.goog/?_x_tr_sl=auto">this site</a>. ` +
		`<img src="https://cdn-translate.goog/img.jpg"> stays.`
	got := rewriteTranslateLinksInHTML(in)
	if !strings.Contains(got, `href="https://t.me/sample/123"`) {
		t.Errorf("translate.goog href not rewritten: %q", got)
	}
	if !strings.Contains(got, `href="https://example.com/"`) {
		t.Errorf("example-com.translate.goog href not rewritten or query not stripped: %q", got)
	}
	if !strings.Contains(got, `src="https://cdn-translate.goog/img.jpg"`) {
		t.Errorf("img src should NOT be rewritten (we serve images via the proxy): %q", got)
	}
	// Sanity check: untouched HTML round-trips without the rewriter
	// damaging it.
	plain := "no proxy links here, just <b>text</b>"
	if got := rewriteTranslateLinksInHTML(plain); got != plain {
		t.Errorf("plain html mutated: %q", got)
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
	// Defaults pinned at the front.
	for i, want := range DefaultChannels {
		if list[i] != want {
			t.Errorf("list[%d] = %q, want %q", i, list[i], want)
		}
	}
	// MyChan present and at the end.
	if list[len(list)-1] != "MyChan" {
		t.Errorf("last entry = %q, want MyChan", list[len(list)-1])
	}

	// Removing a default is rejected.
	if err := s.Remove(DefaultChannels[0]); err != ErrPinnedChannel {
		t.Errorf("Remove default = %v, want ErrPinnedChannel", err)
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
