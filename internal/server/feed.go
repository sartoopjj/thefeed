package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"hash/crc32"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Feed manages the block data for all channels.
type Feed struct {
	mu               sync.RWMutex
	marker           [protocol.MarkerSize]byte
	channels         []string
	displayNames     map[int]string
	blocks           map[int][][]byte
	lastIDs          map[int]uint32
	contentHashes    map[int]uint32
	chatTypes        map[int]protocol.ChatType
	canSend          map[int]bool
	metaBlocks       [][]byte // metadata for channel 0; includes extended header (see metadata_ext.go)
	versionBlocks    [][]byte // channel for latest server-known release version
	titlesBlocks     [][]byte // channel for per-channel display names
	updated          time.Time
	telegramLoggedIn bool
	nextFetch        uint32
	latestVersion    string

	// media holds binary blobs (images, files, ...) on a separate set of
	// channel numbers in the [MediaChannelStart, MediaChannelEnd] range. It
	// may be nil when media downloads are disabled — Feed.GetBlock then
	// rejects queries to media channels with a not-found error, mirroring
	// pre-feature behaviour.
	media *MediaCache

	// gitHubRelay (optional) lets clients fetch media bytes over plain
	// HTTPS from a GitHub repo. nil when disabled.
	gitHubRelay *GitHubRelay
	// relayInfoBlocks serves the relay-discovery channel
	// (RelayInfoChannel) — block 0 contains the GitHub "owner/repo"
	// string, or an empty payload if the relay is off.
	relayInfoBlocks [][]byte

	// ProfilePicsChannel serves the directory; the bundle bytes live
	// in one media-cache entry, with each entry also reachable on its
	// own DNS channel.
	profilePicsBlocks      [][]byte
	profilePicsBundle      protocol.ProfilePicsBundle
	profilePicsBundleBytes []byte // last-built bundle, for MergeProfilePics
	// Serialises MergeProfilePics so concurrent readers can't lose
	// each other's writes through the read-modify-write sequence.
	profilePicsMergeMu sync.Mutex

	// chatInfoBlocks serves the chat capability payload on
	// ChatInfoChannel. nil/empty means chat is disabled — the channel
	// answers not-found and clients negative-cache that.
	chatInfoBlocks [][]byte
	// chatAvailable sets the metadata "server has chat" flag bit.
	chatAvailable bool
	// mtprotoAvailable sets the metadata "server relays MTProto" flag bit, so
	// a client can mark the config without probing it.
	mtprotoAvailable bool
	// mtprotoInfoBlocks serves the tunnel capability payload on
	// MTProtoInfoChannel. Empty means no tunnel — not-found, which clients
	// negative-cache, exactly like chat.
	mtprotoInfoBlocks [][]byte

	// signKey is the server ed25519 key used to sign ExtraBlocks. nil
	// disables signing (old behaviour). extraBlocks holds, per channel,
	// the signed ExtraBlock served at block index == the channel's real
	// block count; old clients never request that index.
	signKey     ed25519.PrivateKey
	extraBlocks map[int][]byte
}

// NewFeed creates a new Feed with the given channel names.
func NewFeed(channels []string) *Feed {
	f := &Feed{
		channels:      channels,
		displayNames:  make(map[int]string),
		blocks:        make(map[int][][]byte),
		lastIDs:       make(map[int]uint32),
		contentHashes: make(map[int]uint32),
		chatTypes:     make(map[int]protocol.ChatType),
		canSend:       make(map[int]bool),
		extraBlocks:   make(map[int][]byte),
	}
	f.rotateMarker()
	f.rebuildMetaBlocks()
	f.rebuildVersionBlocks()
	f.rebuildTitlesBlocks()
	return f
}

func (f *Feed) rotateMarker() {
	rand.Read(f.marker[:])
}

// UpdateChannel replaces the messages for a channel, re-serializing into blocks.
func (f *Feed) UpdateChannel(channelNum int, msgs []protocol.Message) {
	data := protocol.SerializeMessages(msgs)
	compressed := protocol.CompressMessages(data)
	blocks := protocol.SplitIntoBlocks(compressed)

	var lastID uint32
	if len(msgs) > 0 {
		lastID = msgs[0].ID
	}
	contentHash := protocol.ContentHashOf(msgs)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.blocks[channelNum] = blocks
	f.lastIDs[channelNum] = lastID
	f.contentHashes[channelNum] = contentHash
	f.updated = time.Now()
	f.buildExtraBlockLocked(channelNum, blocks)
	f.rotateMarker()
	f.rebuildMetaBlocks()
}

