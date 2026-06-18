package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

const reportChannelBuffer = 4096
const topResolverLimit = 20

// DNSServer serves feed data over DNS TXT queries.
type DNSServer struct {
	domain       string   // main domain; seeds domains[0] (relay paths derive from cfg.Domain separately)
	domains      []string // main + extra sub-domains the server answers on (domains[0] == domain)
	feed         *Feed
	reader       *TelegramReader // nil when --no-telegram
	channelCtl   channelRefresher
	refreshers   []channelRefresher
	xReader      *XPublicReader
	queryKey     [protocol.KeySize]byte
	responseKey  [protocol.KeySize]byte
	listenAddr   string
	maxPadding   int
	allowManage  bool   // if true, admin/send commands are accepted
	channelsFile string // path to channels.txt for admin commands
	xAccounts    []string

	sessionsMu sync.Mutex
	sessions   map[uint16]*uploadSession

	// chat (optional) serves the standalone messenger on dedicated
	// sub-domains; chatDomains never overlap the feed domains.
	chat        *ChatService
	chatDomains []string

	reportCh   chan reportEvent
	reportFile *reportFile // optional rotating JSONL sink for the report TUI
	debug      bool
}

type channelFetchStats struct {
	Queries int64 `json:"queries"`
}

type reportEvent struct {
	channel  uint16
	resolver string
	domain   string // which configured domain the query arrived on
	invalid  bool   // GetBlock failed for this query
	chat     bool   // query landed on a chat sub-domain
}

type hourlyFetchReport struct {
	windowStart     time.Time
	totalQueries    int64
	metadataQueries int64
	versionQueries  int64
	mediaQueries    int64 // queries that landed in the media-blob channel range
	chatQueries     int64 // queries that landed on a chat sub-domain
	invalidQueries  int64 // GetBlock returned an error (unknown ch / blk OOR)
	perChannel      map[uint16]*channelFetchStats
	perResolver     map[string]int64
	perDomain       map[string]int64 // total queries per configured domain
}

type uploadSession struct {
	kind          protocol.UpstreamKind
	targetChannel uint8
	totalBlocks   uint8
	blocks        [][]byte
	received      []bool
	expiresAt     time.Time
}

type channelRefresher interface {
	UpdateChannels(channels []string)
	RequestRefresh()
}

// NewDNSServer creates a DNS server for the given domain.
func NewDNSServer(listenAddr, domain string, feed *Feed, queryKey, responseKey [protocol.KeySize]byte, maxPadding int, reader *TelegramReader, allowManage bool, channelsFile string, xAccounts []string, debug bool) *DNSServer {
	s := &DNSServer{
		domain:       strings.TrimSuffix(domain, "."),
		feed:         feed,
		reader:       reader,
		channelCtl:   reader,
		refreshers:   nil,
		queryKey:     queryKey,
		responseKey:  responseKey,
		listenAddr:   listenAddr,
		maxPadding:   maxPadding,
		allowManage:  allowManage,
		channelsFile: channelsFile,
		xAccounts:    append([]string{}, xAccounts...),
		sessions:     make(map[uint16]*uploadSession),
		reportCh:     make(chan reportEvent, reportChannelBuffer),
		debug:        debug,
	}
	s.domains = []string{s.domain}
	return s
}

// SetExtraDomains adds sub-domains the server also answers feed queries on.
// The main domain (from NewDNSServer) stays canonical for relay path
// derivation. Blanks, the main domain, and duplicates are ignored. Call
// before ListenAndServe.
func (s *DNSServer) SetExtraDomains(extra []string) {
	for _, d := range extra {
		d = strings.TrimSuffix(strings.TrimSpace(d), ".")
		if d == "" {
			continue
		}
		dup := false
		for _, existing := range s.domains {
			if strings.EqualFold(existing, d) {
				dup = true
				break
			}
		}
		if !dup {
			s.domains = append(s.domains, d)
		}
	}
}

// SetReportFile enables appending each hourly report as a JSON line to a
// rotating file (the canonical source for the `--report` dashboard). Empty path
// disables it. Call before ListenAndServe.
func (s *DNSServer) SetReportFile(path string) error {
	rf, err := openReportFile(path)
	if err != nil {
		return err
	}
	s.reportFile = rf
	return nil
}

