package protocol

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math/big"
	"unicode/utf8"
)

const (
	// MinBlockPayload is the minimum decrypted payload per DNS TXT block.
	MinBlockPayload = 200
	// MaxBlockPayload is the maximum decrypted payload per DNS TXT block.
	MaxBlockPayload = 600
	// DefaultBlockPayload is kept for compatibility; equals MaxBlockPayload.
	DefaultBlockPayload = MaxBlockPayload

	// DefaultMaxPadding is the default random padding added to responses to vary DNS response size.
	DefaultMaxPadding = 32

	// PadLengthSize is the 2-byte length prefix added before real data when padding is used.
	PadLengthSize = 2

	// MetadataChannel is the special channel number for server metadata.
	MetadataChannel = 0

	// MarkerSize is the random marker in metadata to verify data freshness.
	MarkerSize = 3

	// Query payload structure sizes.
	QueryPaddingSize = 4
	QueryChannelSize = 2
	QueryBlockSize   = 2
	QueryPayloadSize = QueryPaddingSize + QueryChannelSize + QueryBlockSize // 8

	// Message header sizes (in the serialized message stream).
	MsgIDSize          = 4
	MsgTimestampSize   = 4
	MsgLengthSize      = 2
	MsgHeaderSize      = MsgIDSize + MsgTimestampSize + MsgLengthSize // 10
	MsgContentHashSize = 4
)

// Media placeholder strings for non-text content.
const (
	MediaImage    = "[IMAGE]"
	MediaVideo    = "[VIDEO]"
	MediaFile     = "[FILE]"
	MediaAudio    = "[AUDIO]"
	MediaSticker  = "[STICKER]"
	MediaGIF      = "[GIF]"
	MediaPoll     = "[POLL]"
	MediaContact  = "[CONTACT]"
	MediaLocation = "[LOCATION]"
	MediaReply    = "[REPLY]"
)

// ChatType distinguishes channel types in metadata.
type ChatType uint8

const (
	ChatTypeChannel ChatType = 0 // public Telegram channel
	ChatTypePrivate ChatType = 1 // private chat / bot
	ChatTypeX       ChatType = 2 // public X (Twitter) account
)

// Metadata holds channel 0 data: server info + channel list.
type Metadata struct {
	Marker           [MarkerSize]byte
	Timestamp        uint32
	NextFetch        uint32 // unix timestamp of next server-side fetch (0 = unknown)
	TelegramLoggedIn bool   // true if server has an active Telegram session
	Channels         []ChannelInfo
}

// ChannelInfo describes a single feed channel.
type ChannelInfo struct {
	Name        string
	DisplayName string   // human-readable title; empty means fall back to Name
	Blocks      uint16
	LastMsgID   uint32
	ContentHash uint32   // CRC32 of serialized message data; changes on edits
	ChatType    ChatType // 0=Telegram channel, 1=private chat, 2=X account
	CanSend     bool     // true if server allows sending messages to this chat
}

// Message represents a single feed message in a channel.
type Message struct {
	ID        uint32
	Timestamp uint32
	Text      string
}

// ContentHashOf computes a CRC32 hash of serialized message data.
// This changes when any message is edited, even if IDs stay the same.
func ContentHashOf(msgs []Message) uint32 {
	data := SerializeMessages(msgs)
	return crc32.ChecksumIEEE(data)
}

// SerializeMetadata encodes metadata into bytes for channel 0 blocks.
// Format: marker(3) + timestamp(4) + nextFetch(4) + flags(1) + channelCount(2) + per-channel data
// Per-channel: nameLen(1) + name + blocks(2) + lastMsgID(4) + contentHash(4) + chatType(1) + flags(1)
func SerializeMetadata(m *Metadata) []byte {
	// 3 marker + 4 timestamp + 4 nextFetch + 1 flags + 2 channel count + per-channel data
	size := MarkerSize + 4 + 4 + 1 + 2
	for _, ch := range m.Channels {
		size += 1 + len(ch.Name) + 2 + 4 + 4 + 1 + 1
	}
	buf := make([]byte, size)
	off := 0

	copy(buf[off:], m.Marker[:])
	off += MarkerSize

	binary.BigEndian.PutUint32(buf[off:], m.Timestamp)
	off += 4

	binary.BigEndian.PutUint32(buf[off:], m.NextFetch)
	off += 4

	var flags byte
	if m.TelegramLoggedIn {
		flags |= 0x01
	}
	buf[off] = flags
	off++

	binary.BigEndian.PutUint16(buf[off:], uint16(len(m.Channels)))
	off += 2

	for _, ch := range m.Channels {
		nameBytes := []byte(ch.Name)
		if len(nameBytes) > 255 {
			nameBytes = nameBytes[:255]
		}
		buf[off] = byte(len(nameBytes))
		off++
		copy(buf[off:], nameBytes)
		off += len(nameBytes)
		binary.BigEndian.PutUint16(buf[off:], ch.Blocks)
		off += 2
		binary.BigEndian.PutUint32(buf[off:], ch.LastMsgID)
		off += 4
		binary.BigEndian.PutUint32(buf[off:], ch.ContentHash)
		off += 4
		buf[off] = byte(ch.ChatType)
		off++
		var chFlags byte
		if ch.CanSend {
			chFlags |= 0x01
		}
		buf[off] = chFlags
		off++
	}

	return buf
}

