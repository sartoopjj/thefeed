package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/html"
)

// extractPublicAvatarURL finds the channel avatar URL on a t.me/s page.
// Returns "" when the channel has no photo or the layout is unfamiliar.
func extractPublicAvatarURL(body []byte) string {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return ""
	}
	if i := findFirstByClass(doc, "tgme_page_photo_image"); i != nil {
		if img := firstImgChild(i); img != nil {
			if src := attrValue(img, "src"); src != "" {
				return src
			}
		}
		if attrValue(i, "src") != "" {
			return attrValue(i, "src")
		}
	}
	if u := findFirstByClass(doc, "tgme_widget_message_user_photo"); u != nil {
		if src := attrValue(u, "src"); src != "" {
			return src
		}
		if img := firstImgChild(u); img != nil {
			return attrValue(img, "src")
		}
	}
	return ""
}

func firstImgChild(n *html.Node) *html.Node {
	if n == nil {
		return nil
	}
	var found *html.Node
	visitNodes(n, func(c *html.Node) {
		if found != nil || c.Type != html.ElementNode {
			return
		}
		if c.Data == "img" {
			found = c
		}
	})
	return found
}

// fetchPublicAvatar downloads the avatar from t.me/s/<username>.
// Returns nil bytes when the channel has no photo.
func (pr *PublicReader) fetchPublicAvatar(ctx context.Context, username string) ([]byte, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, nil
	}
	body, err := pr.fetchPageBody(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("fetch %s page: %w", username, err)
	}
	imgURL := extractPublicAvatarURL(body)
	if imgURL == "" {
		return nil, nil
	}
	const maxAvatarBytes = 512 * 1024
	imgBytes, err := httpGetWithLimit(ctx, pr.client, imgURL, maxAvatarBytes)
	if err != nil {
		return nil, fmt.Errorf("download avatar %s: %w", imgURL, err)
	}
	return imgBytes, nil
}

// fetchAllPublicProfilePhotos scrapes each channel's avatar from
// t.me/s/<u> and merges into the bundle. Best-effort.
func (pr *PublicReader) fetchAllPublicProfilePhotos(ctx context.Context) {
	pr.mu.RLock()
	channels := append([]string(nil), pr.channels...)
	pr.mu.RUnlock()

	pics := make(map[string][]byte, len(channels))
	current := make(map[string]bool, len(channels))
	var picsMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // cap concurrency vs t.me
	for _, u := range channels {
		if ctx.Err() != nil {
			return
		}
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		current[u] = true
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, err := pr.fetchPublicAvatar(ctx, u)
			if err != nil {
				log.Printf("[public profile-pic] %s: %v", u, err)
				return
			}
			if len(body) == 0 {
				return
			}
			picsMu.Lock()
			pics[u] = body
			picsMu.Unlock()
		}(u)
	}
	wg.Wait()
	// No photo found is not "no photo": a page layout change looks the same,
	// so only channels gone from the list lose theirs here.
	total, dropped := pr.feed.RefreshProfilePics(pics, current, false)
	if len(pics) > 0 || len(dropped) > 0 {
		log.Printf("[public profile-pic] cycle done: %d fetched, %d dropped, %d total in bundle", len(pics), len(dropped), total)
	}
}

func httpGetWithLimit(ctx context.Context, c *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; thefeed/1.0; +https://github.com/sartoopjj/thefeed)")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("http %s: status %s", u, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("http %s: response exceeds %d bytes", u, limit)
	}
	return body, nil
}
