package telemirror

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ParseHTML extracts channel info and recent posts from a t.me/s/ widget.
// The HTML is the rendered output of Google Translate's proxy, which keeps
// the original Telegram class names but rewrites cross-domain URLs onto
// translate.goog so the browser can load them through the same proxy.
func ParseHTML(htmlBody string) (*Channel, []Post, error) {
	doc, err := html.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return nil, nil, err
	}
	return parseChannelInfo(doc), parsePosts(doc), nil
}

func parseChannelInfo(doc *html.Node) *Channel {
	ch := &Channel{}

	if titleEl := findFirstByClass(doc, "tgme_channel_info_header_title"); titleEl != nil {
		if span := findFirstChildElement(titleEl, "span"); span != nil {
			ch.Title = textOf(span)
		} else {
			ch.Title = textOf(titleEl)
		}
	}
	if userEl := findFirstByClass(doc, "tgme_channel_info_header_username"); userEl != nil {
		if a := findFirstChildElement(userEl, "a"); a != nil {
			ch.Username = strings.TrimPrefix(textOf(a), "@")
		}
	}
	if descEl := findFirstByClass(doc, "tgme_channel_info_description"); descEl != nil {
		ch.Description = sanitizePostHTML(innerHTML(descEl))
	}
	// Telegram wraps the avatar in `tgme_page_photo_image`. Look for
	// that specifically — Google Translate's proxy may inject other
	// imagery in the header, so picking the first <img> grabs the
	// wrong one.
	if photoEl := findFirstByClass(doc, "tgme_page_photo_image"); photoEl != nil {
		if img := findFirstByTag(photoEl, "img"); img != nil {
			ch.Photo = attrOf(img, "src")
		} else if src := attrOf(photoEl, "src"); src != "" {
			ch.Photo = src
		}
	}
	// Fallback: first <img> in the header (older Telegram layouts).
	if ch.Photo == "" {
		if header := findFirstByClass(doc, "tgme_channel_info_header"); header != nil {
			if img := findFirstByTag(header, "img"); img != nil {
				ch.Photo = attrOf(img, "src")
			}
		}
	}
	if cnt := findFirstByClass(doc, "tgme_channel_info_counter"); cnt != nil {
		ch.Subscribers = textOf(cnt)
	}
	return ch
}

func parsePosts(doc *html.Node) []Post {
	var posts []Post
	visit(doc, func(n *html.Node) bool {
		if !hasClass(n, "tgme_widget_message_wrap") {
			return true
		}
		if p := parseSinglePost(n); p != nil {
			posts = append(posts, *p)
		}
		return false // posts don't nest
	})
	return posts
}