// ParseMetadata decodes metadata from concatenated channel 0 block data.
func ParseMetadata(data []byte) (*Metadata, error) {
	// Minimum: marker(3) + timestamp(4) + nextFetch(4) + flags(1) + count(2) = 14
	if len(data) < MarkerSize+4+4+1+2 {
		return nil, fmt.Errorf("metadata too short: %d bytes", len(data))
	}
	m := &Metadata{}
	off := 0

	copy(m.Marker[:], data[off:off+MarkerSize])
	off += MarkerSize

	m.Timestamp = binary.BigEndian.Uint32(data[off:])
	off += 4

	m.NextFetch = binary.BigEndian.Uint32(data[off:])
	off += 4

	flags := data[off]
	off++
	m.TelegramLoggedIn = flags&0x01 != 0

	count := binary.BigEndian.Uint16(data[off:])
	off += 2

	m.Channels = make([]ChannelInfo, 0, count)
	for i := 0; i < int(count); i++ {
		if off >= len(data) {
			return nil, fmt.Errorf("truncated metadata at channel %d", i)
		}
		nameLen := int(data[off])
		off++
		if off+nameLen > len(data) {
			return nil, fmt.Errorf("truncated channel name at %d", i)
		}
		name := string(data[off : off+nameLen])
		off += nameLen

		if off+12 > len(data) {
			return nil, fmt.Errorf("truncated channel info at %d", i)
		}
		blocks := binary.BigEndian.Uint16(data[off:])
		off += 2
		lastID := binary.BigEndian.Uint32(data[off:])
		off += 4
		contentHash := binary.BigEndian.Uint32(data[off:])
		off += 4
		chatType := ChatType(data[off])
		off++
		chFlags := data[off]
		off++

		m.Channels = append(m.Channels, ChannelInfo{
			Name:        name,
			Blocks:      blocks,
			LastMsgID:   lastID,
			ContentHash: contentHash,
			ChatType:    chatType,
			CanSend:     chFlags&0x01 != 0,
		})
	}

	return m, nil
}

// SerializeMessages encodes messages into a byte stream for data channel blocks.
func SerializeMessages(msgs []Message) []byte {
	size := 0
	for _, msg := range msgs {
		size += MsgHeaderSize + len(msg.Text)
	}
	buf := make([]byte, size)
	off := 0

	for _, msg := range msgs {
		textBytes := []byte(msg.Text)
		binary.BigEndian.PutUint32(buf[off:], msg.ID)
		off += MsgIDSize
		binary.BigEndian.PutUint32(buf[off:], msg.Timestamp)
		off += MsgTimestampSize
		binary.BigEndian.PutUint16(buf[off:], uint16(len(textBytes)))
		off += MsgLengthSize
		copy(buf[off:], textBytes)
		off += len(textBytes)
	}

	return buf
}

// ParseMessages decodes messages from concatenated data channel block data.
func ParseMessages(data []byte) ([]Message, error) {
	var msgs []Message
	off := 0

	for off < len(data) {
		if off+MsgHeaderSize > len(data) {
			break // incomplete message header, stop
		}
		id := binary.BigEndian.Uint32(data[off:])
		off += MsgIDSize
		ts := binary.BigEndian.Uint32(data[off:])
		off += MsgTimestampSize
		textLen := int(binary.BigEndian.Uint16(data[off:]))
		off += MsgLengthSize

		if off+textLen > len(data) {
			break // incomplete message text, stop
		}
		textBytes := data[off : off+textLen]
		off += textLen

		// Skip messages with invalid UTF-8 text — these are artifacts of
		// corrupt/decompression-failed data, not real messages.
		if !utf8.Valid(textBytes) {
			continue
		}
		text := string(textBytes)

		msgs = append(msgs, Message{
			ID:        id,
			Timestamp: ts,
			Text:      text,
		})
	}

	return msgs, nil
}

// SplitIntoBlocks splits data into blocks of randomly varying size in [MinBlockPayload, MaxBlockPayload].
// Random sizes make traffic analysis harder; the client just concatenates all blocks to reassemble.
func SplitIntoBlocks(data []byte) [][]byte {
	if len(data) == 0 {
		return [][]byte{{}} // channel 0 block 0 must always exist
	}
	var blocks [][]byte
	rem := data
	for len(rem) > 0 {
		size := randBlockSize()
		if size > len(rem) {
			size = len(rem)
		}
		block := make([]byte, size)
		copy(block, rem[:size])
		blocks = append(blocks, block)
		rem = rem[size:]
	}
	return blocks
}