// SetChatService attaches the chat handler on its dedicated sub-domains.
// Chat domains must not equal any feed domain (nesting under one is fine —
// longest-suffix matching routes them correctly). Call before ListenAndServe.
func (s *DNSServer) SetChatService(chat *ChatService) error {
	for _, cd := range chat.Domains() {
		cd = strings.TrimSuffix(strings.TrimSpace(cd), ".")
		if cd == "" {
			continue
		}
		for _, fd := range s.domains {
			if strings.EqualFold(cd, fd) {
				return fmt.Errorf("chat domain %q conflicts with feed domain %q", cd, fd)
			}
		}
		for _, existing := range s.chatDomains {
			if strings.EqualFold(cd, existing) {
				return fmt.Errorf("duplicate chat domain %q", cd)
			}
		}
		s.chatDomains = append(s.chatDomains, cd)
	}
	if len(s.chatDomains) == 0 {
		return fmt.Errorf("no usable chat domains")
	}
	s.chat = chat
	return nil
}

// isChatDomain reports whether domain is one of the chat sub-domains.
func (s *DNSServer) isChatDomain(domain string) bool {
	for _, d := range s.chatDomains {
		if strings.EqualFold(d, domain) {
			return true
		}
	}
	return false
}

// matchDomain returns the configured domain that qname belongs to, choosing
// the longest suffix match so a sub-domain that nests under another is handled
// correctly. Returns false when qname matches none of the configured domains.
func (s *DNSServer) matchDomain(qname string) (string, bool) {
	name := strings.ToLower(strings.TrimSuffix(qname, "."))
	matched := ""
	for _, d := range s.domains {
		dl := strings.ToLower(d)
		if name == dl || strings.HasSuffix(name, "."+dl) {
			if len(d) > len(matched) {
				matched = d
			}
		}
	}
	for _, d := range s.chatDomains {
		dl := strings.ToLower(d)
		if name == dl || strings.HasSuffix(name, "."+dl) {
			if len(d) > len(matched) {
				matched = d
			}
		}
	}
	return matched, matched != ""
}

// SetChannelRefresher allows wiring a non-Telegram channel source (e.g. public reader)
// for admin update/refresh operations.
func (s *DNSServer) SetChannelRefresher(channelCtl channelRefresher) {
	s.channelCtl = channelCtl
	if channelCtl != nil {
		s.refreshers = append(s.refreshers, channelCtl)
	}
}

// AddRefresher adds an additional source refresher (e.g., X reader) for admin refresh.
func (s *DNSServer) AddRefresher(channelCtl channelRefresher) {
	if channelCtl != nil {
		s.refreshers = append(s.refreshers, channelCtl)
	}
}

// SetXReader stores the XPublicReader so baseCh can be updated on channel changes.
func (s *DNSServer) SetXReader(xr *XPublicReader) {
	s.xReader = xr
}

