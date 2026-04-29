package web

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// mediaDLProgress tracks how many blocks of a single in-flight download have
// been fetched. The frontend polls /api/media/progress to drive a smooth
// per-block counter while the xhr is still reading the response body.
type mediaDLProgress struct {
	completed int32
	total     int32
}

// mediaProgressKey is the join of (channel, blockCount, crc) the frontend
// uses to look up its own download. It matches the params on the GET URL so
// no extra bookkeeping leaks into the JSON response.
func mediaProgressKey(channel uint16, blocks uint16, crc uint32) string {
	return fmt.Sprintf("%d:%d:%08x", channel, blocks, crc)
}

func (s *Server) handleMediaProgress(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ch64, _ := strconv.ParseUint(q.Get("ch"), 10, 16)
	blk64, _ := strconv.ParseUint(q.Get("blk"), 10, 16)
	crc64, _ := strconv.ParseUint(strings.TrimSpace(q.Get("crc")), 16, 32)
	key := mediaProgressKey(uint16(ch64), uint16(blk64), uint32(crc64))

	s.dlMu.Lock()
	prog := s.dlProgress[key]
	s.dlMu.Unlock()
	if prog == nil {
		writeJSON(w, map[string]any{"active": false, "completed": 0, "total": int(blk64)})
		return
	}
	writeJSON(w, map[string]any{
		"active":    true,
		"completed": int(atomic.LoadInt32(&prog.completed)),
		"total":     int(atomic.LoadInt32(&prog.total)),
	})
}

