package server

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// MediaCache stores binary media blobs (images, files, ...) keyed by an
// upstream-stable identifier (Telegram file_id, image URL, ...). Each entry
// occupies one channel number drawn from the [MediaChannelStart, MediaChannelEnd]
// range, plus a precomputed list of fixed-size raw blocks served via the
// regular DNS TXT path.
//
// The cache is safe for concurrent use. Hot-path operations (Store, GetBlock)
// are O(log n) at worst and typically O(1) with the help of two side maps.
type MediaCache struct {
	maxFileBytes int64
	ttl          time.Duration
	compression  protocol.MediaCompression
	dnsEnabled   bool // when false, RelayDNS stays unset on the wire

	logf func(format string, args ...interface{})

	gh *GitHubRelay

	mu          sync.RWMutex
	byKey       map[string]*mediaEntry // upstream key (file_id / URL) → entry
	byChannel   map[uint16]*mediaEntry // assigned channel → entry
	byHash      map[uint32]*mediaEntry // CRC32(content) → entry, for cross-key dedup
	nextChannel uint16                 // round-robin allocation hint

	// Counters surfaced via Stats(); written with atomics so reads from the
	// hourly reporter don't have to acquire mu.
	storeHits      uint64
	storeMisses    uint64
	storeRejected  uint64 // file too large
	queryCount     uint64 // total media block queries served
	evictionCount  uint64
	currentEntries int64 // live entry count
	currentBytes   int64 // sum of file sizes currently cached
}

type mediaEntry struct {
	channel   uint16
	cacheKey  string   // primary upstream id this entry was first stored under
	aliases   []string // additional keys (different upstream ids, same content)
	mimeType  string
	filename  string
	tag       string // protocol media tag (MediaImage, MediaFile, ...)
	size      int64
	crc32     uint32
	blocks    [][]byte
	expiresAt time.Time
	// inflight prevents the eviction sweep from reaping an entry that is
	// currently being downloaded by a goroutine that hasn't installed it yet.
	inflight bool
}

// MediaCacheConfig configures a new MediaCache.
type MediaCacheConfig struct {
	MaxFileBytes    int64
	TTL             time.Duration
	Compression     protocol.MediaCompression
	Logf            func(format string, args ...interface{})
	DNSRelayEnabled bool // controls Relays[RelayDNS] on the wire
}

// ErrTooLarge is returned by Store when content exceeds MaxFileBytes.
var ErrTooLarge = errors.New("media file exceeds configured max-size")

// ErrCacheFull is returned by Store when no media channel slot is available.
// In practice this requires either MediaChannelEnd-Start+1 simultaneously
// pinned files or a TTL too generous for the workload.
var ErrCacheFull = errors.New("no free media channel slot")

// NewMediaCache constructs a cache with the given configuration. A zero
// MaxFileBytes disables the size cap; a zero TTL means entries never expire
// (not recommended in production).
func NewMediaCache(cfg MediaCacheConfig) *MediaCache {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	return &MediaCache{
		maxFileBytes: cfg.MaxFileBytes,
		ttl:          cfg.TTL,
		compression:  cfg.Compression,
		dnsEnabled:   cfg.DNSRelayEnabled,
		logf:         logf,
		byKey:        make(map[string]*mediaEntry),
		byChannel:    make(map[uint16]*mediaEntry),
		byHash:       make(map[uint32]*mediaEntry),
		nextChannel:  protocol.MediaChannelStart,
	}
}

// Store inserts (or refreshes) a media blob into the cache and returns
// metadata that the caller can embed in a feed message.
//
// cacheKey is an upstream-stable identifier (e.g. Telegram file_id, image
// URL). When the same key is stored again, the existing entry's TTL is
// refreshed and the same channel/blocks are returned without copying the
// contents — callers should rely on this for the "fetch every 10 min"
// duplicate-handling case described in the design.
//
// tag is the protocol media tag (MediaImage, MediaFile, ...); mimeType and
// filename are optional and stored for the HTTP layer to surface to the
// client. content is the raw file bytes; the caller may pass a slice it
// continues to use after the call (Store copies into block-sized chunks).
func (c *MediaCache) Store(cacheKey, tag string, content []byte, mimeType, filename string) (protocol.MediaMeta, error) {
	return c.StoreWithOptions(cacheKey, tag, content, mimeType, filename, MediaCacheStoreOptions{})
}

