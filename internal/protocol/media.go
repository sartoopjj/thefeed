package protocol

import (
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// Relay indices: each MediaMeta.Relays[N] flags whether the file is
// reachable via that relay. Order is fixed so the wire format is positional.
// Future relays append to this list; older clients ignore unknown trailing
// flags.
const (
	RelayDNS    = 0 // slow path — bytes assembled from DNS blocks
	RelayGitHub = 1 // fast path — bytes pulled from a GitHub repo
)

// MediaMeta describes a downloadable media blob attached to a feed message.
//
// Wire format (immediately after the media tag, before any caption):
//
//	[IMAGE]<size>:<f0>,<f1>,...:<dnsCh>:<dnsBlk>:<crc32hex>[:<filename>]
//
// where each <fN> is "1" or "0" indicating availability via relay N.
// <dnsCh>:<dnsBlk> are only meaningful when f0 (RelayDNS) is set.
type MediaMeta struct {
	Tag      string // e.g. MediaImage, MediaVideo, MediaFile
	Size     int64
	Relays   []bool // index = relay constant, value = availability
	Channel  uint16 // DNS channel (when Relays[RelayDNS])
	Blocks   uint16 // DNS block count (when Relays[RelayDNS])
	CRC32    uint32
	Filename string
}

// HasRelay reports whether the relay at idx is available. Out-of-range and
// nil-relay-list both return false.
func (m MediaMeta) HasRelay(idx int) bool {
	if idx < 0 || idx >= len(m.Relays) {
		return false
	}
	return m.Relays[idx]
}

// HasAnyRelay reports whether at least one relay can serve this file.
func (m MediaMeta) HasAnyRelay() bool {
	for _, on := range m.Relays {
		if on {
			return true
		}
	}
	return false
}

// String renders the metadata in the wire format documented above.
func (m MediaMeta) String() string {
	flags := encodeRelayFlags(m.Relays)
	if fn := SanitiseMediaFilename(m.Filename); fn != "" {
		return fmt.Sprintf("%s%d:%s:%d:%d:%08x:%s\n",
			m.Tag, m.Size, flags, m.Channel, m.Blocks, m.CRC32, fn)
	}
	return fmt.Sprintf("%s%d:%s:%d:%d:%08x\n",
		m.Tag, m.Size, flags, m.Channel, m.Blocks, m.CRC32)
}

// encodeRelayFlags serialises a relay list as "1,0,1". An empty list is
// "0,0" (DNS off, GitHub off) so older clients always see at least the two
// known relay slots.
func encodeRelayFlags(relays []bool) string {
	n := len(relays)
	if n < 2 {
		n = 2
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		on := i < len(relays) && relays[i]
		if on {
			parts[i] = "1"
		} else {
			parts[i] = "0"
		}
	}
	return strings.Join(parts, ",")
}

// parseRelayFlags decodes "1,0,1" into a relay slice sized to the input.
// Caller-side accessors guard against out-of-range reads, so future relays
// can be added without breaking older clients.
func parseRelayFlags(s string) ([]bool, bool) {
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ",")
	out := make([]bool, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p != "0" && p != "1" {
			return nil, false
		}
		out[i] = p == "1"
	}
	return out, true
}

// SanitiseMediaFilename returns a filename safe to embed in the wire
// metadata line. The output uses a restricted alphabet ([A-Za-z0-9._-]) so
// no path separator, colon, newline, or control char can ever survive.
// When the input is too long the base name is replaced with a short
// hash-derived id but the extension is preserved so other OSes still
// recognise the file type.
func SanitiseMediaFilename(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	cleaned := filterFilenameRunes(s)
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return ""
	}

	const maxBase = 24
	const maxExt = 8

	base, ext := splitFilenameExt(cleaned)
	if len(ext) > maxExt {
		ext = ext[:maxExt]
	}
	if len(base) > maxBase {
		h := fnv.New64a()
		_, _ = h.Write([]byte(cleaned))
		base = "media-" + hex.EncodeToString(h.Sum(nil))[:8]
	}
	if base == "" || base == "." {
		base = "media"
	}
	if ext != "" {
		return base + "." + ext
	}
	return base
}

