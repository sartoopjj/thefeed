package server

import (
	"context"
	cryptoRand "crypto/rand"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// TelegramConfig holds Telegram API credentials.
type TelegramConfig struct {
	APIID       int
	APIHash     string
	Phone       string
	Password    string // 2FA password, empty if not used
	SessionPath string
	LoginOnly   bool // if true, authenticate and exit
	CodePrompt  func(ctx context.Context) (string, error)
}

// fileSessionStorage persists gotd session to a JSON file.
type fileSessionStorage struct {
	path string
}

func (f *fileSessionStorage) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, session.ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

func (f *fileSessionStorage) StoreSession(_ context.Context, data []byte) error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(f.path, data, 0600)
}

// TelegramReader fetches messages from Telegram channels.
type TelegramReader struct {
	cfg      TelegramConfig
	channels []string // channel usernames without @
	feed     *Feed
	msgLimit int // max messages to fetch per channel
	baseCh   int

	mu       sync.RWMutex
	cache    map[string]cachedMessages
	cacheTTL time.Duration

	// api is set once authenticated, used for sending messages.
	apiMu sync.RWMutex
	api   *tg.Client

	refreshCh chan struct{} // signals Run() to re-fetch immediately
}

// resolvedPeer holds the resolved Telegram peer along with its chat type.
type resolvedPeer struct {
	peer     tg.InputPeerClass
	chatType protocol.ChatType
	canSend  bool
	title    string
}

type cachedMessages struct {
	msgs    []protocol.Message
	fetched time.Time
}

// NewTelegramReader creates a reader for the given channel usernames.
func NewTelegramReader(cfg TelegramConfig, channelUsernames []string, feed *Feed, msgLimit int, baseCh int) *TelegramReader {
	cleaned := make([]string, len(channelUsernames))
	for i, u := range channelUsernames {
		cleaned[i] = strings.TrimPrefix(strings.TrimSpace(u), "@")
	}
	if msgLimit <= 0 {
		msgLimit = 15
	}
	if baseCh <= 0 {
		baseCh = 1
	}
	return &TelegramReader{
		cfg:       cfg,
		channels:  cleaned,
		feed:      feed,
		msgLimit:  msgLimit,
		baseCh:    baseCh,
		cache:     make(map[string]cachedMessages),
		cacheTTL:  10 * time.Minute,
		refreshCh: make(chan struct{}, 1),
	}
}

// Run starts the Telegram client, authenticates, and periodically fetches messages.
func (tr *TelegramReader) Run(ctx context.Context) error {
	opts := telegram.Options{}

	// Persist session to file if path is configured
	if tr.cfg.SessionPath != "" {
		opts.SessionStorage = &fileSessionStorage{path: tr.cfg.SessionPath}
	}

	client := telegram.NewClient(tr.cfg.APIID, tr.cfg.APIHash, opts)

	return client.Run(ctx, func(ctx context.Context) error {
		// Authenticate
		if err := tr.authenticate(ctx, client); err != nil {
			return fmt.Errorf("telegram auth: %w", err)
		}

		log.Println("[telegram] authenticated successfully")
		tr.feed.SetTelegramLoggedIn(true)

		// Login-only mode: just authenticate and return
		if tr.cfg.LoginOnly {
			log.Println("[telegram] login-only mode, session saved, exiting")
			return nil
		}

		api := client.API()

		tr.apiMu.Lock()
		tr.api = api
		tr.apiMu.Unlock()

		// Initial fetch
		tr.fetchAll(ctx, api)

		// Periodic fetch loop
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		tr.feed.SetNextFetch(uint32(time.Now().Add(10 * time.Minute).Unix()))

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				tr.fetchAll(ctx, api)
				tr.feed.SetNextFetch(uint32(time.Now().Add(10 * time.Minute).Unix()))
			case <-tr.refreshCh:
				// Invalidate cache so fetchAll re-fetches everything.
				tr.mu.Lock()
				tr.cache = make(map[string]cachedMessages)
				tr.mu.Unlock()
				tr.fetchAll(ctx, api)
				ticker.Reset(10 * time.Minute)
				tr.feed.SetNextFetch(uint32(time.Now().Add(10 * time.Minute).Unix()))
			}
		}
	})
}

// RequestRefresh signals the fetch loop to re-fetch immediately.
func (tr *TelegramReader) RequestRefresh() {
	select {
	case tr.refreshCh <- struct{}{}:
	default: // already pending
	}
}

