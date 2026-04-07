package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// DNSServer serves feed data over DNS TXT queries.
type DNSServer struct {
	domain       string
	feed         *Feed
	reader       *TelegramReader // nil when --no-telegram
	queryKey     [protocol.KeySize]byte
	responseKey  [protocol.KeySize]byte
	listenAddr   string
	maxPadding   int
	allowManage  bool   // if true, admin/send commands are accepted
	channelsFile string // path to channels.txt for admin commands

	sessionsMu sync.Mutex
	sessions   map[uint16]*uploadSession

	mediaMu    sync.Mutex
	mediaCache map[string]*cachedMedia
}

type uploadSession struct {
	kind          protocol.UpstreamKind
	targetChannel uint8
	totalBlocks   uint8
	blocks        [][]byte
	received      []bool
	expiresAt     time.Time
}

type cachedMedia struct {
	fileName  string
	mimeType  string
	size      int
	blocks    [][]byte
	expiresAt time.Time
}

// NewDNSServer creates a DNS server for the given domain.
func NewDNSServer(listenAddr, domain string, feed *Feed, queryKey, responseKey [protocol.KeySize]byte, maxPadding int, reader *TelegramReader, allowManage bool, channelsFile string) *DNSServer {
	s := &DNSServer{
		domain:       strings.TrimSuffix(domain, "."),
		feed:         feed,
		reader:       reader,
		queryKey:     queryKey,
		responseKey:  responseKey,
		listenAddr:   listenAddr,
		maxPadding:   maxPadding,
		allowManage:  allowManage,
		channelsFile: channelsFile,
		sessions:     make(map[uint16]*uploadSession),
		mediaCache:   make(map[string]*cachedMedia),
	}
	return s
}

// ListenAndServe starts the DNS server on UDP, shutting down when ctx is cancelled.
func (s *DNSServer) ListenAndServe(ctx context.Context) error {
	mux := dns.NewServeMux()
	mux.HandleFunc(s.domain+".", s.handleQuery)

	server := &dns.Server{
		Addr:    s.listenAddr,
		Net:     "udp",
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		log.Println("[dns] shutting down...")
		server.Shutdown()
	}()

	log.Printf("[dns] listening on %s (domain: %s)", s.listenAddr, s.domain)
	return server.ListenAndServe()
}

func (s *DNSServer) handleQuery(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	if len(r.Question) == 0 {
		w.WriteMsg(m)
		return
	}

	q := r.Question[0]
	if q.Qtype != dns.TypeTXT {
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	channel, block, err := protocol.DecodeQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode query: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	// Handle upstream init/data queries
	switch channel {
	case protocol.UpstreamInitChannel:
		s.handleUpstreamInitQuery(w, m, q)
		return
	case protocol.UpstreamDataChannel:
		s.handleUpstreamDataQuery(w, m, q)
		return
	case protocol.MediaInitChannel:
		s.handleMediaInitQuery(w, m, q)
		return
	case protocol.MediaDataChannel:
		s.handleMediaDataQuery(w, m, q)
		return
	}

	// Handle send-message queries
	if channel == protocol.SendChannel {
		s.handleSendQuery(w, m, q)
		return
	}

	// Handle admin command queries
	if channel == protocol.AdminChannel {
		s.handleAdminQuery(w, m, q)
		return
	}

	data, err := s.feed.GetBlock(int(channel), int(block))
	if err != nil {
		log.Printf("[dns] get block ch=%d blk=%d: %v", channel, block, err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	encoded, err := protocol.EncodeResponse(s.responseKey, data, s.maxPadding)
	if err != nil {
		log.Printf("[dns] encode response: %v", err)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	// Split base64 string into 255-byte TXT chunks
	txtParts := splitTXT(encoded)

	m.Answer = append(m.Answer, &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET,
			Ttl:    1,
		},
		Txt: txtParts,
	})

	w.WriteMsg(m)
}

// splitTXT splits a string into 255-byte chunks for DNS TXT records.
func splitTXT(s string) []string {
	var parts []string
	for len(s) > 255 {
		parts = append(parts, s[:255])
		s = s[255:]
	}
	if len(s) > 0 {
		parts = append(parts, s)
	}
	return parts
}

func (s *DNSServer) writeEncodedResponse(w dns.ResponseWriter, m *dns.Msg, name string, data []byte) {
	encoded, err := protocol.EncodeResponse(s.responseKey, data, s.maxPadding)
	if err != nil {
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}
	m.Answer = append(m.Answer, &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   name,
			Rrtype: dns.TypeTXT,
			Class:  dns.ClassINET,
			Ttl:    1,
		},
		Txt: splitTXT(encoded),
	})
	w.WriteMsg(m)
}

func (s *DNSServer) cleanupExpiredSessions(now time.Time) {
	for id, sess := range s.sessions {
		if now.After(sess.expiresAt) {
			delete(s.sessions, id)
		}
	}
}