// MediaCacheStoreOptions toggles relay paths for a single Store call.
// Zero value = both DNS channel and (if a relay is configured) GitHub
// upload. SkipGitHub keeps the DNS allocation but skips the upload —
// used when many small siblings share one bundled GitHub upload.
type MediaCacheStoreOptions struct {
	SkipGitHub bool
}

// StoreWithOptions is Store with selective relay control.
func (c *MediaCache) StoreWithOptions(cacheKey, tag string, content []byte, mimeType, filename string, opts MediaCacheStoreOptions) (protocol.MediaMeta, error) {
	if cacheKey == "" {
		return protocol.MediaMeta{}, errors.New("media: empty cache key")
	}
	if tag == "" {
		tag = protocol.MediaFile
	}
	size := int64(len(content))
	if max := c.MaxAcceptableBytes(); max > 0 && size > max {
		atomic.AddUint64(&c.storeRejected, 1)
		return protocol.MediaMeta{
			Tag:    tag,
			Size:   size,
			Relays: nil,
		}, ErrTooLarge
	}
	dnsFits := c.maxFileBytes == 0 || size <= c.maxFileBytes

	now := time.Now()
	hash := crc32.ChecksumIEEE(content)

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.byKey[cacheKey]; ok && existing.crc32 == hash {
		existing.expiresAt = c.expiry(now)
		atomic.AddUint64(&c.storeHits, 1)
		c.logf("media: refresh tag=%s key=%s ch=%d size=%d", tag, cacheKey, existing.channel, existing.size)
		if c.gh != nil {
			c.gh.Touch(existing.size, existing.crc32)
		}
		return c.metaForLocked(existing), nil
	}

	if existing, ok := c.byHash[hash]; ok {
		existing.expiresAt = c.expiry(now)
		if cacheKey != existing.cacheKey {
			alreadyAliased := false
			for _, a := range existing.aliases {
				if a == cacheKey {
					alreadyAliased = true
					break
				}
			}
			if !alreadyAliased {
				existing.aliases = append(existing.aliases, cacheKey)
			}
		}
		c.byKey[cacheKey] = existing
		atomic.AddUint64(&c.storeHits, 1)
		c.logf("media: dedup tag=%s key=%s ch=%d size=%d (hash match)", tag, cacheKey, existing.channel, existing.size)
		if c.gh != nil {
			c.gh.Touch(existing.size, existing.crc32)
		}
		return c.metaForLocked(existing), nil
	}

	// Either a new key, or the same key carries different bytes (a Telegram
	// edit, a re-upload). Allocate a fresh channel and replace.
	if existing, ok := c.byKey[cacheKey]; ok {
		c.dropEntryLocked(existing)
	}

	// Opportunistic sweep before we allocate. Without this, expired entries
	// that don't sit on the allocator's linear-scan path (i.e. ones below
	// nextChannel) accumulate until the periodic sweep runs. That breaks
	// the "TTL is the upper bound on how long a slot stays cached" promise
	// across burst-store workloads with small TTLs. The cost is O(n) over
	// active entries; n is capped by the media-channel range.
	c.sweepExpiredLocked(now)

	var (
		channel uint16
		blocks  [][]byte
	)
	if dnsFits {
		var err error
		channel, err = c.allocateChannelLocked(now)
		if err != nil {
			return protocol.MediaMeta{}, err
		}
		var encErr error
		blocks, encErr = splitMediaBlocks(hash, content, c.compression)
		if encErr != nil {
			return protocol.MediaMeta{}, encErr
		}
		if size > 0 {
			var compressedBody int
			for _, b := range blocks {
				compressedBody += len(b)
			}
			compressedBody -= protocol.MediaBlockHeaderLen
			if compressedBody < 0 {
				compressedBody = 0
			}
			var savedPct int
			if c.compression != protocol.MediaCompressionNone && size > 0 {
				savedPct = int((size - int64(compressedBody)) * 100 / size)
			}
			c.logf("media: compress=%s key=%s orig=%d body=%d saved=%d%%", c.compression, cacheKey, size, compressedBody, savedPct)
		}
	} else {
		c.logf("media: store key=%s size=%d too big for DNS — relay only", cacheKey, size)
	}
	entry := &mediaEntry{
		channel:   channel,
		cacheKey:  cacheKey,
		mimeType:  mimeType,
		filename:  protocol.SanitiseMediaFilename(filename),
		tag:       tag,
		size:      size,
		crc32:     hash,
		blocks:    blocks,
		expiresAt: c.expiry(now),
	}
	c.byKey[cacheKey] = entry
	if dnsFits {
		c.byChannel[channel] = entry
	}
	c.byHash[hash] = entry
	atomic.AddUint64(&c.storeMisses, 1)
	atomic.AddInt64(&c.currentEntries, 1)
	atomic.AddInt64(&c.currentBytes, size)
	c.logf("media: store tag=%s key=%s ch=%d size=%d blocks=%d", tag, cacheKey, channel, size, len(blocks))

	// Best-effort relay upload. Copy because the caller may reuse the
	// slice. Failures don't block the DNS path.
	if c.gh != nil && !opts.SkipGitHub {
		gh := c.gh
		body := append([]byte(nil), content...)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if err := gh.Upload(ctx, body); err != nil {
				c.logf("media: gh-relay upload failed: %v", err)
			}
		}()
	}

	meta := c.metaForLocked(entry)
	if opts.SkipGitHub && len(meta.Relays) > protocol.RelayGitHub {
		meta.Relays[protocol.RelayGitHub] = false
	}
	return meta, nil
}

