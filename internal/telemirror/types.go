// Package telemirror is an optional, removable backup feed.
// It fetches public Telegram channel widgets through Google Translate's
// public web-page proxy so users can browse public channels even when
// Telegram itself is blocked. The implementation is intentionally
// self-contained so the feature can be removed by deleting this package
// and the matching handlers in internal/web/telemirror.go.
package telemirror

import (
	"errors"
	"strings"
	"time"
)

// DefaultChannels are pinned in the UI; users cannot remove them.
var DefaultChannels = []string{"VahidOnline", "networkti", "thefeedconfig"}

// Channel describes the public channel header.
type Channel struct {
	Username    string `json:"username"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Photo       string `json:"photo,omitempty"`
	Subscribers string `json:"subscribers,omitempty"`
}

// Media is one attachment on a post.
type Media struct {
	Type     string   `json:"type"` // photo | video | voice | audio | document | sticker | poll
	URL      string   `json:"url,omitempty"`
	Thumb    string   `json:"thumb,omitempty"`
	Duration string   `json:"duration,omitempty"`
	Title    string   `json:"title,omitempty"`    // file name / poll question / audio title
	Subtitle string   `json:"subtitle,omitempty"`
	Options  []string `json:"options,omitempty"`  // poll options
}

type Reaction struct {
	Emoji string `json:"emoji"`
	Count string `json:"count,omitempty"`
}

// Reply is the quoted preview shown above a reply.
type Reply struct {
	Author string `json:"author,omitempty"`
	Text   string `json:"text,omitempty"` // inner HTML of the snippet
	URL    string `json:"url,omitempty"`  // link to the replied-to post
}

// Forward is the "Forwarded from <name>" header.
type Forward struct {
	Author string `json:"author,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Post is a single message from the channel feed.
type Post struct {
	ID        string     `json:"id"` // "<channel>/<msgid>"
	Author    string     `json:"author,omitempty"`
	Text      string     `json:"text,omitempty"` // sanitised inner HTML
	Reply     *Reply     `json:"reply,omitempty"`
	Forward   *Forward   `json:"forward,omitempty"`
	Media     []Media    `json:"media,omitempty"`
	Reactions []Reaction `json:"reactions,omitempty"`
	Time      time.Time  `json:"time,omitempty"`
	Views     string     `json:"views,omitempty"`
	Edited    bool       `json:"edited,omitempty"`
}

// FetchResult is what we cache per channel.
type FetchResult struct {
	Channel   Channel   `json:"channel"`
	Posts     []Post    `json:"posts"`
	FetchedAt time.Time `json:"fetchedAt"`
}

// Sentinel errors returned by Store.
var (
	ErrEmptyUsername = errors.New("empty username")
	ErrPinnedChannel = errors.New("pinned channel cannot be removed")
)

// SanitizeUsername strips @ / t.me/ prefixes and rejects characters not
// allowed by Telegram's username rules. Returns "" if the result is empty.
func SanitizeUsername(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"https://t.me/", "http://t.me/", "t.me/", "s/", "@"} {
		s = strings.TrimPrefix(s, p)
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			out = append(out, r)
		}
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return string(out)
}

// IsDefault reports whether username is one of the pinned defaults.
func IsDefault(username string) bool {
	for _, d := range DefaultChannels {
		if strings.EqualFold(d, username) {
			return true
		}
	}
	return false
}
