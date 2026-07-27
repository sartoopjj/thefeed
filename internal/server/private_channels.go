package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// privateChannelID derives a short, stable channel ID from an invite
// hash. Keeps Feed channel Names and DNS metadata small while still
// preserving cache stability across file reorders. 4-byte SHA-256
// prefix; collision space ~4 billion, more than enough per server.
func privateChannelID(inviteHash string) string {
	sum := sha256.Sum256([]byte(inviteHash))
	return "pv:" + hex.EncodeToString(sum[:4])
}

// ParseInviteHash extracts the invite hash from any supported
// invite-link shape: t.me/+HASH, t.me/joinchat/HASH, tg://join?invite=HASH,
// or bare HASH (with or without leading +). Public-username URLs
// (t.me/foo without + or joinchat/) are rejected.
func ParseInviteHash(link string) (string, error) {
	s := strings.TrimSpace(link)
	if s == "" {
		return "", fmt.Errorf("empty invite link")
	}

	// tg://join?invite=HASH or telegram://join?invite=HASH — capture
	// the `invite=` param before any query-string stripping.
	if lower := strings.ToLower(s); strings.HasPrefix(lower, "tg://") || strings.HasPrefix(lower, "telegram://") {
		if i := strings.Index(s, "invite="); i >= 0 {
			h := s[i+len("invite="):]
			if j := strings.IndexAny(h, "&#"); j >= 0 {
				h = h[:j]
			}
			return validateHash(h)
		}
		return "", fmt.Errorf("not an invite link: %q (tg://join requires ?invite=HASH)", link)
	}

	// Strip query / fragment and trailing slashes.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")

	// Strip URL scheme.
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")

	// If a t.me-family host is present, require /+HASH or /joinchat/HASH
	// after it. Anything else (e.g. t.me/publicchannel) is a public
	// username and doesn't belong here.
	for _, host := range []string{"t.me/", "telegram.me/", "telegram.dog/"} {
		if strings.HasPrefix(s, host) {
			rest := s[len(host):]
			if strings.HasPrefix(rest, "+") {
				return validateHash(rest[1:])
			}
			if strings.HasPrefix(rest, "joinchat/") {
				return validateHash(rest[len("joinchat/"):])
			}
			return "", fmt.Errorf("not an invite link: %q (expected /+HASH or /joinchat/HASH after %s)", link, host)
		}
	}

	// Bare form: +HASH or HASH (no URL host).
	s = strings.TrimPrefix(s, "+")
	return validateHash(s)
}

var inviteHashRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func validateHash(h string) (string, error) {
	h = strings.TrimSpace(h)
	if !inviteHashRE.MatchString(h) {
		return "", fmt.Errorf("invalid invite hash %q (expected 8–64 chars of [A-Za-z0-9_-])", h)
	}
	return h, nil
}

// LoadPrivateInvites reads invite links from path (one per line, `#`
// comments allowed) and returns the parsed hashes. Missing file is
// not an error — the feature is opt-in.
func LoadPrivateInvites(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("[server] close private invites file: %v", err)
		}
	}()

	var hashes []string
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		hash, err := ParseInviteHash(parts[0])
		if err != nil {
			log.Printf("[server] private_channels.txt:%d: %v", lineNum, err)
			continue
		}
		if seen[hash] {
			continue
		}
		seen[hash] = true
		hashes = append(hashes, hash)
	}
	return hashes, scanner.Err()
}

// privatePeers holds resolved invite hashes after a successful join.
// Keyed by invite hash; the value is the InputPeerChannel the fetch
// loop uses to call MessagesGetHistory.
type privatePeers struct {
	mu      sync.RWMutex
	byHash  map[string]*resolvedPeer
	ordered []string // hashes in declaration order, drives fetch ordering
}

func newPrivatePeers(hashes []string) *privatePeers {
	pp := &privatePeers{byHash: make(map[string]*resolvedPeer)}
	for _, h := range hashes {
		pp.ordered = append(pp.ordered, h)
	}
	return pp
}