// UpdateChannels replaces the channel list and updates the Feed accordingly.
func (tr *TelegramReader) UpdateChannels(channels []string) {
	cleaned := make([]string, len(channels))
	for i, u := range channels {
		cleaned[i] = strings.TrimPrefix(strings.TrimSpace(u), "@")
	}
	tr.mu.Lock()
	tr.channels = cleaned
	tr.cache = make(map[string]cachedMessages)
	tr.mu.Unlock()
}

func (tr *TelegramReader) authenticate(ctx context.Context, client *telegram.Client) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return err
	}
	if status.Authorized {
		return nil
	}

	codeAuth := auth.CodeAuthenticatorFunc(func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
		if tr.cfg.CodePrompt != nil {
			return tr.cfg.CodePrompt(ctx)
		}
		return "", fmt.Errorf("no code prompt configured")
	})

	var authConv auth.UserAuthenticator
	if tr.cfg.Password != "" {
		authConv = auth.Constant(tr.cfg.Phone, tr.cfg.Password, codeAuth)
	} else {
		authConv = auth.Constant(tr.cfg.Phone, "", codeAuth)
	}

	flow := auth.NewFlow(authConv, auth.SendCodeOptions{})
	return client.Auth().IfNecessary(ctx, flow)
}

func (tr *TelegramReader) fetchAll(ctx context.Context, api *tg.Client) {
	log.Printf("[telegram] fetch cycle started for %d channels", len(tr.channels))
	start := time.Now()
	var fetched, failed int
	for i, username := range tr.channels {
		chNum := tr.baseCh + i

		// Check cache
		tr.mu.RLock()
		cached, ok := tr.cache[username]
		tr.mu.RUnlock()
		if ok && time.Since(cached.fetched) < tr.cacheTTL {
			continue
		}

		// Resolve peer to get chat type info
		rp, err := tr.resolvePeer(ctx, api, username)
		if err != nil {
			log.Printf("[telegram] fetch %s: resolve peer failed: %v", username, err)
			failed++
			continue
		}

		hist, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:  rp.peer,
			Limit: tr.msgLimit,
		})
		if err != nil {
			log.Printf("[telegram] fetch %s: get history failed: %v", username, err)
			failed++
			continue
		}

		userNames := buildUserMap(hist)
		msgs, err := tr.extractMessages(hist, rp.chatType, userNames)
		if err != nil {
			log.Printf("[telegram] fetch %s: extract messages failed: %v", username, err)
			failed++
			continue
		}

		// Update cache
		tr.mu.Lock()
		tr.cache[username] = cachedMessages{msgs: msgs, fetched: time.Now()}
		tr.mu.Unlock()

		// Update feed with messages and chat type info
		tr.feed.UpdateChannel(chNum, msgs)
		tr.feed.SetChatInfo(chNum, rp.chatType, rp.canSend)
		tr.feed.SetChannelDisplayName(chNum, rp.title)
		fetched++
		log.Printf("[telegram] updated %s (%s): %d messages (type=%d, canSend=%v)", username, rp.title, len(msgs), rp.chatType, rp.canSend)
	}
	log.Printf("[telegram] fetch cycle done in %s: %d fetched, %d failed, %d total", time.Since(start).Round(time.Millisecond), fetched, failed, len(tr.channels))
}

// resolvePeer resolves a Telegram username to an InputPeer, handling channels,
// bots, and private chats.
func (tr *TelegramReader) resolvePeer(ctx context.Context, api *tg.Client, username string) (*resolvedPeer, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", username, err)
	}

	// Check Chats first (channels, supergroups)
	for _, chat := range resolved.Chats {
		if ch, ok := chat.(*tg.Channel); ok {
			canSend := !ch.Broadcast || ch.Creator || ch.AdminRights.PostMessages
			return &resolvedPeer{
				peer: &tg.InputPeerChannel{
					ChannelID:  ch.ID,
					AccessHash: ch.AccessHash,
				},
				chatType: protocol.ChatTypeChannel,
				canSend:  canSend,
				title:    ch.Title,
			}, nil
		}
	}

	// Check Users (bots, private chats)
	for _, u := range resolved.Users {
		if user, ok := u.(*tg.User); ok {
			return &resolvedPeer{
				peer: &tg.InputPeerUser{
					UserID:     user.ID,
					AccessHash: user.AccessHash,
				},
				chatType: protocol.ChatTypePrivate,
				canSend:  true,
				title:    user.FirstName,
			}, nil
		}
	}

	return nil, fmt.Errorf("%s not found in resolved chats or users", username)
}