// SetSigningKey enables ExtraBlock signing with the server key and builds
// the signed blocks for every channel already populated. Call once at
// startup, after NewFeed. nil leaves signing disabled.
func (f *Feed) SetSigningKey(priv ed25519.PrivateKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signKey = priv
	if priv == nil {
		return
	}
	f.buildExtraBlockLocked(protocol.MetadataChannel, f.metaBlocks)
	f.buildExtraBlockLocked(int(protocol.VersionChannel), f.versionBlocks)
	f.buildExtraBlockLocked(int(protocol.TitlesChannel), f.titlesBlocks)
	f.buildExtraBlockLocked(int(protocol.RelayInfoChannel), f.relayInfoBlocks)
	f.buildExtraBlockLocked(int(protocol.ProfilePicsChannel), f.profilePicsBlocks)
	f.buildExtraBlockLocked(int(protocol.ChatInfoChannel), f.chatInfoBlocks)
	for chNum, blocks := range f.blocks {
		f.buildExtraBlockLocked(chNum, blocks)
	}
}

// buildExtraBlockLocked (re)builds the signed ExtraBlock for a channel from
// the concatenation of its served blocks (the exact bytes the client
// reassembles, so digests match without any canonicalisation concern).
// No-op when signing is disabled or the channel has no blocks yet. Caller
// holds f.mu.
func (f *Feed) buildExtraBlockLocked(channel int, blocks [][]byte) {
	if f.signKey == nil || len(blocks) == 0 {
		return
	}
	var concat []byte
	for _, b := range blocks {
		concat = append(concat, b...)
	}
	eb, err := protocol.EncodeExtraBlock(f.signKey, uint16(channel), protocol.ContentDigest(concat), time.Now().Unix())
	if err != nil {
		log.Printf("[extrablock] channel %d: %v", channel, err)
		return
	}
	f.extraBlocks[channel] = eb
}

// extraBlockAt returns the signed ExtraBlock for a channel if the requested
// block index is exactly the channel's real block count. Caller holds f.mu.
func (f *Feed) extraBlockAt(channel, block, realCount int) ([]byte, bool) {
	if block != realCount {
		return nil, false
	}
	eb, ok := f.extraBlocks[channel]
	return eb, ok
}

// GetBlock returns the block data for a given channel and block number.
func (f *Feed) GetBlock(channel, block int) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if channel == protocol.MetadataChannel {
		return f.getMetadataBlock(block)
	}
	if channel == int(protocol.VersionChannel) {
		return f.getVersionBlock(block)
	}
	if channel == int(protocol.TitlesChannel) {
		return f.getTitlesBlock(block)
	}
	if channel == int(protocol.RelayInfoChannel) {
		return f.getRelayInfoBlock(block)
	}
	if channel == int(protocol.ProfilePicsChannel) {
		return f.getProfilePicsBlock(block)
	}
	if channel == int(protocol.ChatInfoChannel) {
		return f.getChatInfoBlock(block)
	}
	if channel == int(protocol.MTProtoInfoChannel) {
		return f.getMTProtoInfoBlock(block)
	}
	// Channel sits in the binary media range — delegate to MediaCache. We
	// drop the read lock first because MediaCache uses its own lock and we
	// don't want to hold f.mu across that path.
	if channel >= 0 && channel <= 0xFFFF && protocol.IsMediaChannel(uint16(channel)) {
		media := f.media
		if media == nil {
			return nil, fmt.Errorf("media channel %d not configured", channel)
		}
		return media.GetBlock(uint16(channel), uint16(block))
	}

	ch, ok := f.blocks[channel]
	if !ok {
		return nil, fmt.Errorf("channel %d not found", channel)
	}
	if block < 0 || block >= len(ch) {
		if eb, ok := f.extraBlockAt(channel, block, len(ch)); ok {
			return eb, nil
		}
		return nil, fmt.Errorf("block %d out of range (channel %d has %d blocks)", block, channel, len(ch))
	}
	return ch[block], nil
}