func parseSinglePost(wrap *html.Node) *Post {
	msg := findFirstByClass(wrap, "tgme_widget_message")
	if msg == nil {
		msg = wrap
	}
	p := &Post{ID: attrOf(msg, "data-post")}

	if owner := findFirstByClass(msg, "tgme_widget_message_owner_name"); owner != nil {
		p.Author = textOf(owner)
	}
	// The reply preview also has a `.tgme_widget_message_text` (the
	// quoted snippet) which appears BEFORE the body in the DOM, so
	// findFirstByClass would grab the wrong one.
	if textEl := findMessageBodyText(msg); textEl != nil {
		p.Text = sanitizePostHTML(innerHTML(textEl))
	}

	p.Reply = parseReply(msg)
	p.Forward = parseForward(msg)

	visit(msg, func(n *html.Node) bool {
		switch {
		case hasClass(n, "tgme_widget_message_photo_wrap"):
			// Source-post URL → real t.me. Thumb stays on the proxy
			// (that's how the bytes reach a blocked client).
			p.Media = append(p.Media, Media{
				Type:  "photo",
				URL:   rewriteTranslateLink(attrOf(n, "href")),
				Thumb: extractBgImage(attrOf(n, "style")),
				Ratio: extractPhotoRatio(n),
			})
		case hasClass(n, "tgme_widget_message_video_player"):
			m := Media{Type: "video", URL: rewriteTranslateLink(attrOf(n, "href"))}
			if t := findFirstByClass(n, "tgme_widget_message_video_thumb"); t != nil {
				m.Thumb = extractBgImage(attrOf(t, "style"))
			}
			if d := findFirstByClass(n, "message_video_duration"); d != nil {
				m.Duration = textOf(d)
			}
			p.Media = append(p.Media, m)
		case hasClass(n, "tgme_widget_message_voice_player"):
			// Outer player wraps the inner _voice element; only match the
			// outer to avoid emitting one Media per nested element.
			m := Media{Type: "voice"}
			if d := findFirstByClass(n, "tgme_widget_message_voice_duration"); d != nil {
				m.Duration = textOf(d)
			}
			p.Media = append(p.Media, m)
		case hasClass(n, "tgme_widget_message_audio_player"):
			m := Media{Type: "audio"}
			if d := findFirstByClass(n, "tgme_widget_message_audio_duration"); d != nil {
				m.Duration = textOf(d)
			}
			if t := findFirstByClass(n, "tgme_widget_message_audio_title"); t != nil {
				m.Title = textOf(t)
			}
			if a := findFirstByClass(n, "tgme_widget_message_audio_performer"); a != nil {
				m.Subtitle = textOf(a)
			}
			p.Media = append(p.Media, m)
		case hasClass(n, "tgme_widget_message_document_wrap"):
			m := Media{Type: "document"}
			if t := findFirstByClass(n, "tgme_widget_message_document_title"); t != nil {
				m.Title = textOf(t)
			}
			if e := findFirstByClass(n, "tgme_widget_message_document_extra"); e != nil {
				m.Subtitle = textOf(e)
			}
			p.Media = append(p.Media, m)
		case hasClass(n, "tgme_widget_message_sticker_wrap"):
			m := Media{Type: "sticker"}
			if img := findFirstByTag(n, "img"); img != nil {
				m.Thumb = attrOf(img, "src")
			}
			if m.Thumb == "" {
				m.Thumb = extractBgImage(attrOf(n, "style"))
			}
			p.Media = append(p.Media, m)
		case hasClass(n, "tgme_widget_message_poll"):
			m := Media{Type: "poll"}
			if q := findFirstByClass(n, "tgme_widget_message_poll_question"); q != nil {
				m.Title = textOf(q)
			}
			if t := findFirstByClass(n, "tgme_widget_message_poll_type"); t != nil {
				m.Subtitle = textOf(t)
			}
			visit(n, func(opt *html.Node) bool {
				if hasClass(opt, "tgme_widget_message_poll_option_text") {
					txt := strings.TrimSpace(textOf(opt))
					if txt != "" {
						m.Options = append(m.Options, txt)
					}
					return false
				}
				return true
			})
			p.Media = append(p.Media, m)
		}
		return true
	})

	// Reactions: each .tgme_reaction holds an emoji + a count.
	visit(msg, func(n *html.Node) bool {
		if !hasClass(n, "tgme_reaction") {
			return true
		}
		emoji := ""
		if e := findFirstByClass(n, "emoji"); e != nil {
			if b := findFirstByTag(e, "b"); b != nil {
				emoji = textOf(b)
			} else {
				emoji = textOf(e)
			}
		}
		// Fallback: take any <b> directly inside.
		if emoji == "" {
			if b := findFirstByTag(n, "b"); b != nil {
				emoji = textOf(b)
			}
		}
		count := strings.TrimSpace(strings.TrimPrefix(textOf(n), emoji))
		if emoji != "" || count != "" {
			p.Reactions = append(p.Reactions, Reaction{Emoji: emoji, Count: count})
		}
		return false // don't recurse inside a reaction
	})

	if dateEl := findFirstByClass(msg, "tgme_widget_message_date"); dateEl != nil {
		if tag := findFirstByTag(dateEl, "time"); tag != nil {
			if dt := attrOf(tag, "datetime"); dt != "" {
				if parsed, err := time.Parse(time.RFC3339, dt); err == nil {
					p.Time = parsed
				}
			}
		}
	}
	if v := findFirstByClass(msg, "tgme_widget_message_views"); v != nil {
		p.Views = textOf(v)
	}
	if meta := findFirstByClass(msg, "tgme_widget_message_meta"); meta != nil {
		if strings.Contains(strings.ToLower(textOf(meta)), "edited") {
			p.Edited = true
		}
	}

	if p.ID == "" && p.Text == "" && len(p.Media) == 0 {
		return nil
	}
	return p
}