// handleMediaGet streams a media blob assembled from the
// (channel, blocks, crc) tuple embedded in a message's text.
//
// Query string:
//
//	ch=<uint16>      media channel number (10000..60000)
//	blk=<uint16>     total block count
//	size=<bytes>     expected file size (Content-Length)
//	crc=<hex8>       expected CRC32 of full body
//	name=<filename>  optional filename for Content-Disposition
//	type=<mime>      optional mime type override; sanitized
func (s *Server) handleMediaGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	source := strings.ToLower(strings.TrimSpace(q.Get("source")))
	if source == "" {
		source = "slow"
	}

	const maxClaimedSize = 100 * 1024 * 1024
	expectedSize, _ := strconv.ParseInt(q.Get("size"), 10, 64)
	if expectedSize < 0 || expectedSize > maxClaimedSize {
		http.Error(w, "bad size", http.StatusBadRequest)
		return
	}

	expectedCRC := uint32(0)
	if v := strings.TrimSpace(q.Get("crc")); v != "" {
		c, err := strconv.ParseUint(v, 16, 32)
		if err != nil {
			http.Error(w, "bad crc", http.StatusBadRequest)
			return
		}
		expectedCRC = uint32(c)
	}

	// Fast path: stream straight from the GitHub relay (or its server-side
	// disk cache). On failure we return 502 instead of silently falling
	// through to DNS — the client can then prompt the user before
	// retrying with source=slow.
	if source == "fast" {
		if served := s.serveFromGitHubRelay(w, r, expectedSize, expectedCRC, q.Get("name"), q.Get("type")); served {
			return
		}
		http.Error(w, "fast relay unavailable", http.StatusBadGateway)
		return
	}

	ch64, err := strconv.ParseUint(q.Get("ch"), 10, 16)
	if err != nil {
		http.Error(w, "bad ch", http.StatusBadRequest)
		return
	}
	channel := uint16(ch64)
	if !protocol.IsMediaChannel(channel) {
		http.Error(w, "ch out of media range", http.StatusBadRequest)
		return
	}
	blk64, err := strconv.ParseUint(q.Get("blk"), 10, 16)
	if err != nil || blk64 == 0 {
		http.Error(w, "bad blk", http.StatusBadRequest)
		return
	}
	blockCount := uint16(blk64)

	s.mu.RLock()
	fetcher := s.fetcher
	s.mu.RUnlock()
	if fetcher == nil {
		http.Error(w, "fetcher not configured", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()

	// Disk-cache hit: serve directly without ever talking to DNS.
	if s.mediaCache != nil && expectedCRC != 0 && expectedSize > 0 {
		if body, mime, ok := s.mediaCache.Get(expectedSize, expectedCRC); ok {
			servedMime := sanitizeMime(q.Get("type"))
			if servedMime == "application/octet-stream" {
				if mime != "" {
					servedMime = sanitizeMime(mime)
				} else if sniffed := http.DetectContentType(body); sniffed != "" {
					servedMime = sanitizeMime(sniffed)
				}
			}
			w.Header().Set("Content-Type", servedMime)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("Cache-Control", "private, max-age=86400")
			w.Header().Set("X-Total-Blocks", strconv.Itoa(int(blockCount)))
			w.Header().Set("X-Cache", "HIT")
			if filename := sanitizeFilename(q.Get("name")); filename != "" {
				w.Header().Set("Content-Disposition", "inline; filename=\""+filename+"\"")
			}
			if _, err := w.Write(body); err != nil {
				s.addLog(fmt.Sprintf("media disk-cache write failed: %v", err))
			}
			return
		}
	}

	// Fetch block 0 synchronously: it carries the protocol header (CRC32,
	// version, compression). We need that before we can decompress and
	// before we can sniff Content-Type from the decompressed body.
	firstBlock, err := fetcher.FetchBlock(ctx, channel, 0)
	if err != nil {
		if ctx.Err() != nil {
			http.Error(w, "fetch cancelled", 499)
			return
		}
		http.Error(w, fmt.Sprintf("fetch media: %v", err), http.StatusBadGateway)
		return
	}
	if len(firstBlock) < protocol.MediaBlockHeaderLen {
		http.Error(w, "malformed block 0", http.StatusBadGateway)
		return
	}
	header, err := protocol.DecodeMediaBlockHeader(firstBlock[:protocol.MediaBlockHeaderLen])
	if err != nil {
		http.Error(w, "malformed block 0", http.StatusBadGateway)
		return
	}
	if expectedCRC != 0 && header.CRC32 != expectedCRC {
		http.Error(w, "content hash mismatch", http.StatusBadGateway)
		return
	}
	firstCompressed := firstBlock[protocol.MediaBlockHeaderLen:]

	// Register this download so /api/media/progress can report block
	// progress as the client polls. Block 0 is already fetched.
	progKey := mediaProgressKey(channel, blockCount, expectedCRC)
	prog := &mediaDLProgress{total: int32(blockCount), completed: 1}
	s.dlMu.Lock()
	s.dlProgress[progKey] = prog
	s.dlMu.Unlock()
	defer func() {
		s.dlMu.Lock()
		delete(s.dlProgress, progKey)
		s.dlMu.Unlock()
	}()

	// Pipe compressed bytes (block-0 payload + later blocks) into a
	// decompressor reader. Fed by a goroutine; consumed below for sniffing
	// and for streaming to the HTTP response.
	pipeR, pipeW := io.Pipe()
	go func() {
		var pipeErr error
		defer func() { pipeW.CloseWithError(pipeErr) }()
		if _, err := pipeW.Write(firstCompressed); err != nil {
			pipeErr = err
			return
		}
		if blockCount > 1 {
			progressCB := func(done, _ int) {
				// done counts blocks 1..N-1; add 1 for block 0 already fetched.
				atomic.StoreInt32(&prog.completed, int32(done+1))
			}
			pipeErr = fetcher.FetchMediaBlocksStream(ctx, channel, 1, blockCount-1, pipeW, progressCB)
		}
	}()

	body, err := client.DecompressMediaReader(pipeR, header.Compression)
	if err != nil {
		http.Error(w, fmt.Sprintf("decompress: %v", err), http.StatusBadGateway)
		return
	}
	defer body.Close()

	// Tee decompressed bytes into a buffer so we can persist them to the
	// disk cache after a successful response.
	var teeBuf *bytes.Buffer
	if s.mediaCache != nil && expectedCRC != 0 && expectedSize > 0 && expectedSize <= mediaCacheMaxFileExt {
		teeBuf = bytes.NewBuffer(make([]byte, 0, expectedSize))
	}

	// Sniff Content-Type from the first decompressed bytes before flushing
	// headers — once Content-Type goes out we can't change it.
	const sniffSize = 512
	sniff := make([]byte, sniffSize)
	n, err := io.ReadFull(body, sniff)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		http.Error(w, fmt.Sprintf("read media: %v", err), http.StatusBadGateway)
		return
	}
	sniff = sniff[:n]

	mime := sanitizeMime(q.Get("type"))
	if mime == "application/octet-stream" {
		if got := http.DetectContentType(sniff); got != "" {
			mime = sanitizeMime(got)
		}
	}
	filename := sanitizeFilename(q.Get("name"))

	w.Header().Set("Content-Type", mime)
	if expectedSize > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(expectedSize, 10))
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Total-Blocks", strconv.Itoa(int(blockCount)))
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Media-Compression", header.Compression.String())
	if filename != "" {
		w.Header().Set("Content-Disposition", "inline; filename=\""+filename+"\"")
	}
	flusher, _ := w.(http.Flusher)

	if teeBuf != nil {
		teeBuf.Write(sniff)
	}
	if _, err := w.Write(sniff); err != nil {
		s.addLog(fmt.Sprintf("media write head failed: %v", err))
		return
	}
	if flusher != nil {
		flusher.Flush()
	}

	dst := io.Writer(&flushAfterEachWriter{w: w, flusher: flusher})
	if teeBuf != nil {
		dst = io.MultiWriter(dst, teeBuf)
	}
	// Small buffer so the browser sees many small chunks instead of one big
	// one — the xhr onprogress event fires per chunk, which is what drives
	// the smooth K/N block counter on the client.
	buf := make([]byte, 2048)
	if _, err := io.CopyBuffer(dst, body, buf); err != nil {
		s.addLog(fmt.Sprintf("media stream failed: %v", err))
		return
	}

	if teeBuf == nil {
		s.addLog(fmt.Sprintf("media disk-cache skipped: size=%d crc=%x mediaCache=%v", expectedSize, expectedCRC, s.mediaCache != nil))
	} else if expectedSize > 0 && int64(teeBuf.Len()) != expectedSize {
		s.addLog(fmt.Sprintf("media disk-cache skipped: tee=%d expected=%d (truncated stream)", teeBuf.Len(), expectedSize))
	} else {
		if err := s.mediaCache.Put(int64(teeBuf.Len()), expectedCRC, teeBuf.Bytes(), mime); err != nil {
			s.addLog(fmt.Sprintf("media disk-cache put failed: %v", err))
		} else {
			s.addLog(fmt.Sprintf("media cached: %d bytes, crc=%08x, mime=%s", teeBuf.Len(), expectedCRC, mime))
		}
	}
}

type flushAfterEachWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (fw *flushAfterEachWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil && fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}

func (fw *flushAfterEachWriter) Flush() {
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
}

// sanitizeMime returns a "type/subtype" MIME string built from safe
// characters. HTML/SVG variants are rejected.
func sanitizeMime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "application/octet-stream"
	}
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	slash := strings.IndexByte(s, '/')
	if slash <= 0 || slash == len(s)-1 {
		return "application/octet-stream"
	}
	for _, r := range s {
		if r == '/' || r == '-' || r == '+' || r == '.' {
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return "application/octet-stream"
		}
	}
	switch strings.ToLower(s) {
	case "text/html", "application/xhtml+xml", "image/svg+xml":
		return "application/octet-stream"
	}
	return s
}

// sanitizeFilename strips path components and control characters.
func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	if s == "" || s == ".." {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7F || r == '"' || r == '\\' {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}