func (s *DNSServer) cleanupExpiredMedia(now time.Time) {
	for token, item := range s.mediaCache {
		if now.After(item.expiresAt) {
			delete(s.mediaCache, token)
		}
	}
}

func (s *DNSServer) handleMediaInitQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	if s.reader == nil {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	token, err := protocol.DecodeMediaInitQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode media init: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	now := time.Now()
	s.mediaMu.Lock()
	s.cleanupExpiredMedia(now)
	item, ok := s.mediaCache[token]
	s.mediaMu.Unlock()

	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		data, name, mime, err := s.reader.DownloadMedia(ctx, token)
		if err != nil {
			log.Printf("[dns] media download token=%s: %v", token, err)
			m.Rcode = dns.RcodeServerFailure
			w.WriteMsg(m)
			return
		}

		item = &cachedMedia{
			fileName:  name,
			mimeType:  mime,
			size:      len(data),
			blocks:    protocol.SplitIntoBlocks(data),
			expiresAt: now.Add(20 * time.Minute),
		}
		s.mediaMu.Lock()
		s.mediaCache[token] = item
		s.mediaMu.Unlock()
	}

	resp, _ := json.Marshal(map[string]any{
		"name":   item.fileName,
		"mime":   item.mimeType,
		"size":   item.size,
		"blocks": len(item.blocks),
	})
	s.writeEncodedResponse(w, m, q.Name, resp)
}