func (p *privatePeers) get(hash string) (*resolvedPeer, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.byHash[hash]
	return r, ok
}

func (p *privatePeers) set(hash string, r *resolvedPeer) {
	p.mu.Lock()
	p.byHash[hash] = r
	p.mu.Unlock()
}

// joinOrCheckInvite returns the InputPeerChannel for a private
// channel, joining it via importChatInvite if not already a member.
func joinOrCheckInvite(ctx context.Context, api *tg.Client, hash string) (*resolvedPeer, error) {
	check, err := api.MessagesCheckChatInvite(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("check invite %q: %w", hash, err)
	}

	// chatInviteAlready: we're a member. Use the embedded chat directly.
	if already, ok := check.(*tg.ChatInviteAlready); ok {
		return chatToResolvedPeer(already.Chat, "private channel")
	}
	// chatInvitePeek: peek (preview) — usually means we need to join.
	// Fall through to ImportChatInvite below.

	// Not a member yet → import (join).
	updates, err := api.MessagesImportChatInvite(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("import invite %q: %w", hash, err)
	}
	// Updates can be one of several shapes; we look for embedded chats.
	chats := extractChatsFromUpdates(updates)
	if len(chats) == 0 {
		return nil, fmt.Errorf("import invite %q: no chat in response", hash)
	}
	return chatToResolvedPeer(chats[0], "private channel")
}

// chatToResolvedPeer converts a tg.ChatClass to our internal peer
// representation. Only channels/supergroups are supported — legacy
// basic groups (tg.Chat without access hash) are rejected.
func chatToResolvedPeer(c tg.ChatClass, fallbackTitle string) (*resolvedPeer, error) {
	ch, ok := c.(*tg.Channel)
	if !ok {
		return nil, fmt.Errorf("invite resolved to non-channel %T (basic groups not supported)", c)
	}
	title := strings.TrimSpace(ch.Title)
	if title == "" {
		title = fallbackTitle
	}
	canSend := !ch.Broadcast || ch.Creator || ch.AdminRights.PostMessages
	pid, _ := extractChatPhotoID(ch.Photo)
	return &resolvedPeer{
		peer: &tg.InputPeerChannel{
			ChannelID:  ch.ID,
			AccessHash: ch.AccessHash,
		},
		chatType: protocol.ChatTypeChannel,
		canSend:  canSend,
		title:    title,
		photoID:  pid,
	}, nil
}

// extractChatsFromUpdates pulls tg.ChatClass values out of any of the
// gotd Updates variants. ImportChatInvite returns
// tg.UpdatesClass — concretely usually *tg.Updates which has .Chats.
func extractChatsFromUpdates(updates tg.UpdatesClass) []tg.ChatClass {
	switch u := updates.(type) {
	case *tg.Updates:
		return u.Chats
	case *tg.UpdatesCombined:
		return u.Chats
	}
	return nil
}

// resolveAllPrivate resolves every queued invite hash, logging and
// skipping individual failures. betweenCalls paces requests below
// Telegram's FLOOD_WAIT threshold.
func (p *privatePeers) resolveAllPrivate(ctx context.Context, api *tg.Client, betweenCalls time.Duration) {
	for i, hash := range p.ordered {
		if ctx.Err() != nil {
			return
		}
		if _, ok := p.get(hash); ok {
			continue
		}
		rp, err := joinOrCheckInvite(ctx, api, hash)
		if err != nil {
			log.Printf("[telegram] private channel %d/%d (hash=%s): %v",
				i+1, len(p.ordered), hash, err)
			continue
		}
		p.set(hash, rp)
		log.Printf("[telegram] joined private channel %q (id=%d, hash=%s)",
			rp.title, rp.peer.(*tg.InputPeerChannel).ChannelID, hash)
		if betweenCalls > 0 && i < len(p.ordered)-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(betweenCalls):
			}
		}
	}
}