func filterFilenameRunes(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func splitFilenameExt(s string) (base, ext string) {
	if i := strings.LastIndexByte(s, '.'); i >= 0 && i < len(s)-1 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// EncodeMediaText prepends the metadata line to an optional caption and
// returns the combined message text. A nil/empty caption yields just the tag
// + metadata + trailing newline-less string (the caption split is by the
// metadata line's trailing \n, so an empty caption simply has no extra body).
func EncodeMediaText(meta MediaMeta, caption string) string {
	header := meta.String()
	if caption == "" {
		// Drop the trailing newline so the message text doesn't end with a
		// blank line for caption-less media.
		return strings.TrimSuffix(header, "\n")
	}
	return header + caption
}

// ParseMediaText parses a message body that begins with a known media tag.
// Returns metadata + remaining caption. Legacy "[TAG]\ncaption" bodies parse
// with empty Relays (HasAnyRelay()==false). Unknown tags return ok=false.
func ParseMediaText(body string) (meta MediaMeta, caption string, ok bool) {
	tag, rest, found := splitKnownMediaTag(body)
	if !found {
		return MediaMeta{}, body, false
	}
	meta.Tag = tag

	// The bit between the tag and the first newline is the metadata payload.
	nl := strings.IndexByte(rest, '\n')
	var metaLine string
	if nl < 0 {
		metaLine = rest
		caption = ""
	} else {
		metaLine = rest[:nl]
		caption = rest[nl+1:]
	}
	metaLine = strings.TrimSpace(metaLine)

	if metaLine == "" {
		// Legacy [TAG]\ncaption — no per-file metadata. Treat as not-downloadable.
		return MediaMeta{Tag: tag}, caption, true
	}

	parts := strings.Split(metaLine, ":")
	if len(parts) < 5 {
		return MediaMeta{Tag: tag}, rest, true
	}

	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || size < 0 {
		return MediaMeta{Tag: tag}, rest, true
	}
	relays, ok := parseRelayFlags(parts[1])
	if !ok {
		return MediaMeta{Tag: tag}, rest, true
	}
	ch, err := strconv.ParseUint(parts[2], 10, 16)
	if err != nil {
		return MediaMeta{Tag: tag}, rest, true
	}
	blk, err := strconv.ParseUint(parts[3], 10, 16)
	if err != nil {
		return MediaMeta{Tag: tag}, rest, true
	}
	crc, err := strconv.ParseUint(parts[4], 16, 32)
	if err != nil {
		return MediaMeta{Tag: tag}, rest, true
	}
	// Reject DNS availability if the channel/block range is malformed —
	// other relays stay as-claimed.
	channel := uint16(ch)
	if len(relays) > RelayDNS && relays[RelayDNS] && (!IsMediaChannel(channel) || blk == 0) {
		relays[RelayDNS] = false
	}

	meta.Size = size
	meta.Relays = relays
	meta.Channel = channel
	meta.Blocks = uint16(blk)
	meta.CRC32 = uint32(crc)
	if len(parts) >= 6 {
		meta.Filename = SanitiseMediaFilename(parts[5])
	}
	return meta, caption, true
}

// knownMediaTags are the message text prefixes that mark a downloadable media
// attachment. Order matters only for prefix matching; longer/more-specific
// tags are not currently aliased so the order is alphabetical for clarity.
var knownMediaTags = []string{
	MediaAudio,
	MediaFile,
	MediaGIF,
	MediaImage,
	MediaSticker,
	MediaVideo,
}

// splitKnownMediaTag returns the matched tag and the remainder of the body
// when body starts with one of knownMediaTags.
func splitKnownMediaTag(body string) (tag, rest string, ok bool) {
	for _, t := range knownMediaTags {
		if strings.HasPrefix(body, t) {
			return t, body[len(t):], true
		}
	}
	return "", body, false
}