func (tr *TelegramReader) fetchChannel(ctx context.Context, api *tg.Client, username string) ([]protocol.Message, error) {
	rp, err := tr.resolvePeer(ctx, api, username)
	if err != nil {
		return nil, err
	}

	hist, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  rp.peer,
		Limit: tr.msgLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("get history %s: %w", username, err)
	}

	userNames := buildUserMap(hist)
	return tr.extractMessages(hist, protocol.ChatTypeChannel, userNames)
}

// buildUserMap extracts a user ID → display name map from a history response.
func buildUserMap(hist tg.MessagesMessagesClass) map[int64]string {
	var users []tg.UserClass
	switch h := hist.(type) {
	case *tg.MessagesMessages:
		users = h.Users
	case *tg.MessagesMessagesSlice:
		users = h.Users
	case *tg.MessagesChannelMessages:
		users = h.Users
	}
	m := make(map[int64]string, len(users))
	for _, u := range users {
		if user, ok := u.(*tg.User); ok {
			name := user.FirstName
			if user.LastName != "" {
				name += " " + user.LastName
			}
			if name == "" {
				name = user.Username
			}
			if name != "" {
				m[user.ID] = name
			}
		}
	}
	return m
}

func (tr *TelegramReader) extractMessages(hist tg.MessagesMessagesClass, chatType protocol.ChatType, userNames map[int64]string) ([]protocol.Message, error) {
	var tgMsgs []tg.MessageClass

	switch h := hist.(type) {
	case *tg.MessagesMessages:
		tgMsgs = h.Messages
	case *tg.MessagesMessagesSlice:
		tgMsgs = h.Messages
	case *tg.MessagesChannelMessages:
		tgMsgs = h.Messages
	default:
		return nil, fmt.Errorf("unexpected messages type: %T", hist)
	}

	var msgs []protocol.Message
	for _, raw := range tgMsgs {
		msg, ok := raw.(*tg.Message)
		if !ok {
			continue
		}

		text := tr.extractText(msg)
		if text == "" {
			continue
		}

		// For private chats, prefix with the sender's name.
		if chatType == protocol.ChatTypePrivate {
			if fromID, ok := msg.GetFromID(); ok {
				if pu, ok := fromID.(*tg.PeerUser); ok {
					if name, ok := userNames[pu.UserID]; ok {
						text = name + ": " + text
					}
				}
			}
		}

		// Mark messages that are replies (include reply-to message ID).
		if replyTo, hasReply := msg.GetReplyTo(); hasReply {
			if rh, ok := replyTo.(*tg.MessageReplyHeader); ok {
				if rid, hasID := rh.GetReplyToMsgID(); hasID {
					text = fmt.Sprintf("%s:%d\n%s", protocol.MediaReply, rid, text)
				} else {
					text = protocol.MediaReply + "\n" + text
				}
			} else {
				text = protocol.MediaReply + "\n" + text
			}
		}

		msgs = append(msgs, protocol.Message{
			ID:        uint32(msg.ID),
			Timestamp: uint32(msg.Date),
			Text:      text,
		})
	}

	return msgs, nil
}

func (tr *TelegramReader) extractText(msg *tg.Message) string {
	text := applyTextURLEntities(msg.Message, msg.Entities)

	mediaPrefix := ""
	if msg.Media != nil {
		switch msg.Media.(type) {
		case *tg.MessageMediaPhoto:
			mediaPrefix = protocol.MediaImage
		case *tg.MessageMediaDocument:
			mediaPrefix = tr.classifyDocument(msg.Media.(*tg.MessageMediaDocument))
		case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive, *tg.MessageMediaVenue:
			mediaPrefix = protocol.MediaLocation
		case *tg.MessageMediaContact:
			mediaPrefix = protocol.MediaContact
		case *tg.MessageMediaPoll:
			mediaPrefix = protocol.MediaPoll
			pollMedia := msg.Media.(*tg.MessageMediaPoll)
			question := pollMedia.Poll.Question.Text
			var opts []string
			for _, a := range pollMedia.Poll.Answers {
				opts = append(opts, "○ "+a.Text.Text)
			}
			pollBody := "📊 " + question
			if len(opts) > 0 {
				pollBody += "\n" + strings.Join(opts, "\n")
			}
			if text != "" {
				return mediaPrefix + "\n" + pollBody + "\n" + text
			}
			return mediaPrefix + "\n" + pollBody
		}
	}

	if mediaPrefix != "" {
		if text != "" {
			return mediaPrefix + "\n" + text
		}
		return mediaPrefix
	}

	return text
}