func randBlockSize() int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(MaxBlockPayload-MinBlockPayload+1)))
	if err != nil {
		return (MinBlockPayload + MaxBlockPayload) / 2
	}
	return MinBlockPayload + int(n.Int64())
}

// EncodeVersionData encodes a version string into a single block padded to a
// random size in [MinBlockPayload, MaxBlockPayload], making it indistinguishable
// in size from regular content blocks for DPI resistance. Format:
//
//	[2 bytes: version byte length][version bytes][random padding]
func EncodeVersionData(version string) ([]byte, error) {
	raw := []byte(version)
	if len(raw) > MaxBlockPayload-2 {
		raw = raw[:MaxBlockPayload-2]
	}
	blockSize := randBlockSize()
	if blockSize < 2+len(raw) {
		blockSize = 2 + len(raw)
	}
	buf := make([]byte, blockSize)
	binary.BigEndian.PutUint16(buf, uint16(len(raw)))
	copy(buf[2:], raw)
	if _, err := rand.Read(buf[2+len(raw):]); err != nil {
		return nil, fmt.Errorf("version padding: %w", err)
	}
	return buf, nil
}

// DecodeVersionData extracts the version string from a block produced by EncodeVersionData.
func DecodeVersionData(block []byte) (string, error) {
	if len(block) < 2 {
		return "", fmt.Errorf("version block too short: %d bytes", len(block))
	}
	dataLen := int(binary.BigEndian.Uint16(block))
	if 2+dataLen > len(block) {
		return "", fmt.Errorf("version block truncated: need %d bytes, have %d", 2+dataLen, len(block))
	}
	return string(block[2 : 2+dataLen]), nil
}

const (
	// compressionNone means no compression applied (raw serialized messages).
	compressionNone byte = 0x00
	// compressionDeflate means data is deflate-compressed.
	compressionDeflate byte = 0x01
)

// CompressMessages compresses serialized message data using deflate.
// The output has a 1-byte header (compression type) followed by the payload.
// If compression doesn't reduce size, the raw data is stored instead.
func CompressMessages(data []byte) []byte {
	if len(data) == 0 {
		return append([]byte{compressionNone}, data...)
	}

	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return append([]byte{compressionNone}, data...)
	}
	w.Write(data)
	w.Close()

	compressed := buf.Bytes()
	if len(compressed) >= len(data) {
		// Compression didn't help — store raw
		return append([]byte{compressionNone}, data...)
	}

	return append([]byte{compressionDeflate}, compressed...)
}

// EncodeTitlesData encodes a name→title map into bytes for TitlesChannel blocks.
// Format: count(2) + [nameLen(1)+name+titleLen(1)+title]*count
func EncodeTitlesData(titles map[string]string) []byte {
	size := 2
	for name, title := range titles {
		n := name
		if len(n) > 255 {
			n = n[:255]
		}
		t := title
		if len([]byte(t)) > 255 {
			t = string([]byte(t)[:255])
		}
		size += 1 + len(n) + 1 + len([]byte(t))
	}
	buf := make([]byte, size)
	binary.BigEndian.PutUint16(buf, uint16(len(titles)))
	off := 2
	for name, title := range titles {
		nb := []byte(name)
		if len(nb) > 255 {
			nb = nb[:255]
		}
		tb := []byte(title)
		if len(tb) > 255 {
			tb = tb[:255]
		}
		buf[off] = byte(len(nb))
		off++
		copy(buf[off:], nb)
		off += len(nb)
		buf[off] = byte(len(tb))
		off++
		copy(buf[off:], tb)
		off += len(tb)
	}
	return buf
}

// DecodeTitlesData decodes a name→title map from bytes produced by EncodeTitlesData.
func DecodeTitlesData(data []byte) (map[string]string, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("titles data too short: %d bytes", len(data))
	}
	count := int(binary.BigEndian.Uint16(data))
	titles := make(map[string]string, count)
	off := 2
	for i := 0; i < count; i++ {
		if off >= len(data) {
			return nil, fmt.Errorf("truncated titles data at entry %d", i)
		}
		nameLen := int(data[off])
		off++
		if off+nameLen > len(data) {
			return nil, fmt.Errorf("truncated title name at entry %d", i)
		}
		name := string(data[off : off+nameLen])
		off += nameLen
		if off >= len(data) {
			return nil, fmt.Errorf("truncated titles data at title %d", i)
		}
		titleLen := int(data[off])
		off++
		if off+titleLen > len(data) {
			return nil, fmt.Errorf("truncated title value at entry %d", i)
		}
		title := string(data[off : off+titleLen])
		off += titleLen
		titles[name] = title
	}
	return titles, nil
}

// DecompressMessages decompresses data produced by CompressMessages.
// Reads the 1-byte header to determine the compression type.
func DecompressMessages(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty compressed data")
	}

	switch data[0] {
	case compressionNone:
		return data[1:], nil
	case compressionDeflate:
		r := flate.NewReader(bytes.NewReader(data[1:]))
		defer r.Close()
		out, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("deflate decompress: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown compression type: 0x%02x", data[0])
	}
}