// ListenAndServe starts the DNS server on UDP, shutting down when ctx is cancelled.
func (s *DNSServer) ListenAndServe(ctx context.Context) error {
	mux := dns.NewServeMux()
	for _, d := range s.domains {
		mux.HandleFunc(d+".", s.handleQuery)
	}
	for _, d := range s.chatDomains {
		mux.HandleFunc(d+".", s.handleQuery)
	}

	// Bind the UDP socket ourselves so its buffers can be enlarged: under a
	// query burst the kernel's default receive buffer overflows and silently
	// drops packets before the handler ever sees them. Best-effort — the OS
	// caps the value (e.g. net.core.rmem_max on Linux).
	pc, err := net.ListenPacket("udp", s.listenAddr)
	if err != nil {
		s.reportFile.Close()
		return err
	}
	if uc, ok := pc.(*net.UDPConn); ok {
		_ = uc.SetReadBuffer(8 << 20)
		_ = uc.SetWriteBuffer(8 << 20)
	}
	server := &dns.Server{
		PacketConn: pc,
		Handler:    mux,
	}

	go func() {
		<-ctx.Done()
		log.Println("[dns] shutting down...")
		server.Shutdown()
	}()

	// Run the hourly reporter on a child context so we can wait for its final
	// flush before closing the report file — otherwise the final report races
	// reportFile.Close() (and process exit) and is lost on shutdown.
	reportCtx, cancelReport := context.WithCancel(ctx)
	reportsDone := make(chan struct{})
	go func() {
		s.runHourlyReports(reportCtx)
		close(reportsDone)
	}()

	log.Printf("[dns] listening on %s (domains: %s)", s.listenAddr, strings.Join(s.domains, ", "))
	err = server.ActivateAndServe()

	// Server has stopped. Ensure the reporter exits, wait for its final flush
	// to hit disk, then close the file.
	cancelReport()
	<-reportsDone
	s.reportFile.Close()
	return err
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

	// Identify which configured domain this query belongs to (main or a
	// sub-domain) so the right suffix is stripped before decoding.
	domain, ok := s.matchDomain(q.Name)
	if !ok {
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	// Chat domains serve only chat. Chat queries are whitened end-to-end (no
	// feed-style ECB channel header), so they are dispatched by domain and
	// decoded with the chat codec — never run through DecodeQuery.
	if s.isChatDomain(domain) {
		if s.chat == nil {
			m.Rcode = dns.RcodeNameError
			w.WriteMsg(m)
			return
		}
		s.handleChatQuery(w, m, q, domain)
		return
	}

	channel, block, err := protocol.DecodeQuery(s.queryKey, q.Name, domain)
	if err != nil {
		log.Printf("[dns] decode query: %v", err)
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}

	// Feed domains never serve chat.
	if channel == protocol.ChatChannel {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	// Handle upstream init/data queries
	switch channel {
	case protocol.UpstreamInitChannel:
		s.handleUpstreamInitQuery(w, m, q, domain)
		return
	case protocol.UpstreamDataChannel:
		s.handleUpstreamDataQuery(w, m, q, domain)
		return
	}

	// Handle send-message queries
	if channel == protocol.SendChannel {
		s.handleSendQuery(w, m, q, domain)
		return
	}

	// Handle admin command queries
	if channel == protocol.AdminChannel {
		s.handleAdminQuery(w, m, q, domain)
		return
	}

	s.trackRequestStart(channel, resolverHost(w.RemoteAddr()), domain)
	if s.debug {
		log.Printf("[dns] query ch=%d blk=%d from=%s name=%s", channel, block, resolverHost(w.RemoteAddr()), q.Name)
	}

	data, err := s.feed.GetBlock(int(channel), int(block))
	if err != nil {
		// Don't log: there's a known protocol limitation where the metadata
		// channel doesn't carry a total-block count, so clients sometimes
		// over-fetch and ask for blocks/channels that don't exist. Counter
		// in the hourly report is enough.
		s.trackInvalidQuery()
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
			Ttl:    defaultResponseTTL,
		},
		Txt: txtParts,
	})

	w.WriteMsg(m)
}

// defaultResponseTTL is what we put on every DNS reply. A normal-looking
// value blends with real-world DNS — TTL=1 used to scream "non-standard
// traffic". Random-suffix queries stay uncacheable by uniqueness; only the
// deterministic ones (opt-in via the client setting) benefit from the
// resolver-side cache window this enables.
const defaultResponseTTL uint32 = 60

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
			Ttl:    defaultResponseTTL,
		},
		Txt: splitTXT(encoded),
	})
	w.WriteMsg(m)
}

func (s *DNSServer) handleChatQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question, domain string) {
	selector, counter, payload, err := protocol.DecodeChatCell(s.queryKey, q.Name, domain)
	if err != nil {
		if s.debug {
			log.Printf("[chat] decode cell: %v", err)
		}
		m.Rcode = dns.RcodeNameError
		w.WriteMsg(m)
		return
	}
	select {
	case s.reportCh <- reportEvent{chat: true, resolver: resolverHost(w.RemoteAddr()), domain: domain}:
	default:
	}
	if s.debug {
		log.Printf("[chat] cell sel=%x ctr=%d dom=%s name=%s from=%s", selector, counter, domain, q.Name, resolverHost(w.RemoteAddr()))
	}
	resp := s.chat.HandleCell(selector, counter, payload, domain, time.Now())
	s.writeEncodedResponse(w, m, q.Name, resp)
}

func (s *DNSServer) cleanupExpiredSessions(now time.Time) {
	for id, sess := range s.sessions {
		if now.After(sess.expiresAt) {
			delete(s.sessions, id)
		}
	}
}