// LookupByChannel returns the cached entry's transport metadata (mime,
// filename) for a serving channel. Returns ok=false if no entry is mapped.
// Used by the HTTP layer to pick a sensible Content-Type/Content-Disposition
// for clients that didn't provide one in the query string.
func (c *MediaCache) LookupByChannel(channel uint16) (mime, filename string, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, found := c.byChannel[channel]
	if !found {
		return "", "", false
	}
	return entry.mimeType, entry.filename, true
}

// Lookup returns the metadata for an entry by cache key, refreshing TTL on
// hit. Returns ok=false if not present.
func (c *MediaCache) Lookup(cacheKey string) (protocol.MediaMeta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byKey[cacheKey]
	if !ok {
		return protocol.MediaMeta{}, false
	}
	entry.expiresAt = c.expiry(time.Now())
	return c.metaForLocked(entry), true
}

// GetBlock returns one block of cached media for serving over DNS. Returns an
// error if the channel isn't a media channel, the entry has expired, or the
// block index is out of range. Increments the served-query counter.
func (c *MediaCache) GetBlock(channel, block uint16) ([]byte, error) {
	if !protocol.IsMediaChannel(channel) {
		return nil, fmt.Errorf("channel %d is outside media range", channel)
	}
	atomic.AddUint64(&c.queryCount, 1)

	c.mu.RLock()
	entry, ok := c.byChannel[channel]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("media channel %d not found", channel)
	}
	if int(block) >= len(entry.blocks) {
		return nil, fmt.Errorf("media block %d out of range (%d blocks)", block, len(entry.blocks))
	}
	// Reading a block extends the entry lifetime — clients in the middle of
	// downloading shouldn't have the cache rug pulled mid-transfer.
	c.mu.Lock()
	entry.expiresAt = c.expiry(time.Now())
	c.mu.Unlock()
	return entry.blocks[block], nil
}

