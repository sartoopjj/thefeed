package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Nitter / RSS feeds put the channel image at <rss><channel><image><url>.
type xRSSImageEnvelope struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Image struct {
			URL string `xml:"url"`
		} `xml:"image"`
	} `xml:"channel"`
}

func extractXAvatarURL(body []byte) string {
	var env xRSSImageEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return ""
	}
	return strings.TrimSpace(env.Channel.Image.URL)
}

// fetchXAvatar tries each Nitter instance and returns the first
// avatar that downloads. (nil, nil) when no instance had one.
func (xr *XPublicReader) fetchXAvatar(ctx context.Context, account string) ([]byte, error) {
	const maxAvatarBytes = 512 * 1024
	var lastErr error
	for _, instance := range xr.instances {
		rssURL := strings.TrimSuffix(instance, "/") + "/" + url.PathEscape(account) + "/rss"
		body, err := xRSSGet(ctx, xr.client, rssURL)
		if err != nil {
			lastErr = err
			continue
		}
		avatarURL := extractXAvatarURL(body)
		if avatarURL == "" {
			continue
		}
		// Some Nitter builds return a relative "/pic/..." path.
		if strings.HasPrefix(avatarURL, "/") {
			avatarURL = strings.TrimSuffix(instance, "/") + avatarURL
		}
		imgBytes, err := httpGetWithLimit(ctx, xr.client, avatarURL, maxAvatarBytes)
		if err != nil {
			lastErr = err
			continue
		}
		return imgBytes, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

// fetchAllXProfilePhotos downloads each account's avatar (via Nitter)
// and merges into the bundle under "x:<handle>" keys.
func (xr *XPublicReader) fetchAllXProfilePhotos(ctx context.Context) {
	xr.mu.RLock()
	accounts := append([]string(nil), xr.accounts...)
	xr.mu.RUnlock()

	pics := make(map[string][]byte, len(accounts))
	var picsMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		wg.Add(1)
		go func(account string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, err := xr.fetchXAvatar(ctx, account)
			if err != nil {
				log.Printf("[x profile-pic] @%s: %v", account, err)
				return
			}
			if len(body) == 0 {
				return
			}
			// "x:" prefix avoids colliding with same-name Telegram channels.
			picsMu.Lock()
			pics["x:"+account] = body
			picsMu.Unlock()
		}(a)
	}
	wg.Wait()
	if len(pics) == 0 {
		return
	}
	total := xr.feed.MergeProfilePics(pics)
	log.Printf("[x profile-pic] cycle done: %d new, %d total in bundle", len(pics), total)
}

// xRSSGet mirrors the per-instance RSS request (same UA + Accept).
func xRSSGet(ctx context.Context, c *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; thefeed/1.0; +https://github.com/sartoopjj/thefeed)")
	req.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, text/xml;q=0.8")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("rss %s: status %s", u, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxXRSSBodyBytes))
	if err != nil {
		return nil, err
	}
	return body, nil
}