// applyTextURLEntities embeds hyperlink URLs from MessageEntityTextURL entities
// into the message text, producing output like "[display text](https://url)".
// This mirrors what the public HTML reader does when it extracts <a> tags.
// Offsets are in UTF-16 code units per the Telegram API spec.
func applyTextURLEntities(text string, entities []tg.MessageEntityClass) string {
	type textURL struct {
		offset int
		length int
		url    string
	}
	var urls []textURL
	for _, e := range entities {
		tu, ok := e.(*tg.MessageEntityTextURL)
		if !ok {
			continue
		}
		u := tu.URL
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			continue
		}
		urls = append(urls, textURL{tu.Offset, tu.Length, u})
	}
	if len(urls) == 0 {
		return text
	}

	// Process from end to start so earlier offsets stay valid.
	sort.Slice(urls, func(i, j int) bool { return urls[i].offset > urls[j].offset })

	runes := []rune(text)

	// Build UTF-16 offset → rune index mapping.
	utf16Pos := 0
	utf16ToRune := make(map[int]int, len(runes)+1)
	for i, r := range runes {
		utf16ToRune[utf16Pos] = i
		if r > 0xFFFF {
			utf16Pos += 2
		} else {
			utf16Pos++
		}
	}
	utf16ToRune[utf16Pos] = len(runes)

	for _, u := range urls {
		startIdx, ok1 := utf16ToRune[u.offset]
		endIdx, ok2 := utf16ToRune[u.offset+u.length]
		if !ok1 || !ok2 {
			continue
		}
		// Skip if the display text already IS the URL.
		if string(runes[startIdx:endIdx]) == u.url {
			continue
		}
		label := string(runes[startIdx:endIdx])
		replacement := []rune("[" + label + "](" + u.url + ")")
		newRunes := make([]rune, 0, len(runes)-len([]rune(label))+len(replacement))
		newRunes = append(newRunes, runes[:startIdx]...)
		newRunes = append(newRunes, replacement...)
		newRunes = append(newRunes, runes[endIdx:]...)
		runes = newRunes
	}

	return string(runes)
}

func (tr *TelegramReader) classifyDocument(media *tg.MessageMediaDocument) string {
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		return protocol.MediaFile
	}

	for _, attr := range doc.Attributes {
		switch attr.(type) {
		case *tg.DocumentAttributeVideo:
			return protocol.MediaVideo
		case *tg.DocumentAttributeAudio:
			return protocol.MediaAudio
		case *tg.DocumentAttributeSticker:
			return protocol.MediaSticker
		case *tg.DocumentAttributeAnimated:
			return protocol.MediaGIF
		}
	}

	return protocol.MediaFile
}

// SendMessage sends a text message to the given channel/chat (1-indexed).
func (tr *TelegramReader) SendMessage(ctx context.Context, channelNum int, text string) error {
	if channelNum < 1 || channelNum > len(tr.channels) {
		return fmt.Errorf("invalid channel number: %d", channelNum)
	}

	tr.apiMu.RLock()
	api := tr.api
	tr.apiMu.RUnlock()
	if api == nil {
		return fmt.Errorf("not authenticated")
	}

	username := tr.channels[channelNum-1]
	rp, err := tr.resolvePeer(ctx, api, username)
	if err != nil {
		return fmt.Errorf("send resolve: %w", err)
	}

	var ridBuf [8]byte
	if _, ridErr := cryptoRand.Read(ridBuf[:]); ridErr != nil {
		return fmt.Errorf("generate random id: %w", ridErr)
	}
	randomID := int64(ridBuf[0]) | int64(ridBuf[1])<<8 | int64(ridBuf[2])<<16 | int64(ridBuf[3])<<24 |
		int64(ridBuf[4])<<32 | int64(ridBuf[5])<<40 | int64(ridBuf[6])<<48 | int64(ridBuf[7])<<56

	_, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     rp.peer,
		Message:  text,
		RandomID: randomID,
	})
	if err != nil {
		return fmt.Errorf("send to %s: %w", username, err)
	}

	log.Printf("[telegram] sent message to %s (%d chars)", username, len(text))
	return nil
}