// Sweep evicts entries whose TTL has elapsed. Returns the number evicted.
// Safe to call from a periodic goroutine.
func (c *MediaCache) Sweep() int {
	if c.ttl <= 0 {
		return 0
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.sweepExpiredLocked(now)
	if n > 0 {
		c.logf("media: sweep evicted=%d remaining=%d", n, len(c.byChannel))
	}
	return n
}

// sweepExpiredLocked is the shared implementation behind both the periodic
// Sweep and the opportunistic per-Store sweep. Caller must hold c.mu.
// It returns the number of entries evicted.
func (c *MediaCache) sweepExpiredLocked(now time.Time) int {
	if c.ttl <= 0 {
		return 0
	}
	var expired []*mediaEntry
	for _, entry := range c.byChannel {
		if entry.inflight {
			continue
		}
		if now.After(entry.expiresAt) {
			expired = append(expired, entry)
		}
	}
	for _, entry := range expired {
		c.dropEntryLocked(entry)
	}
	return len(expired)
}

// MediaCacheStats is a snapshot of cache counters.
type MediaCacheStats struct {
	Entries        int64  `json:"entries"`
	Bytes          int64  `json:"bytes"`
	Queries        uint64 `json:"queries"`
	StoreHits      uint64 `json:"storeHits"`
	StoreMisses    uint64 `json:"storeMisses"`
	StoreRejected  uint64 `json:"storeRejected"`
	Evictions      uint64 `json:"evictions"`
	MaxFileBytes   int64  `json:"maxFileBytes"`
	TTLSeconds     int64  `json:"ttlSeconds"`
}

// Stats returns a snapshot of cache counters. Lock-free for the per-counter
// fields; Entries and Bytes are also atomic.
func (c *MediaCache) Stats() MediaCacheStats {
	return MediaCacheStats{
		Entries:       atomic.LoadInt64(&c.currentEntries),
		Bytes:         atomic.LoadInt64(&c.currentBytes),
		Queries:       atomic.LoadUint64(&c.queryCount),
		StoreHits:     atomic.LoadUint64(&c.storeHits),
		StoreMisses:   atomic.LoadUint64(&c.storeMisses),
		StoreRejected: atomic.LoadUint64(&c.storeRejected),
		Evictions:     atomic.LoadUint64(&c.evictionCount),
		MaxFileBytes:  c.maxFileBytes,
		TTLSeconds:    int64(c.ttl / time.Second),
	}
}

// allocateChannelLocked finds a free channel in the media range, evicting
// expired entries on the way. Caller must hold c.mu.
func (c *MediaCache) allocateChannelLocked(now time.Time) (uint16, error) {
	rangeSize := int(protocol.MediaChannelEnd) - int(protocol.MediaChannelStart) + 1
	start := c.nextChannel
	if start < protocol.MediaChannelStart || start > protocol.MediaChannelEnd {
		start = protocol.MediaChannelStart
	}
	cur := start
	for i := 0; i < rangeSize; i++ {
		entry, taken := c.byChannel[cur]
		if !taken {
			c.advanceNextLocked(cur)
			return cur, nil
		}
		if !entry.inflight && c.ttl > 0 && now.After(entry.expiresAt) {
			c.dropEntryLocked(entry)
			c.advanceNextLocked(cur)
			return cur, nil
		}
		// Step to next slot, wrap when we hit the end of the range.
		if cur == protocol.MediaChannelEnd {
			cur = protocol.MediaChannelStart
		} else {
			cur++
		}
	}
	// Range fully occupied with non-expired entries — evict the oldest one as
	// a last resort, so the cache never hard-fails under steady-state
	// pressure with reasonable configs.
	var oldest *mediaEntry
	for _, entry := range c.byChannel {
		if entry.inflight {
			continue
		}
		if oldest == nil || entry.expiresAt.Before(oldest.expiresAt) {
			oldest = entry
		}
	}
	if oldest == nil {
		return 0, ErrCacheFull
	}
	freed := oldest.channel
	c.dropEntryLocked(oldest)
	c.advanceNextLocked(freed)
	return freed, nil
}

func (c *MediaCache) advanceNextLocked(used uint16) {
	if used == protocol.MediaChannelEnd {
		c.nextChannel = protocol.MediaChannelStart
	} else {
		c.nextChannel = used + 1
	}
}

func (c *MediaCache) dropEntryLocked(entry *mediaEntry) {
	delete(c.byChannel, entry.channel)
	delete(c.byKey, entry.cacheKey)
	for _, alias := range entry.aliases {
		// Only delete an alias if it still resolves to this entry; a later
		// store under the same key may have rebound it elsewhere.
		if c.byKey[alias] == entry {
			delete(c.byKey, alias)
		}
	}
	if c.byHash[entry.crc32] == entry {
		delete(c.byHash, entry.crc32)
	}
	atomic.AddInt64(&c.currentEntries, -1)
	atomic.AddInt64(&c.currentBytes, -entry.size)
	atomic.AddUint64(&c.evictionCount, 1)
}

func (c *MediaCache) expiry(now time.Time) time.Time {
	if c.ttl <= 0 {
		// "Never" — represented as far future so all comparisons act as expected.
		return time.Unix(1<<62, 0)
	}
	return now.Add(c.ttl)
}

func (c *MediaCache) metaForLocked(entry *mediaEntry) protocol.MediaMeta {
	// DNS bit only when DNS is enabled AND we actually computed blocks for
	// this entry. Files larger than the DNS cap have len(blocks)==0.
	dnsOK := c.dnsEnabled && len(entry.blocks) > 0
	// GitHub bit reflects "the relay would serve this file": relay enabled
	// and the file fits its cap. We don't require the upload to have
	// finished — small files in particular would otherwise miss the bit on
	// first render because the upload runs asynchronously. The web layer
	// retries transient 404s while the upload is still in flight.
	ghOK := false
	if c.gh != nil {
		ghMax := c.gh.MaxBytes()
		ghOK = ghMax == 0 || entry.size <= ghMax
	}
	relays := []bool{dnsOK, ghOK}
	meta := protocol.MediaMeta{
		Tag:      entry.tag,
		Size:     entry.size,
		Relays:   relays,
		CRC32:    entry.crc32,
		Filename: entry.filename,
	}
	if dnsOK {
		meta.Channel = entry.channel
		meta.Blocks = uint16(len(entry.blocks))
	}
	return meta
}

// SetGitHubRelay attaches the GitHub fast relay. Store calls (and Lookup
// hits) will then surface RelayGitHub when the relay has the bytes.
func (c *MediaCache) SetGitHubRelay(g *GitHubRelay) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gh = g
}