// SetMediaCache attaches a MediaCache to this Feed. Pass nil to disable
// media serving (the default for backward compat). Safe to call once at
// startup before any DNS query is served.
func (f *Feed) SetMediaCache(c *MediaCache) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.media = c
}

// MediaCache returns the configured MediaCache or nil.
func (f *Feed) MediaCache() *MediaCache {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.media
}

// SetGitHubRelay attaches the GitHub fast relay. Safe to call once at
// startup. nil disables.
func (f *Feed) SetGitHubRelay(r *GitHubRelay) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gitHubRelay = r
	f.rebuildRelayInfoBlocks()
}

// GitHubRelay returns the configured relay, or nil.
func (f *Feed) GitHubRelay() *GitHubRelay {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.gitHubRelay
}

// AfterFetchCycle: touch live media → flush pending → prune stale.
// Touch must come first so files referenced by skipped fetches don't age out.
func (f *Feed) AfterFetchCycle(ctx context.Context) {
	gh := f.GitHubRelay()
	if gh == nil {
		return
	}
	if mc := f.MediaCache(); mc != nil {
		mc.TouchRelayEntries()
	}
	if err := gh.Flush(ctx); err != nil {
		log.Printf("[gh-relay] flush after fetch: %v", err)
	}
	if ttl := gh.TTL(); ttl > 0 {
		cutoff := time.Now().Add(-ttl)
		if n, err := gh.PruneStale(ctx, cutoff); err != nil {
			log.Printf("[gh-relay] prune after fetch: %v", err)
		} else if n > 0 {
			log.Printf("[gh-relay] pruned %d stale file(s) after fetch", n)
		}
	}
}

// rebuildRelayInfoBlocks builds the discovery payload served on
// RelayInfoChannel. Format: "key=value\n" lines (UTF-8). Block 0 is
// prefixed with a uint16 total-block count so the client can fetch the
// rest in parallel.
//
// Keys are short (gh = github owner/repo) to keep packets small.
func (f *Feed) rebuildRelayInfoBlocks() {
	var payload []byte
	if r := f.gitHubRelay; r != nil {
		payload = []byte(fmt.Sprintf("gh=%s\n", r.Repo()))
	}
	blocks := protocol.SplitIntoBlocks(payload)
	if len(blocks) == 0 {
		blocks = [][]byte{nil}
	}
	prefix := []byte{byte(len(blocks) >> 8), byte(len(blocks))}
	blocks[0] = append(prefix, blocks[0]...)
	f.relayInfoBlocks = blocks
	f.buildExtraBlockLocked(int(protocol.RelayInfoChannel), f.relayInfoBlocks)
}

// SetChatAvailable sets the metadata flag advertising that this server has
// the messenger configured. Call before SetChatInfoPayload.
func (f *Feed) SetChatAvailable(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chatAvailable == v {
		return
	}
	f.chatAvailable = v
	f.rebuildMetaBlocks()
}

// SetMTProtoAvailable sets the metadata flag advertising that this server
// relays MTProto over DNS.
func (f *Feed) SetMTProtoAvailable(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mtprotoAvailable == v {
		return
	}
	f.mtprotoAvailable = v
	f.rebuildMetaBlocks()
}

// SetChatInfoPayload publishes (or clears, with empty payload) the chat
// capability data on ChatInfoChannel. Block 0 is prefixed with a uint16
// total-block count (same convention as titles); the channel is signed like
// any other when a signing key is set. Call after SetSigningKey.
func (f *Feed) SetChatInfoPayload(payload []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(payload) == 0 {
		f.chatInfoBlocks = nil
		delete(f.extraBlocks, int(protocol.ChatInfoChannel))
		return
	}
	blocks := protocol.SplitIntoBlocks(payload)
	prefix := []byte{byte(len(blocks) >> 8), byte(len(blocks))}
	blocks[0] = append(prefix, blocks[0]...)
	f.chatInfoBlocks = blocks
	f.buildExtraBlockLocked(int(protocol.ChatInfoChannel), f.chatInfoBlocks)
}

func (f *Feed) getChatInfoBlock(block int) ([]byte, error) {
	blocks := f.chatInfoBlocks
	if len(blocks) == 0 {
		// Chat disabled — clients treat not-found as "no chat" and
		// negative-cache it.
		return nil, fmt.Errorf("chat not enabled")
	}
	if block < 0 || block >= len(blocks) {
		if eb, ok := f.extraBlockAt(int(protocol.ChatInfoChannel), block, len(blocks)); ok {
			return eb, nil
		}
		return nil, fmt.Errorf("chat-info block %d out of range (%d blocks)", block, len(blocks))
	}
	return blocks[block], nil
}