func (s *DNSServer) handleUpstreamInitQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question, domain string) {
	if !s.allowManage {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	init, err := protocol.DecodeUpstreamInitQuery(s.queryKey, q.Name, domain)
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

func (s *DNSServer) handleUpstreamDataQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question, domain string) {
	if !s.allowManage {
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	sessionID, index, chunk, err := protocol.DecodeUpstreamBlockQuery(s.queryKey, q.Name, domain)
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

func (s *DNSServer) handleSendQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question, domain string) {
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

	targetCh, message, err := protocol.DecodeSendQuery(s.queryKey, q.Name, domain)
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

func (s *DNSServer) handleAdminQuery(w dns.ResponseWriter, m *dns.Msg, q dns.Question, domain string) {
	if !s.allowManage {
		log.Printf("[dns] admin query rejected: --allow-manage not set")
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	cmd, arg, err := protocol.DecodeAdminQuery(s.queryKey, q.Name, domain)
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

	all, err := loadChannelsFromFile(s.channelsFile)
	if err == nil {
		s.feed.SetChannels(combineDisplayChannels(all, s.xAccounts))
		if s.xReader != nil {
			s.xReader.SetBaseCh(len(all) + 1)
		}
		if s.channelCtl != nil {
			s.channelCtl.UpdateChannels(all)
			s.channelCtl.RequestRefresh()
		}
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

	s.feed.SetChannels(combineDisplayChannels(remaining, s.xAccounts))
	if s.xReader != nil {
		s.xReader.SetBaseCh(len(remaining) + 1)
	}
	if s.channelCtl != nil {
		s.channelCtl.UpdateChannels(remaining)
		s.channelCtl.RequestRefresh()
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
	if len(s.refreshers) == 0 {
		return "", fmt.Errorf("no active channel reader")
	}
	for _, refresher := range s.refreshers {
		refresher.RequestRefresh()
	}
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
		parts := strings.Fields(line)
		if len(parts) > 0 {
			channels = append(channels, strings.TrimPrefix(parts[0], "@"))
		}
	}
	return channels, scanner.Err()
}

func combineDisplayChannels(telegramChannels, xAccounts []string) []string {
	combined := make([]string, 0, len(telegramChannels)+len(xAccounts))
	combined = append(combined, telegramChannels...)
	for _, account := range xAccounts {
		combined = append(combined, "x/"+account)
	}
	return combined
}

func resolverHost(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err == nil {
		return host
	}
	return addr.String()
}

func (s *DNSServer) trackRequestStart(channel uint16, resolver, domain string) {
	// Exclude special control channels from per-channel content reporting.
	if channel == protocol.UpstreamInitChannel || channel == protocol.UpstreamDataChannel || channel == protocol.SendChannel || channel == protocol.AdminChannel {
		return
	}
	s.reportCh <- reportEvent{channel: channel, resolver: resolver, domain: domain}
}

func (s *DNSServer) trackInvalidQuery() {
	select {
	case s.reportCh <- reportEvent{invalid: true}:
	default:
	}
}

func (s *DNSServer) runHourlyReports(ctx context.Context) {
	rep := newHourlyFetchReport(time.Now())
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.drainReportEvents(rep)
			s.emitHourlyReport(rep, true)
			return
		case event := <-s.reportCh:
			recordReportQuery(rep, event)
		case <-ticker.C:
			s.emitHourlyReport(rep, false)
			rep = newHourlyFetchReport(time.Now())
		}
	}
}

func newHourlyFetchReport(start time.Time) *hourlyFetchReport {
	return &hourlyFetchReport{
		windowStart: start,
		perChannel:  make(map[uint16]*channelFetchStats),
		perResolver: make(map[string]int64),
		perDomain:   make(map[string]int64),
	}
}

func recordReportQuery(rep *hourlyFetchReport, event reportEvent) {
	if event.invalid {
		rep.invalidQueries++
		return
	}
	rep.totalQueries++
	if event.resolver != "" {
		rep.perResolver[event.resolver]++
	}
	if event.domain != "" {
		rep.perDomain[event.domain]++
	}
	if event.chat {
		rep.chatQueries++
		return
	}
	channel := event.channel
	if channel == protocol.MetadataChannel {
		rep.metadataQueries++
		return
	}
	if channel == protocol.VersionChannel {
		rep.versionQueries++
		return
	}
	if protocol.IsMediaChannel(channel) {
		// We don't fan out per-media-channel stats — the channel-id is just
		// a transient slot, and 50K possible ids would explode the report.
		// Total media-query volume is enough for the operator's purposes.
		rep.mediaQueries++
		return
	}

	stats := rep.perChannel[channel]
	if stats == nil {
		stats = &channelFetchStats{}
		rep.perChannel[channel] = stats
	}
	stats.Queries++
}

func (s *DNSServer) drainReportEvents(rep *hourlyFetchReport) {
	for {
		select {
		case event := <-s.reportCh:
			recordReportQuery(rep, event)
		default:
			return
		}
	}
}

func (s *DNSServer) emitHourlyReport(rep *hourlyFetchReport, final bool) {

	names := s.feed.ChannelNames()
	chs := make([]uint16, 0, len(rep.perChannel))
	for ch := range rep.perChannel {
		chs = append(chs, ch)
	}
	sort.Slice(chs, func(i, j int) bool { return chs[i] < chs[j] })

	type channelEntry struct {
		Channel uint16 `json:"channel"`
		Name    string `json:"name,omitempty"`
		Queries int64  `json:"queries"`
	}
	entries := make([]channelEntry, 0, len(chs))
	for _, ch := range chs {
		st := rep.perChannel[ch]
		name := ""
		if int(ch) >= 1 && int(ch) <= len(names) {
			name = names[int(ch)-1]
		}
		entries = append(entries, channelEntry{
			Channel: ch,
			Name:    name,
			Queries: st.Queries,
		})
	}

	type resolverEntry struct {
		Resolver string `json:"resolver"`
		Queries  int64  `json:"queries"`
	}
	resolvers := make([]resolverEntry, 0, len(rep.perResolver))
	for resolver, queries := range rep.perResolver {
		resolvers = append(resolvers, resolverEntry{Resolver: resolver, Queries: queries})
	}
	sort.Slice(resolvers, func(i, j int) bool {
		if resolvers[i].Queries == resolvers[j].Queries {
			return resolvers[i].Resolver < resolvers[j].Resolver
		}
		return resolvers[i].Queries > resolvers[j].Queries
	})
	if len(resolvers) > topResolverLimit {
		resolvers = resolvers[:topResolverLimit]
	}

	// Per-domain query totals (multi-domain): kept as its own report field.
	type domainEntry struct {
		Domain  string `json:"domain"`
		Queries int64  `json:"queries"`
	}
	domains := make([]domainEntry, 0, len(rep.perDomain))
	for d, q := range rep.perDomain {
		domains = append(domains, domainEntry{Domain: d, Queries: q})
	}
	sort.Slice(domains, func(i, j int) bool {
		if domains[i].Queries == domains[j].Queries {
			return domains[i].Domain < domains[j].Domain
		}
		return domains[i].Queries > domains[j].Queries
	})

	payload := map[string]any{
		"type":                 "dns_hourly_report",
		"from":                 rep.windowStart.UTC().Format(time.RFC3339),
		"to":                   time.Now().UTC().Format(time.RFC3339),
		"totalDnsQueries":      rep.totalQueries,
		"totalMetadataQueries": rep.metadataQueries,
		"totalVersionQueries":  rep.versionQueries,
		"totalMediaQueries":    rep.mediaQueries,
		"totalChatQueries":     rep.chatQueries,
		"totalInvalidQueries":  rep.invalidQueries,
		"channels":             entries,
		"topResolvers":         resolvers,
		"domains":              domains,
		"finalFlush":           final,
	}
	if mediaCache := s.feed.MediaCache(); mediaCache != nil {
		payload["mediaCache"] = mediaCache.Stats()
	}
	if s.chat != nil {
		payload["chat"] = s.chat.StatsSnapshot()
	}
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[dns_hourly] marshal error: %v", err)
		return
	}
	log.Printf("[dns_hourly] %s", string(b))
	if err := s.reportFile.Append(b); err != nil {
		log.Printf("[dns_hourly] write report file: %v", err)
	}
}