// TouchRelayEntries refreshes relay lastSeen for every cached file so
// files referenced by skipped-fetch cycles aren't pruned.
func (c *MediaCache) TouchRelayEntries() {
	if c == nil {
		return
	}
	c.mu.RLock()
	gh := c.gh
	if gh == nil {
		c.mu.RUnlock()
		return
	}
	pairs := make([][2]uint64, 0, len(c.byHash))
	for _, e := range c.byHash {
		pairs = append(pairs, [2]uint64{uint64(e.size), uint64(e.crc32)})
	}
	c.mu.RUnlock()
	for _, p := range pairs {
		gh.Touch(int64(p[0]), uint32(p[1]))
	}
}

// MaxAcceptableBytes returns the largest file size any enabled relay would
// accept. Callers use it as the "should we even fetch this?" gate so that
// files which fit GitHub but not DNS still get pulled. 0 means "no cap".
func (c *MediaCache) MaxAcceptableBytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	gh := c.gh
	c.mu.RUnlock()
	dns := c.maxFileBytes
	var ghMax int64
	if gh != nil {
		ghMax = gh.MaxBytes()
	}
	// 0 from any enabled relay means "no cap" — propagate.
	if (dns == 0 && c.dnsEnabled) || (gh != nil && ghMax == 0) {
		return 0
	}
	if !c.dnsEnabled {
		return ghMax
	}
	if gh == nil {
		return dns
	}
	if ghMax > dns {
		return ghMax
	}
	return dns
}

// splitMediaBlocks compresses the content (when compression != none),
// prepends the protocol media header, then splits the result into
// randomly-sized blocks. The CRC32 carried in the header is over the
// DECOMPRESSED bytes so the client can verify integrity after
// decompression. Uniform sizing is avoided to match the anti-DPI strategy
// used for feed-message blocks.
func splitMediaBlocks(crc32Hash uint32, content []byte, compression protocol.MediaCompression) ([][]byte, error) {
	body, err := compressMediaBytes(content, compression)
	if err != nil {
		return nil, err
	}
	header := protocol.EncodeMediaBlockHeader(protocol.MediaBlockHeader{
		CRC32:       crc32Hash,
		Version:     protocol.MediaHeaderVersion,
		Compression: compression,
	})
	full := make([]byte, 0, len(header)+len(body))
	full = append(full, header...)
	full = append(full, body...)
	return protocol.SplitIntoBlocks(full), nil
}

func compressMediaBytes(content []byte, compression protocol.MediaCompression) ([]byte, error) {
	switch compression {
	case protocol.MediaCompressionNone:
		return content, nil
	case protocol.MediaCompressionGzip:
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil, err
		}
		if _, err := zw.Write(content); err != nil {
			zw.Close()
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case protocol.MediaCompressionDeflate:
		var buf bytes.Buffer
		zw, err := flate.NewWriter(&buf, flate.BestCompression)
		if err != nil {
			return nil, err
		}
		if _, err := zw.Write(content); err != nil {
			zw.Close()
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("unsupported media compression: %d", compression)
}

// DecompressMediaBytes is the inverse of compressMediaBytes; exposed for
// the HTTP layer (which receives a stream of compressed bytes after the
// header is stripped) and tests.
func DecompressMediaBytes(r io.Reader, compression protocol.MediaCompression) (io.ReadCloser, error) {
	switch compression {
	case protocol.MediaCompressionNone:
		return io.NopCloser(r), nil
	case protocol.MediaCompressionGzip:
		return gzip.NewReader(r)
	case protocol.MediaCompressionDeflate:
		return flate.NewReader(r), nil
	}
	return nil, fmt.Errorf("unsupported media compression: %d", compression)
}