// findMessageBodyText: first `.tgme_widget_message_text` not nested
// inside a `.tgme_widget_message_reply` (which would be the snippet).
func findMessageBodyText(msg *html.Node) *html.Node {
	var found *html.Node
	visit(msg, func(n *html.Node) bool {
		if found != nil {
			return false
		}
		if !hasClass(n, "tgme_widget_message_text") {
			return true
		}
		for p := n.Parent; p != nil; p = p.Parent {
			if hasClass(p, "tgme_widget_message_reply") {
				return false
			}
		}
		found = n
		return false
	})
	return found
}

// parseReply extracts author + snippet + URL from a reply preview.
func parseReply(msg *html.Node) *Reply {
	rNode := findFirstByClass(msg, "tgme_widget_message_reply")
	if rNode == nil {
		return nil
	}
	r := &Reply{
		URL: rewriteTranslateLink(attrOf(rNode, "href")),
	}
	if a := findFirstByClass(rNode, "tgme_widget_message_author_name"); a != nil {
		r.Author = textOf(a)
	}
	if t := findFirstByClass(rNode, "tgme_widget_message_text"); t != nil {
		r.Text = sanitizePostHTML(innerHTML(t))
	}
	if r.Author == "" && r.Text == "" {
		return nil
	}
	return r
}

// parseForward extracts the "Forwarded from <name>" header.
func parseForward(msg *html.Node) *Forward {
	fNode := findFirstByClass(msg, "tgme_widget_message_forwarded_from")
	if fNode == nil {
		return nil
	}
	f := &Forward{}
	if a := findFirstByClass(fNode, "tgme_widget_message_forwarded_from_name"); a != nil {
		f.Author = textOf(a)
		f.URL = rewriteTranslateLink(attrOf(a, "href"))
	} else if a := findFirstByTag(fNode, "a"); a != nil {
		// Bare <a> without the _name class.
		f.Author = textOf(a)
		f.URL = rewriteTranslateLink(attrOf(a, "href"))
	} else {
		f.Author = textOf(fNode)
	}
	if f.Author == "" {
		return nil
	}
	return f
}

// ===== DOM helpers =====

func visit(n *html.Node, fn func(*html.Node) bool) {
	if !fn(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		visit(c, fn)
	}
}

func hasClass(n *html.Node, class string) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	for _, a := range n.Attr {
		if a.Key != "class" {
			continue
		}
		for _, c := range strings.Fields(a.Val) {
			if c == class {
				return true
			}
		}
	}
	return false
}

func attrOf(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func findFirstByClass(root *html.Node, class string) *html.Node {
	var found *html.Node
	visit(root, func(n *html.Node) bool {
		if found != nil {
			return false
		}
		if hasClass(n, class) {
			found = n
			return false
		}
		return true
	})
	return found
}

func findFirstByTag(root *html.Node, tag string) *html.Node {
	var found *html.Node
	visit(root, func(n *html.Node) bool {
		if found != nil {
			return false
		}
		if n.Type == html.ElementNode && n.Data == tag {
			found = n
			return false
		}
		return true
	})
	return found
}

func findFirstChildElement(n *html.Node, tag string) *html.Node {
	if n == nil {
		return nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == tag {
			return c
		}
	}
	return nil
}

func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	visit(n, func(x *html.Node) bool {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		return true
	})
	return strings.TrimSpace(b.String())
}

// innerHTML serialises children only — drops the wrapping element so the
// caller can splice the result into its own container.
func innerHTML(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			return ""
		}
	}
	return strings.TrimSpace(b.String())
}

