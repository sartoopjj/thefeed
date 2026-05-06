package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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
	pageURL := pr.baseURL + "/" + url.PathEscape(username)
	body, err := httpGetBody(ctx, pr.client, pageURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", pageURL, err)
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
	if len(pics) == 0 {
		return
	}
	total := pr.feed.MergeProfilePics(pics)
	log.Printf("[public profile-pic] cycle done: %d new, %d total in bundle", len(pics), total)
}

// httpGetBody is a tiny GET helper. Cap is 8 MB.
func httpGetBody(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	return httpGetWithLimit(ctx, c, url, 8*1024*1024)
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