// SetMTProtoInfoPayload publishes (or clears, with an empty payload) the
// tunnel capability data on MTProtoInfoChannel. Block 0 carries a uint16
// block count, mirroring the chat payload.
func (f *Feed) SetMTProtoInfoPayload(payload []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(payload) == 0 {
		f.mtprotoInfoBlocks = nil
		delete(f.extraBlocks, int(protocol.MTProtoInfoChannel))
		return
	}
	blocks := protocol.SplitIntoBlocks(payload)
	prefix := []byte{byte(len(blocks) >> 8), byte(len(blocks))}
	blocks[0] = append(prefix, blocks[0]...)
	f.mtprotoInfoBlocks = blocks
	f.buildExtraBlockLocked(int(protocol.MTProtoInfoChannel), f.mtprotoInfoBlocks)
}

func (f *Feed) getMTProtoInfoBlock(block int) ([]byte, error) {
	blocks := f.mtprotoInfoBlocks
	if len(blocks) == 0 {
		return nil, fmt.Errorf("mtproto not enabled")
	}
	if block < 0 || block >= len(blocks) {
		if eb, ok := f.extraBlockAt(int(protocol.MTProtoInfoChannel), block, len(blocks)); ok {
			return eb, nil
		}
		return nil, fmt.Errorf("mtproto-info block %d out of range (%d blocks)", block, len(blocks))
	}
	return blocks[block], nil
}

func (f *Feed) getVersionBlock(block int) ([]byte, error) {
	blocks := f.versionBlocks
	if len(blocks) == 0 {
		f.rebuildVersionBlocks()
		blocks = f.versionBlocks
	}
	if block < 0 || block >= len(blocks) {
		if eb, ok := f.extraBlockAt(int(protocol.VersionChannel), block, len(blocks)); ok {
			return eb, nil
		}
		return nil, fmt.Errorf("version block %d out of range (%d blocks)", block, len(blocks))
	}
	return blocks[block], nil
}

func (f *Feed) getMetadataBlock(block int) ([]byte, error) {
	blocks := f.metaBlocks
	if len(blocks) == 0 {
		f.rebuildMetaBlocks()
		blocks = f.metaBlocks
	}
	if block < 0 || block >= len(blocks) {
		if eb, ok := f.extraBlockAt(protocol.MetadataChannel, block, len(blocks)); ok {
			return eb, nil
		}
		return nil, fmt.Errorf("metadata block %d out of range (%d blocks)", block, len(blocks))
	}
	return blocks[block], nil
}

// rebuildMetaBlocks re-serializes the metadata and splits it into blocks
// for channel 0. The Marker + Timestamp fields are repurposed to embed a
// 7-byte header (magic + block_count + content_hash) so up-to-date clients
// can fetch all blocks in parallel and verify snapshot coherence; old
// clients still parse the same wire format and just ignore those fields.
// See protocol/metadata_ext.go for the layout. Must be called with f.mu
// held.
func (f *Feed) rebuildMetaBlocks() {
	meta := protocol.Metadata{
		// Marker + Timestamp are overwritten by EncodeMetadataExtended.
		NextFetch:        f.nextFetch,
		TelegramLoggedIn: f.telegramLoggedIn,
		ChatAvailable:    f.chatAvailable,
		MTProtoAvailable: f.mtprotoAvailable,
	}

	for i, name := range f.channels {
		chNum := i + 1
		blocks, ok := f.blocks[chNum]
		blockCount := uint16(0)
		if ok {
			blockCount = uint16(len(blocks))
		}
		meta.Channels = append(meta.Channels, protocol.ChannelInfo{
			Name:        name,
			Blocks:      blockCount,
			LastMsgID:   f.lastIDs[chNum],
			ContentHash: f.contentHashes[chNum],
			ChatType:    f.chatTypes[chNum],
			CanSend:     f.canSend[chNum],
		})
	}

	blocks, err := protocol.EncodeMetadataExtended(&meta)
	if err != nil {
		// Should never happen for sensible metadata; fall back to the
		// plain V0 encoding so we still serve *something* and old clients
		// see no change.
		log.Printf("[meta] EncodeMetadataExtended failed: %v — serving plain V0", err)
		meta.Marker = f.marker
		meta.Timestamp = uint32(time.Now().Unix())
		f.metaBlocks = protocol.SplitIntoBlocks(protocol.SerializeMetadata(&meta))
		f.buildExtraBlockLocked(protocol.MetadataChannel, f.metaBlocks)
		return
	}
	f.metaBlocks = blocks
	f.buildExtraBlockLocked(protocol.MetadataChannel, f.metaBlocks)
}