var bgImageRe = regexp.MustCompile(`url\(['"]?([^'")]+)['"]?\)`)

func extractBgImage(style string) string {
	m := bgImageRe.FindStringSubmatch(style)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

var (
	cssWidthRe  = regexp.MustCompile(`(?:^|[;\s])width:\s*([0-9.]+)px`)
	cssHeightRe = regexp.MustCompile(`(?:^|[;\s])height:\s*([0-9.]+)px`)
	cssPadTopRe = regexp.MustCompile(`padding-top:\s*([0-9.]+)%`)
	dataRatioRe = regexp.MustCompile(`^[0-9.]+$`)
)

func cssNum(re *regexp.Regexp, s string) float64 {
	m := re.FindStringSubmatch(s)
	if len(m) == 2 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return v
		}
	}
	return 0
}

// extractPhotoRatio returns a photo's aspect ratio (width/height) from the
// Telegram markup so the UI can reserve the exact box and never reflow (no
// scroll jump). Album/grouped photos carry width+height (and data-ratio) in the
// wrap style; single photos carry it as a padding-top:% spacer child.
func extractPhotoRatio(n *html.Node) float64 {
	style := attrOf(n, "style")
	w, h := cssNum(cssWidthRe, style), cssNum(cssHeightRe, style)
	if w > 0 && h > 0 {
		return w / h
	}
	if child := findFirstByClass(n, "tgme_widget_message_photo"); child != nil {
		// padding-top:% = height/width*100, so width/height = 100/%.
		if pt := cssNum(cssPadTopRe, attrOf(child, "style")); pt > 0 {
			return 100.0 / pt
		}
	}
	if dr := attrOf(n, "data-ratio"); dataRatioRe.MatchString(dr) {
		if v, err := strconv.ParseFloat(dr, 64); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// Google Translate proxy URLs come in two forms; we rewrite hrefs
// back to the originals so links work when Translate is blocked / the
// site is region-locked. Image src attributes are left alone since
// the proxy is how those bytes actually reach the user.

const translateHostSuffix = ".translate.goog"

// decodeTranslateHost: '.' ↔ '-', '-' ↔ '--'.
//
//	t-me              → t.me
//	cdn4-telegram-org → cdn4.telegram.org
//	my--domain-com    → my-domain.com
func decodeTranslateHost(s string) string {
	s = strings.ReplaceAll(s, "--", "\x00")
	s = strings.ReplaceAll(s, "-", ".")
	s = strings.ReplaceAll(s, "\x00", "-")
	return s
}

// rewriteTranslateLink handles two forms:
//  1. <encoded>.translate.goog/<path>?_x_tr_*=… — inline-proxy form;
//     decode host, strip tracking params.
//  2. translate.google.com/website?u=<original> (also /translate) —
//     wrapper form; pull `u` and recurse once (Google occasionally
//     double-wraps).
func rewriteTranslateLink(raw string) string {
	if raw == "" {
		return raw
	}
	// Structured URL fields (media / reply / forward) flow through here; never
	// surface a javascript:/vbscript:/data: scheme as a clickable URL. Inline
	// post links are already dropped upstream in sanitizeAndRewriteTree, but
	// those fields aren't, so guard here too.
	if isDangerousURL(raw) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	// Form 2.
	hostLower := strings.ToLower(u.Host)
	if (hostLower == "translate.google.com" || hostLower == "www.translate.google.com") &&
		(u.Path == "/website" || u.Path == "/translate") {
		if orig := u.Query().Get("u"); orig != "" {
			return rewriteTranslateLink(orig)
		}
	}

	// Form 1.
	if !strings.HasSuffix(hostLower, translateHostSuffix) {
		return raw
	}
	encoded := u.Host[:len(u.Host)-len(translateHostSuffix)]
	u.Host = decodeTranslateHost(encoded)
	if q := u.Query(); len(q) > 0 {
		for k := range q {
			if strings.HasPrefix(k, "_x_tr_") {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// sanitizePostHTML scrubs an untrusted HTML fragment from a channel post and
// rewrites Google Translate proxy hrefs back to their originals. The source
// markup is fully attacker-controlled (a channel can put anything in a post),
// so we ALWAYS parse fragments containing markup and drop script vectors:
// inline event handlers (onclick, onerror, …) and javascript:/vbscript:/data:
// hrefs that would otherwise execute in the client WebView. <img src> is left
// alone (we serve images via the proxy).
func sanitizePostHTML(htmlStr string) string {
	if htmlStr == "" || !strings.Contains(htmlStr, "<") {
		return htmlStr // plain text: no markup to sanitize
	}
	// Sentinel div so we can locate the fragment in the parsed tree
	// (html.Parse injects <html><head><body>…).
	doc, err := html.Parse(strings.NewReader("<div id=\"tm-rewrite-root\">" + htmlStr + "</div>"))
	if err != nil {
		return htmlStr
	}
	// Find the sentinel BEFORE sanitising: the attribute allow-list strips id,
	// including this one's, and a lost sentinel means returning the fragment
	// unsanitised -- failing open, which is the one outcome worth avoiding.
	root := findFirstByID(doc, "tm-rewrite-root")
	if root == nil {
		return htmlStr
	}
	pruneTree(root)
	sanitizeAndRewriteTree(root)
	var b strings.Builder
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			return htmlStr
		}
	}
	return b.String()
}

// Channel post markup is written by whoever runs the channel, and reaches us
// over a connection the censor is on. The renderer splices it straight into
// the UI's origin with innerHTML, so what survives here decides what can run.
//
// ALLOW-LIST, not deny-list. Stripping on* handlers and javascript: hrefs left
// every element standing, and <iframe srcdoc="..."> inserted via innerHTML
// executes with the parent's origin -- no click, no handler attribute, just
// render. From there: the messenger recovery seed, the feed passphrase, the
// chat history.
//
// These are the tags Telegram's own post markup uses.
var allowedPostTags = map[string]bool{
	"a": true, "b": true, "strong": true, "i": true, "em": true,
	"u": true, "ins": true, "s": true, "strike": true, "del": true,
	"code": true, "pre": true, "br": true, "span": true, "div": true,
	"p": true, "blockquote": true, "img": true,
	"tg-emoji": true, "tg-spoiler": true,
}

// dropWholeTags are containers whose CONTENT is as dangerous as the tag, so
// unwrapping them (keeping the children) would defeat the point.
var dropWholeTags = map[string]bool{
	"script": true, "style": true, "iframe": true, "object": true,
	"embed": true, "svg": true, "math": true, "template": true,
	"form": true, "input": true, "textarea": true, "select": true,
	"button": true, "link": true, "meta": true, "base": true,
	"noscript": true, "frame": true, "frameset": true, "applet": true,
	"audio": true, "video": true, "source": true, "track": true,
	"portal": true, "canvas": true, "slot": true,
}

// allowedPostAttrs is per tag. Everything else goes, including style: it
// cannot execute, but it can cover the page with an element that can.
func allowedPostAttr(tag, key string) bool {
	switch key {
	case "class", "title", "dir":
		return true
	}
	switch tag {
	case "a":
		return key == "href" || key == "target" || key == "rel"
	case "img":
		return key == "src" || key == "alt" || key == "width" || key == "height"
	case "tg-emoji":
		return key == "emoji-id"
	}
	return false
}

// pruneTree drops disallowed elements and unwraps unknown ones, keeping their
// text. Runs before the attribute pass so nothing inside a dropped subtree is
// ever looked at.
func pruneTree(n *html.Node) {
	var next *html.Node
	for c := n.FirstChild; c != nil; c = next {
		next = c.NextSibling
		if c.Type != html.ElementNode {
			pruneTree(c)
			continue
		}
		tag := strings.ToLower(c.Data)
		switch {
		case dropWholeTags[tag]:
			n.RemoveChild(c)
		case allowedPostTags[tag]:
			pruneTree(c)
		default:
			// Unknown tag: keep what it said, lose the tag itself.
			pruneTree(c)
			for gc := c.FirstChild; gc != nil; {
				g := gc
				gc = gc.NextSibling
				c.RemoveChild(g)
				n.InsertBefore(g, c)
			}
			n.RemoveChild(c)
		}
	}
}

func sanitizeAndRewriteTree(n *html.Node) {
	if n.Type == html.ElementNode {
		tag := strings.ToLower(n.Data)
		kept := n.Attr[:0]
		for _, a := range n.Attr {
			key := strings.ToLower(a.Key)
			if strings.HasPrefix(key, "on") || !allowedPostAttr(tag, key) {
				continue
			}
			if key == "href" {
				// Telegram double-encodes & as &amp;amp; in embed hrefs.
				// html.Parse decodes one level → &amp; remains; fix it.
				a.Val = html.UnescapeString(a.Val)
				if isDangerousURL(a.Val) {
					continue
				}
				a.Val = rewriteTranslateLink(a.Val)
			}
			kept = append(kept, a)
		}
		n.Attr = kept
		// Google Translate splits URLs at "&" — the <a> wraps only
		// the first query param and the rest leaks as sibling text
		// nodes ("&port=443", "&secret=…"). Stitch them back.
		if n.DataAtom == atom.A {
			stitchSplitURL(n)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sanitizeAndRewriteTree(c)
	}
}

// paramContRe matches "&key=value" fragments that Google Translate
// leaves as text-node siblings after the <a> tag.
var paramContRe = regexp.MustCompile(`^&[A-Za-z0-9_]+=\S*`)

// stitchSplitURL absorbs trailing "&key=value" text/inline siblings into
// the <a> element's href attribute and display text. Google Translate
// often breaks URLs at "&" boundaries, wrapping only the first query
// param in the <a> and emitting the rest as bare text.
func stitchSplitURL(a *html.Node) {
	var hrefAttr *html.Attribute
	for i := range a.Attr {
		if strings.ToLower(a.Attr[i].Key) == "href" {
			hrefAttr = &a.Attr[i]
			break
		}
	}
	if hrefAttr == nil {
		return
	}
	for sib := a.NextSibling; sib != nil; {
		var text string
		if sib.Type == html.TextNode {
			text = sib.Data
		} else if sib.Type == html.ElementNode && sib.DataAtom != atom.Br {
			text = innerText(sib)
		} else {
			break
		}
		m := paramContRe.FindString(text)
		if m == "" {
			break
		}
		hrefAttr.Val += m
		// Append to the <a> tag's display text too.
		if last := a.LastChild; last != nil && last.Type == html.TextNode {
			last.Data += m
		} else {
			a.AppendChild(&html.Node{Type: html.TextNode, Data: m})
		}
		// Remove consumed portion from sibling.
		leftover := text[len(m):]
		next := sib.NextSibling
		if sib.Type == html.TextNode {
			if leftover == "" {
				sib.Parent.RemoveChild(sib)
			} else {
				sib.Data = leftover
				break
			}
		} else {
			sib.Parent.RemoveChild(sib)
			if leftover != "" {
				break
			}
		}
		sib = next
	}
}

// innerText returns the concatenated text content of a node tree.
func innerText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(innerText(c))
	}
	return b.String()
}

// isDangerousURL reports whether a URL uses a scheme that executes script or
// inlines content. Whitespace/control chars are stripped first, since browsers
// ignore them when resolving the scheme (e.g. "java\tscript:" still runs).
func isDangerousURL(raw string) bool {
	s := strings.Map(func(r rune) rune {
		if r <= 0x20 {
			return -1
		}
		return r
	}, raw)
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "javascript:") ||
		strings.HasPrefix(s, "vbscript:") ||
		strings.HasPrefix(s, "data:")
}

func findFirstByID(root *html.Node, id string) *html.Node {
	var found *html.Node
	visit(root, func(n *html.Node) bool {
		if found != nil {
			return false
		}
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "id" && a.Val == id {
					found = n
					return false
				}
			}
		}
		return true
	})
	return found
}