func (s *DNSServer) handleMediaDataQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	tokenBlock, token, err := protocol.DecodeMediaBlockQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode media block: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	now := time.Now()
	s.mediaMu.Lock()
	s.cleanupExpiredMedia(now)
	item, ok := s.mediaCache[token]
	if ok {
		item.expiresAt = now.Add(20 * time.Minute)
	}
	s.mediaMu.Unlock()

	if !ok || item == nil {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	idx := int(tokenBlock)
	if idx < 0 || idx >= len(item.blocks) {
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	s.writeEncodedResponse(w, m, q.Name, item.blocks[idx])
}

func (s *DNSServer) handleUpstreamInitQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	if !s.allowManage {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	init, err := protocol.DecodeUpstreamInitQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode upstream init: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	if init.Kind == protocol.UpstreamKindSend {
		if s.reader == nil {
			m.Rcode = dns.RcodeRefused
			w.WriteMsg(m)
			return
		}
	}

	now := time.Now()
	s.sessionsMu.Lock()
	s.cleanupExpiredSessions(now)
	s.sessions[init.SessionID] = &uploadSession{
		kind:          init.Kind,
		targetChannel: init.TargetChannel,
		totalBlocks:   init.TotalBlocks,
		blocks:        make([][]byte, init.TotalBlocks),
		received:      make([]bool, init.TotalBlocks),
		expiresAt:     now.Add(5 * time.Minute),
	}
	s.sessionsMu.Unlock()

	s.writeEncodedResponse(w, m, q.Name, []byte("READY"))
}

func (s *DNSServer) handleUpstreamDataQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	if !s.allowManage {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	sessionID, index, chunk, err := protocol.DecodeUpstreamBlockQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode upstream block: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	now := time.Now()
	s.sessionsMu.Lock()
	s.cleanupExpiredSessions(now)
	sess, ok := s.sessions[sessionID]
	if !ok || now.After(sess.expiresAt) {
		if ok {
			delete(s.sessions, sessionID)
		}
		s.sessionsMu.Unlock()
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}
	if int(index) >= len(sess.blocks) {
		s.sessionsMu.Unlock()
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}
	if !sess.received[index] {
		copied := make([]byte, len(chunk))
		copy(copied, chunk)
		sess.blocks[index] = copied
		sess.received[index] = true
	}
	sess.expiresAt = now.Add(5 * time.Minute)
	complete := true
	for _, received := range sess.received {
		if !received {
			complete = false
			break
		}
	}
	if !complete {
		s.sessionsMu.Unlock()
		s.writeEncodedResponse(w, m, q.Name, []byte("CONTINUE"))
		return
	}

	payload := make([]byte, 0)
	for _, block := range sess.blocks {
		payload = append(payload, block...)
	}
	delete(s.sessions, sessionID)
	s.sessionsMu.Unlock()

	result, err := s.executeUpstreamPayload(sess, payload)
	if err != nil {
		log.Printf("[dns] upstream execute: %v", err)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	s.writeEncodedResponse(w, m, q.Name, result)
}

func (s *DNSServer) executeUpstreamPayload(sess *uploadSession, payload []byte) ([]byte, error) {
	switch sess.kind {
	case protocol.UpstreamKindSend:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.reader.SendMessage(ctx, int(sess.targetChannel), string(payload)); err != nil {
			return nil, err
		}
		return []byte("OK"), nil
	case protocol.UpstreamKindAdmin:
		if len(payload) == 0 {
			return nil, fmt.Errorf("empty admin payload")
		}
		cmd := protocol.AdminCmd(payload[0])
		arg := ""
		if len(payload) > 1 {
			arg = string(payload[1:])
		}

		var result string
		var err error
		switch cmd {
		case protocol.AdminCmdAddChannel:
			result, err = s.adminAddChannel(arg)
		case protocol.AdminCmdRemoveChannel:
			result, err = s.adminRemoveChannel(arg)
		case protocol.AdminCmdListChannels:
			result, err = s.adminListChannels()
		case protocol.AdminCmdRefresh:
			result, err = s.adminRefresh()
		default:
			err = fmt.Errorf("unknown command: %d", cmd)
		}
		if err != nil {
			return nil, err
		}
		return []byte(result), nil
	default:
		return nil, fmt.Errorf("unknown upstream kind: %d", sess.kind)
	}
}

func (s *DNSServer) handleSendQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	if !s.allowManage {
		log.Printf("[dns] send query rejected: --allow-manage not set")
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	if s.reader == nil {
		log.Printf("[dns] send query rejected: no Telegram reader")
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	targetCh, message, err := protocol.DecodeSendQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode send query: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := s.reader.SendMessage(ctx, int(targetCh), string(message)); err != nil {
		log.Printf("[dns] send message to ch %d: %v", targetCh, err)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	// Respond with an ACK TXT record
	s.writeEncodedResponse(w, m, q.Name, []byte("OK"))
}

func (s *DNSServer) handleAdminQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question) {
	if !s.allowManage {
		log.Printf("[dns] admin query rejected: --allow-manage not set")
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	cmd, arg, err := protocol.DecodeAdminQuery(s.queryKey, q.Name, s.domain)
	if err != nil {
		log.Printf("[dns] decode admin query: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	var result string
	switch cmd {
	case protocol.AdminCmdAddChannel:
		result, err = s.adminAddChannel(string(arg))
	case protocol.AdminCmdRemoveChannel:
		result, err = s.adminRemoveChannel(string(arg))
	case protocol.AdminCmdListChannels:
		result, err = s.adminListChannels()
	case protocol.AdminCmdRefresh:
		result, err = s.adminRefresh()
	default:
		err = fmt.Errorf("unknown command: %d", cmd)
	}

	if err != nil {
		log.Printf("[dns] admin cmd=%d: %v", cmd, err)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
		return
	}

	s.writeEncodedResponse(w, m, q.Name, []byte(result))
}

func (s *DNSServer) adminAddChannel(username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", fmt.Errorf("empty channel name")
	}
	username = strings.TrimPrefix(username, "@")

	// Check if already exists
	existing, err := loadChannelsFromFile(s.channelsFile)
	if err != nil {
		return "", fmt.Errorf("read channels: %w", err)
	}
	for _, ch := range existing {
		if strings.EqualFold(ch, username) {
			return "already exists", nil
		}
	}

	// Append to file
	f, err := os.OpenFile(s.channelsFile, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return "", fmt.Errorf("open channels file: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "\n@%s\n", username); err != nil {
		return "", fmt.Errorf("write channel: %w", err)
	}

	log.Printf("[admin] added channel @%s", username)

	// Update the live reader and trigger immediate fetch.
	if s.reader != nil {
		all, _ := loadChannelsFromFile(s.channelsFile)
		s.reader.UpdateChannels(all)
		s.reader.RequestRefresh()
	}

	return "OK", nil
}

func (s *DNSServer) adminRemoveChannel(username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", fmt.Errorf("empty channel name")
	}
	username = strings.TrimPrefix(username, "@")

	existing, err := loadChannelsFromFile(s.channelsFile)
	if err != nil {
		return "", fmt.Errorf("read channels: %w", err)
	}

	found := false
	var remaining []string
	for _, ch := range existing {
		if strings.EqualFold(ch, username) {
			found = true
			continue
		}
		remaining = append(remaining, ch)
	}
	if !found {
		return "not found", nil
	}

	// Rewrite file
	content := "# Telegram channel usernames (one per line)\n"
	for _, ch := range remaining {
		content += "@" + ch + "\n"
	}
	if err := os.WriteFile(s.channelsFile, []byte(content), 0600); err != nil {
		return "", fmt.Errorf("write channels: %w", err)
	}

	log.Printf("[admin] removed channel @%s", username)

	// Update the live reader and trigger immediate fetch.
	if s.reader != nil {
		s.reader.UpdateChannels(remaining)
		s.reader.RequestRefresh()
	}

	return "OK", nil
}

func (s *DNSServer) adminListChannels() (string, error) {
	channels, err := loadChannelsFromFile(s.channelsFile)
	if err != nil {
		return "", err
	}
	return strings.Join(channels, "\n"), nil
}

func (s *DNSServer) adminRefresh() (string, error) {
	if s.reader == nil {
		return "", fmt.Errorf("no Telegram reader")
	}
	s.reader.RequestRefresh()
	log.Printf("[admin] hard refresh requested")
	return "OK", nil
}

func loadChannelsFromFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var channels []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		channels = append(channels, strings.TrimPrefix(line, "@"))
	}
	return channels, scanner.Err()
}