func (f *Feed) getTitlesBlock(block int) ([]byte, error) {
	blocks := f.titlesBlocks
	if len(blocks) == 0 {
		f.rebuildTitlesBlocks()
		blocks = f.titlesBlocks
	}
	if block < 0 || block >= len(blocks) {
		if eb, ok := f.extraBlockAt(int(protocol.TitlesChannel), block, len(blocks)); ok {
			return eb, nil
		}
		return nil, fmt.Errorf("titles block %d out of range (%d blocks)", block, len(blocks))
	}
	return blocks[block], nil
}

func (f *Feed) getRelayInfoBlock(block int) ([]byte, error) {
	blocks := f.relayInfoBlocks
	if len(blocks) == 0 {
		f.rebuildRelayInfoBlocks()
		blocks = f.relayInfoBlocks
	}
	if block < 0 || block >= len(blocks) {
		if eb, ok := f.extraBlockAt(int(protocol.RelayInfoChannel), block, len(blocks)); ok {
			return eb, nil
		}
		return nil, fmt.Errorf("relay-info block %d out of range (%d blocks)", block, len(blocks))
	}
	return blocks[block], nil
}

func (f *Feed) getProfilePicsBlock(block int) ([]byte, error) {
	blocks := f.profilePicsBlocks
	if len(blocks) == 0 {
		// Empty payload still has to be a single non-nil block so the
		// usual block-count prefix path stays consistent.
		f.rebuildProfilePicsBlocksLocked()
		blocks = f.profilePicsBlocks
	}
	if block < 0 || block >= len(blocks) {
		if eb, ok := f.extraBlockAt(int(protocol.ProfilePicsChannel), block, len(blocks)); ok {
			return eb, nil
		}
		return nil, fmt.Errorf("profile-pics block %d out of range (%d blocks)", block, len(blocks))
	}
	return blocks[block], nil
}

// rebuildProfilePicsBlocksLocked encodes the bundle and splits into
// blocks; block 0 is prefixed with the uint16 block count (same
// convention as titles). Caller holds f.mu.
func (f *Feed) rebuildProfilePicsBlocksLocked() {
	payload := protocol.EncodeProfilePicsBundle(f.profilePicsBundle)
	blocks := protocol.SplitIntoBlocks(payload)
	if len(blocks) == 0 {
		blocks = [][]byte{nil}
	}
	prefix := []byte{byte(len(blocks) >> 8), byte(len(blocks))}
	blocks[0] = append(prefix, blocks[0]...)
	f.profilePicsBlocks = blocks
	f.buildExtraBlockLocked(int(protocol.ProfilePicsChannel), f.profilePicsBlocks)
}

// SetProfilePics replaces the profile-pic bundle with the given
// username → image-bytes map. Other usernames currently in the bundle
// are dropped; use MergeProfilePics for additive behaviour. Empty
// values are skipped. Requires SetMediaCache. Returns the number of
// avatars in the resulting bundle.
func (f *Feed) SetProfilePics(pics map[string][]byte) int {
	return f.replaceProfilePicsBundle(pics)
}

// MergeProfilePics is SetProfilePics that retains the existing bundle's
// entries (re-extracted and re-verified) and overlays pics on top.
// Used by readers that only know a subset of channels (Telegram-only,
// X-only) so each one contributes without wiping the others.
//
// Serialised so two readers merging from the same prior state can't
// lose each other's writes.
func (f *Feed) MergeProfilePics(pics map[string][]byte) int {
	f.profilePicsMergeMu.Lock()
	defer f.profilePicsMergeMu.Unlock()

	merged := make(map[string][]byte, len(pics))

	f.mu.RLock()
	prev := f.profilePicsBundle
	prevBytes := f.profilePicsBundleBytes
	f.mu.RUnlock()
	if len(prev.Entries) > 0 && len(prevBytes) > 0 {
		for _, e := range prev.Entries {
			slice, err := protocol.VerifyEntry(prevBytes, e)
			if err != nil {
				log.Printf("[profile-pics] merge: skipping %s (%v)", e.Username, err)
				continue
			}
			cp := make([]byte, len(slice))
			copy(cp, slice)
			merged[e.Username] = cp
		}
	}
	for k, v := range pics {
		if k == "" {
			continue
		}
		if len(v) == 0 {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	return f.replaceProfilePicsBundle(merged)
}

// replaceProfilePicsBundle is the shared encode-and-store path. Each
// individual avatar gets its own DNS channel via SkipGitHub=true (so
// the DNS path can fetch one at a time without triggering N GitHub
// uploads), then the concatenated bundle is stored once with default
// opts to trigger the single GitHub upload that covers everything.
func (f *Feed) replaceProfilePicsBundle(pics map[string][]byte) int {
	f.mu.Lock()
	media := f.media
	f.mu.Unlock()
	if media == nil {
		return 0
	}

	type kv struct {
		name string
		b    []byte
	}
	ordered := make([]kv, 0, len(pics))
	for name, b := range pics {
		if name == "" || len(b) == 0 {
			continue
		}
		ordered = append(ordered, kv{name, b})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].name < ordered[j].name })

	if len(ordered) == 0 {
		f.mu.Lock()
		f.profilePicsBundle = protocol.ProfilePicsBundle{}
		f.profilePicsBundleBytes = nil
		f.rebuildProfilePicsBlocksLocked()
		f.mu.Unlock()
		return 0
	}

	// Build the bundle bytes + per-entry directory. Each entry gets its
	// own DNS channel via a SkipGitHub store so the DNS-path client can
	// fetch one avatar at a time without dragging the whole bundle.
	bundle := make([]byte, 0, 8192)
	entries := make([]protocol.ProfilePicEntry, 0, len(ordered))
	for _, e := range ordered {
		_, mimeTag := sniffProfilePicMime(e.b)
		offset := uint32(len(bundle))
		bundle = append(bundle, e.b...)

		entry := protocol.ProfilePicEntry{
			Username: e.name,
			Offset:   offset,
			Size:     uint32(len(e.b)),
			CRC:      crc32.ChecksumIEEE(e.b),
			MIME:     mimeTag,
		}
		// Per-pic DNS channel, no GitHub upload (the bundle covers GitHub).
		key := "profile-pic:" + e.name
		fname := e.name
		switch mimeTag {
		case protocol.ProfilePicMimePNG:
			fname += ".png"
		case protocol.ProfilePicMimeWebP:
			fname += ".webp"
		default:
			fname += ".jpg"
		}
		picMeta, err := media.StoreWithOptions(key, "[PROFILE]", e.b, mimeStringForTag(mimeTag), fname,
			MediaCacheStoreOptions{SkipGitHub: true})
		if err != nil {
			// No DNS channel for this entry; bundle path still works.
			log.Printf("[profile-pics] store individual %s: %v", e.name, err)
		} else {
			entry.DNSChannel = picMeta.Channel
			entry.DNSBlocks = picMeta.Blocks
		}
		entries = append(entries, entry)
	}

	// One bundle store → one GitHub upload covering every avatar.
	bundleMeta, err := media.Store("profile-pics-bundle", "[PROFILE-BUNDLE]",
		bundle, "application/octet-stream", "profile-pics.bin")
	if err != nil {
		log.Printf("[profile-pics] store bundle: %v", err)
		return 0
	}

	header := protocol.ProfilePicsBundleHeader{
		BundleSize: uint32(bundleMeta.Size),
		BundleCRC:  bundleMeta.CRC32,
		Relays:     append([]bool(nil), bundleMeta.Relays...),
	}

	f.mu.Lock()
	f.profilePicsBundle = protocol.ProfilePicsBundle{
		Header:  header,
		Entries: entries,
	}
	f.profilePicsBundleBytes = bundle
	f.rebuildProfilePicsBlocksLocked()
	f.mu.Unlock()
	return len(entries)
}

func mimeStringForTag(tag uint8) string {
	switch tag {
	case protocol.ProfilePicMimePNG:
		return "image/png"
	case protocol.ProfilePicMimeWebP:
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// ProfilePicsBundle returns a copy of the current directory.
func (f *Feed) ProfilePicsBundle() protocol.ProfilePicsBundle {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := protocol.ProfilePicsBundle{
		Header:  f.profilePicsBundle.Header,
		Entries: make([]protocol.ProfilePicEntry, len(f.profilePicsBundle.Entries)),
	}
	if len(f.profilePicsBundle.Header.Relays) > 0 {
		out.Header.Relays = append([]bool(nil), f.profilePicsBundle.Header.Relays...)
	}
	copy(out.Entries, f.profilePicsBundle.Entries)
	return out
}

// sniffProfilePicMime returns (rfc-mime, ProfilePicMime tag) by looking
// at the first few bytes. Falls back to JPEG for anything unrecognised.
func sniffProfilePicMime(b []byte) (string, uint8) {
	if len(b) >= 4 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G' {
		return "image/png", protocol.ProfilePicMimePNG
	}
	if len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return "image/webp", protocol.ProfilePicMimeWebP
	}
	return "image/jpeg", protocol.ProfilePicMimeJPEG
}

// rebuildTitlesBlocks re-serializes the display name map and splits it into blocks.
// Block 0 is prefixed with a uint16 total-block count so the client can fetch all
// remaining blocks in parallel after reading the first one.
// Must be called with f.mu held.
func (f *Feed) rebuildTitlesBlocks() {
	titles := make(map[string]string, len(f.channels))
	for i, name := range f.channels {
		chNum := i + 1
		if dn := f.displayNames[chNum]; dn != "" {
			titles[name] = dn
		}
	}
	blocks := protocol.SplitIntoBlocks(protocol.EncodeTitlesData(titles))
	if len(blocks) > 0 {
		prefix := []byte{byte(len(blocks) >> 8), byte(len(blocks))}
		blocks[0] = append(prefix, blocks[0]...)
	}
	f.titlesBlocks = blocks
	f.buildExtraBlockLocked(int(protocol.TitlesChannel), f.titlesBlocks)
}

func (f *Feed) rebuildVersionBlocks() {
	block, err := protocol.EncodeVersionData(f.latestVersion)
	if err != nil {
		block = make([]byte, protocol.MinBlockPayload)
	}
	f.versionBlocks = [][]byte{block}
	f.buildExtraBlockLocked(int(protocol.VersionChannel), f.versionBlocks)
}

// SetLatestVersion stores latest known release version for the dedicated version channel.
func (f *Feed) SetLatestVersion(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestVersion = v
	f.rebuildVersionBlocks()
}

// ChannelNames returns the configured channel names.
func (f *Feed) ChannelNames() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make([]string, len(f.channels))
	copy(result, f.channels)
	return result
}

// SetTelegramLoggedIn sets the flag indicating whether the server has a Telegram session.
func (f *Feed) SetTelegramLoggedIn(loggedIn bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.telegramLoggedIn = loggedIn
	f.rebuildMetaBlocks()
}

// SetNextFetch sets the unix timestamp of the next server-side fetch.
func (f *Feed) SetNextFetch(ts uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextFetch = ts
	f.rebuildMetaBlocks()
}

// SetChatInfo stores the chat type and send capability for a channel.
func (f *Feed) SetChatInfo(channelNum int, chatType protocol.ChatType, canSend bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatTypes[channelNum] = chatType
	f.canSend[channelNum] = canSend
	f.rebuildMetaBlocks()
}

// IsPrivateChannel returns true if the channel has chatType == ChatTypePrivate.
func (f *Feed) IsPrivateChannel(channelNum int) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.chatTypes[channelNum] == protocol.ChatTypePrivate
}

// SetChannels replaces the channel list and rebuilds metadata.
func (f *Feed) SetChannels(channels []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = channels
	f.rebuildMetaBlocks()
}

// SetChannelDisplayName stores a human-readable title for a channel (1-indexed).
// It never mutates the handle in f.channels, which remains the stable identifier.
func (f *Feed) SetChannelDisplayName(channelNum int, displayName string) {
	if displayName == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if channelNum < 1 || channelNum > len(f.channels) {
		return
	}
	if f.displayNames[channelNum] == displayName {
		return
	}
	f.displayNames[channelNum] = displayName
	f.rebuildTitlesBlocks()
}
