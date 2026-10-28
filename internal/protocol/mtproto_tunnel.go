package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MTProto-over-DNS tunnel: a byte stream between the client and a Telegram DC
// that the server dials on its behalf. The server only moves bytes — the
// MTProto session is negotiated client↔Telegram, so the relay can read nothing.
//
// Cells reuse the chat cell encoding (selector ‖ counter ‖ sealed payload, see
// chatwire.go): same masking, same length jitter, same per-session key. Only
// the op space and the sub-domain differ, so a tunnel op can never be mistaken
// for a chat op on an unknown selector.
//
// The session selector already identifies one client stream, so cells carry no
// tunnel id — one tunnel per session. That keeps the header inside the
// SMALLEST cell budget (compact = 8 plaintext bytes), which is the budget the
// client's scorer falls back to on a hostile network. A header that only fit
// the default budget would make the tunnel unusable exactly where it matters.
const (
	// MTPOpOpen asks the server to dial a DC. Fields: dc(1).
	// Reply: status(1) ‖ chunk(1) — the stream chunk size the server agreed to.
	MTPOpOpen = 1
	// MTPOpData carries upstream bytes at stream offset upSeq, acknowledging
	// downstream bytes below downAck.
	// Fields: upSeq(2) ‖ downAck(2) ‖ chunk. Reply: the data reply below.
	MTPOpData = 2
	// MTPOpPoll carries no stream bytes, only the read pointer — used when the
	// client has nothing to send but wants downstream bytes.
	// Fields: downAck(2). Reply: the data reply.
	MTPOpPoll = 3
	// MTPOpClose tears the tunnel down. No fields. Reply: status(1).
	MTPOpClose = 4
	// MTPOpDataEnd is MTPOpData whose chunk COMPLETES a message. The relay
	// buffers plain Data chunks and writes the whole message to the far end
	// when this arrives, so Telegram sees one complete message instead of a
	// trickle. Without it a slow link cannot finish Telegram's handshake at
	// all: it closes the connection before the first message is through.
	// Fields: upSeq(2) ‖ downAck(2) ‖ chunk. Reply: the data reply.
	MTPOpDataEnd = 5
)

// Tunnel reply status codes.
const (
	MTPStatusOK = 0
	// MTPStatusNoTunnel means the tunnel expired or never existed; the client
	// must re-open and restart the MTProto session.
	MTPStatusNoTunnel = 1
	// MTPStatusUpstreamClosed means the DC hung up. Downstream bytes still
	// buffered are delivered before this is reported.
	MTPStatusUpstreamClosed = 2
	// MTPStatusBusy means the server is at capacity.
	MTPStatusBusy = 3
	// MTPStatusDisabled means the tunnel is configured off on this server.
	MTPStatusDisabled = 4
	// MTPStatusBadRequest covers malformed fields and out-of-window sequences.
	MTPStatusBadRequest = 5
	// MTPStatusBadDC means the requested DC is not a known Telegram DC. The
	// server dials nothing else, so it can never be used as an open proxy.
	MTPStatusBadDC = 6
)

const (
	// MTPSeqSize is the width of a stream sequence number.
	MTPSeqSize = 2
	// MTPDataHeader is op(1) ‖ upSeq(2) ‖ downAck(2).
	MTPDataHeader = 1 + 2*MTPSeqSize
	// MTPPollHeader is op(1) ‖ downAck(2).
	MTPPollHeader = 1 + MTPSeqSize
	// MTPReplyHeader is status(1) ‖ upAck(2) ‖ downSeq(2) ‖ downLeft(2).
	MTPReplyHeader = 1 + 3*MTPSeqSize

	// MTPMinChunk is the smallest stream chunk worth sending. Below this the
	// header dominates so badly that the tunnel cannot make progress, and the
	// client reports the server as unusable instead of spinning.
	MTPMinChunk = 3
)

// Sequence numbers are BYTE offsets into the stream, mod 2^16. A retransmitted
// cell carries the offset it always carried, so replaying it is idempotent and
// the server needs no per-cell dedup table.
//
// Both sides advance well under the 32 KiB half-window between acks (upstream
// sends at most one cell before waiting; downstream carries at most one DNS
// response), so comparisons use the standard half-window rule below rather than
// absolute ordering.

// MTPSeqLess reports whether a precedes b in the sliding window.
func MTPSeqLess(a, b uint16) bool { return int16(a-b) < 0 }

// MTPSeqDelta returns b-a as a signed distance within the window.
func MTPSeqDelta(a, b uint16) int { return int(int16(b - a)) }

var errMTPShort = errors.New("mtproto tunnel: payload too short")

// MTPCellPad returns the seal padding target for a tunnel cell.
//
// Chat ops have fixed-size fields, so SealChatCellPayloadN's zero padding is
// harmless: the reader ignores the tail. A tunnel DATA cell instead ENDS in a
// variable-length slice of the byte stream, so its length is the framing —
// padding it would append zero bytes to the user's stream and the relay would
// write them to Telegram. DATA is therefore sealed at its exact length, while
// control ops still pad to a uniform size so they don't stand out by length.
func MTPCellPad(plaintext []byte, budget, jitter int) int {
	if len(plaintext) > 0 && (plaintext[0] == MTPOpData || plaintext[0] == MTPOpDataEnd) {
		return len(plaintext)
	}
	return budget + jitter
}

// MTPChunkSize is how many stream bytes fit in one uplink cell at the given
// per-cell plaintext budget. Returns 0 when the budget cannot carry a useful
// chunk; callers must treat that as "this budget cannot run the tunnel".
func MTPChunkSize(budget int) int {
	n := budget - MTPDataHeader
	if n < MTPMinChunk {
		return 0
	}
	return n
}

// BuildMTPOpen builds an OPEN request.
func BuildMTPOpen(dc uint8) []byte { return []byte{MTPOpOpen, dc} }

// ParseMTPOpen parses an OPEN request.
func ParseMTPOpen(pt []byte) (dc uint8, err error) {
	if len(pt) < 2 || pt[0] != MTPOpOpen {
		return 0, errMTPShort
	}
	return pt[1], nil
}

// BuildMTPOpenReply builds an OPEN reply. chunk is the agreed stream chunk size.
func BuildMTPOpenReply(status, chunk byte) []byte { return []byte{status, chunk} }

// ParseMTPOpenReply parses an OPEN reply. A refusal carries no chunk size.
func ParseMTPOpenReply(b []byte) (status, chunk byte, err error) {
	if len(b) < 1 {
		return 0, 0, errMTPShort
	}
	if b[0] != MTPStatusOK {
		return b[0], 0, nil
	}
	if len(b) < 2 {
		return 0, 0, errMTPShort
	}
	return b[0], b[1], nil
}

// BuildMTPDataEnd is BuildMTPData for a chunk that completes a message.
func BuildMTPDataEnd(upSeq, downAck uint16, chunk []byte) []byte {
	b := BuildMTPData(upSeq, downAck, chunk)
	b[0] = MTPOpDataEnd
	return b
}

// BuildMTPData builds a DATA request: stream bytes at upSeq, acknowledging
// downstream chunks below downAck.
func BuildMTPData(upSeq, downAck uint16, chunk []byte) []byte {
	b := make([]byte, MTPDataHeader+len(chunk))
	b[0] = MTPOpData
	binary.BigEndian.PutUint16(b[1:], upSeq)
	binary.BigEndian.PutUint16(b[3:], downAck)
	copy(b[MTPDataHeader:], chunk)
	return b
}

// BuildMTPPoll builds a POLL request: no stream bytes, just the read pointer.
func BuildMTPPoll(downAck uint16) []byte {
	b := make([]byte, MTPPollHeader)
	b[0] = MTPOpPoll
	binary.BigEndian.PutUint16(b[1:], downAck)
	return b
}

// MTPRequest is a parsed DATA or POLL request.
type MTPRequest struct {
	Op      byte
	UpSeq   uint16 // DATA only
	DownAck uint16
	Chunk   []byte // DATA only
}

// ParseMTPRequest parses a DATA or POLL request.
func ParseMTPRequest(pt []byte) (*MTPRequest, error) {
	if len(pt) < 1 {
		return nil, errMTPShort
	}
	switch pt[0] {
	case MTPOpPoll:
		if len(pt) < MTPPollHeader {
			return nil, errMTPShort
		}
		return &MTPRequest{Op: MTPOpPoll, DownAck: binary.BigEndian.Uint16(pt[1:])}, nil
	case MTPOpData, MTPOpDataEnd:
		if len(pt) < MTPDataHeader {
			return nil, errMTPShort
		}
		return &MTPRequest{
			Op:      pt[0],
			UpSeq:   binary.BigEndian.Uint16(pt[1:]),
			DownAck: binary.BigEndian.Uint16(pt[3:]),
			Chunk:   pt[MTPDataHeader:],
		}, nil
	}
	return nil, errMTPShort
}

// BuildMTPClose builds a CLOSE request.
func BuildMTPClose() []byte { return []byte{MTPOpClose} }

// MTPReply is the reply to DATA and POLL: how much upstream the server has
// taken, which downstream chunk the returned bytes start at, and those bytes.
type MTPReply struct {
	Status  byte
	UpAck   uint16 // next upstream stream offset the server expects
	DownSeq uint16 // stream offset of Down[0]
	// DownLeft is how much more the server already holds for this client,
	// beyond what rode back in this reply. The client cannot know what
	// Telegram will send, but the server knows what has arrived -- enough to
	// say "still receiving" instead of showing an unexplained wait.
	DownLeft uint16
	Down     []byte
}

// BuildMTPReply builds a DATA/POLL reply.
func BuildMTPReply(r MTPReply) []byte {
	b := make([]byte, MTPReplyHeader+len(r.Down))
	b[0] = r.Status
	binary.BigEndian.PutUint16(b[1:], r.UpAck)
	binary.BigEndian.PutUint16(b[3:], r.DownSeq)
	binary.BigEndian.PutUint16(b[5:], r.DownLeft)
	copy(b[MTPReplyHeader:], r.Down)
	return b
}

// ParseMTPReply parses a DATA/POLL reply. A short non-OK reply (the server
// rejecting before it had sequence numbers) parses as status-only, so the
// client reports the real reason instead of a decode error.
func ParseMTPReply(b []byte) (*MTPReply, error) {
	if len(b) < 1 {
		return nil, errMTPShort
	}
	if len(b) < MTPReplyHeader {
		if b[0] == MTPStatusOK {
			return nil, errMTPShort
		}
		return &MTPReply{Status: b[0]}, nil
	}
	return &MTPReply{
		Status:   b[0],
		UpAck:    binary.BigEndian.Uint16(b[1:]),
		DownSeq:  binary.BigEndian.Uint16(b[3:]),
		DownLeft: binary.BigEndian.Uint16(b[5:]),
		Down:     b[MTPReplyHeader:],
	}, nil
}

// MTPInfo is the signed capability payload served on MTProtoInfoChannel. The
// client reads it to decide whether a config can carry the bridge at all, and
// which sub-domains to send tunnel cells to — the same shape ChatInfo uses, so
// a keyless client can be told "this server has it, re-import with the key"
// instead of failing silently.
type MTPInfo struct {
	MinVersion uint8
	MaxVersion uint8
	Enabled    bool
	Domains    []string
	// Chunk is the stream bytes per cell the server offers at the widest
	// budget; the client still negotiates the real value at OPEN.
	Chunk uint8
	// EkPub is the server's x25519 handshake key. Carried here as well as in
	// ChatInfo so the bridge works on a server with the messenger switched off.
	EkPub []byte
	// TestDCs says the relay dials Telegram's TEST data centres. The client
	// MUST match: a production client against a test DC fails deep in the
	// handshake with "wrong DC", which is impossible to diagnose from the UI.
	TestDCs bool
}

// MTProtoVersion is the tunnel wire version this build speaks.
const MTProtoVersion = 1

// MTProtoInfoChannel serves the signed capability payload (MTPInfo). An absent
// or empty channel means the bridge is off, and the client negative-caches
// that — exactly how ChatInfoChannel behaves.
const MTProtoInfoChannel uint16 = 0xFFF4

// MTPInfo TLV types.
const (
	mtpInfoTLVEnd     = 0x00
	mtpInfoTLVVersion = 0x01
	mtpInfoTLVEnabled = 0x02
	mtpInfoTLVDomains = 0x03
	mtpInfoTLVChunk   = 0x04
	mtpInfoTLVEkPub   = 0x05
	mtpInfoTLVTestDCs = 0x06
)

// EncodeMTPInfo serialises the capability payload.
func EncodeMTPInfo(info MTPInfo) []byte {
	buf := make([]byte, 0, 64)
	buf = appendExtraTLV(buf, mtpInfoTLVVersion, []byte{info.MinVersion, info.MaxVersion})
	enabled := byte(0)
	if info.Enabled {
		enabled = 1
	}
	buf = appendExtraTLV(buf, mtpInfoTLVEnabled, []byte{enabled})
	if len(info.Domains) > 0 {
		buf = appendExtraTLV(buf, mtpInfoTLVDomains, []byte(strings.Join(info.Domains, ",")))
	}
	buf = appendExtraTLV(buf, mtpInfoTLVChunk, []byte{info.Chunk})
	// Only sent when true, so the field costs nothing on a normal server.
	if info.TestDCs {
		buf = appendExtraTLV(buf, mtpInfoTLVTestDCs, []byte{1})
	}
	if len(info.EkPub) == X25519KeySize {
		buf = appendExtraTLV(buf, mtpInfoTLVEkPub, info.EkPub)
	}
	return append(buf, mtpInfoTLVEnd)
}

// ParseMTPInfo decodes the capability payload. Unknown TLVs are skipped so an
// older client keeps working against a newer server.
func ParseMTPInfo(data []byte) (*MTPInfo, error) {
	info := &MTPInfo{}
	off := 0
	for off < len(data) {
		typ := data[off]
		off++
		if typ == mtpInfoTLVEnd {
			break
		}
		// appendExtraTLV writes a 2-byte big-endian length; read it the same way.
		if off+2 > len(data) {
			return nil, errMTPShort
		}
		n := int(binary.BigEndian.Uint16(data[off:]))
		off += 2
		if off+n > len(data) {
			return nil, errMTPShort
		}
		val := data[off : off+n]
		off += n
		switch typ {
		case mtpInfoTLVVersion:
			if n >= 2 {
				info.MinVersion, info.MaxVersion = val[0], val[1]
			}
		case mtpInfoTLVEnabled:
			if n >= 1 {
				info.Enabled = val[0] == 1
			}
		case mtpInfoTLVDomains:
			for _, d := range strings.Split(string(val), ",") {
				if d = strings.TrimSpace(d); d != "" {
					info.Domains = append(info.Domains, d)
				}
			}
		case mtpInfoTLVChunk:
			if n >= 1 {
				info.Chunk = val[0]
			}
		case mtpInfoTLVEkPub:
			if n == X25519KeySize {
				info.EkPub = append([]byte(nil), val...)
			}
		case mtpInfoTLVTestDCs:
			info.TestDCs = n == 1 && val[0] == 1
		}
	}
	return info, nil
}

// Usable reports whether this build can talk to the advertised server.
func (i *MTPInfo) Usable() bool {
	return i != nil && i.Enabled && len(i.Domains) > 0 &&
		len(i.EkPub) == X25519KeySize &&
		i.MinVersion <= MTProtoVersion && MTProtoVersion <= i.MaxVersion
}

// Handshake. A tunnel cell is sealed under a per-connection key, so a selector
// alone is not enough to drive someone else's tunnel: another holder of the
// same config could otherwise guess a live 24-bit selector and break a
// stranger's MTProto session.
//
// The client's ephemeral public key is 32 bytes and a cell carries fewer, so
// the handshake is fragmented. These fragments are the one part of the
// exchange that cannot be sealed — the key is what they are establishing — so
// they carry NO framing bytes at all:
//
//   - the fragment index rides in the cell counter, which EncodeChatCell masks
//   - every fragment is exactly ChatCellPayloadSize, so lengths never vary
//   - the final fragment's tail is RANDOM-padded, never zero-padded
//   - the fragment count is derived from the payload size, not transmitted
//
// The result is a query name of uniformly random-looking bytes, shaped exactly
// like a messenger handshake cell. A header byte here — even a version nibble —
// would sit at a fixed offset in the clear and let an observer fingerprint the
// protocol, which is why the wire version is bound into the session key
// (MTPSessionKey) instead: a mismatched version derives a different key, so the
// sealed reply simply fails to open. Fail-closed, and invisible from outside.

// MTPHelloFragments is how many cells an ephemeral key takes at a given cell
// payload size. Both sides compute it; it is never sent.
func MTPHelloFragments(payloadSize int) int {
	if payloadSize <= 0 {
		return 0
	}
	return (X25519KeySize + payloadSize - 1) / payloadSize
}

// MTPMaxHelloFragments bounds server-side handshake state.
const MTPMaxHelloFragments = 8

// SplitMTPHello fragments an ephemeral public key into fixed-size cell
// payloads, random-padding the final tail so no zero run shows in the query
// name. rnd supplies the padding (crypto/rand in production).
func SplitMTPHello(ephPub []byte, payloadSize int, rnd io.Reader) ([][]byte, error) {
	if len(ephPub) != X25519KeySize {
		return nil, fmt.Errorf("mtproto tunnel: ephemeral key is %d bytes, want %d", len(ephPub), X25519KeySize)
	}
	total := MTPHelloFragments(payloadSize)
	if total == 0 || total > MTPMaxHelloFragments {
		return nil, errors.New("mtproto tunnel: cell payload cannot carry a handshake")
	}
	out := make([][]byte, 0, total)
	for i := 0; i < total; i++ {
		chunk := make([]byte, payloadSize)
		n := copy(chunk, ephPub[i*payloadSize:])
		if n < payloadSize {
			if _, err := io.ReadFull(rnd, chunk[n:]); err != nil {
				return nil, err
			}
		}
		out = append(out, chunk)
	}
	return out, nil
}

// JoinMTPHello reassembles fragments collected by index. It returns false until
// every fragment has arrived.
func JoinMTPHello(frags map[uint32][]byte, payloadSize int) ([]byte, bool) {
	total := MTPHelloFragments(payloadSize)
	if total == 0 || len(frags) != total {
		return nil, false
	}
	out := make([]byte, 0, total*payloadSize)
	for i := 0; i < total; i++ {
		f, ok := frags[uint32(i)]
		if !ok || len(f) != payloadSize {
			return nil, false
		}
		out = append(out, f...)
	}
	return out[:X25519KeySize], true // drop the random tail
}

// MTPHelloAck is the reply to the final HELLO fragment, sealed under the newly
// derived session key: opening it proves the server did the same ECDH.
// Layout: status(1) ‖ ref(ChatSelectorSize) ‖ chunk(1).
const MTPHelloAckSize = 1 + chatSelectorSize + 1

// BuildMTPHelloAck builds the sealed-payload plaintext for a HELLO reply.
func BuildMTPHelloAck(status byte, ref [chatSelectorSize]byte, chunk byte) []byte {
	b := make([]byte, MTPHelloAckSize)
	b[0] = status
	copy(b[1:], ref[:])
	b[1+chatSelectorSize] = chunk
	return b
}

// ParseMTPHelloAck parses a HELLO reply.
func ParseMTPHelloAck(pt []byte) (status byte, ref [chatSelectorSize]byte, chunk byte, err error) {
	if len(pt) < 1 {
		return 0, ref, 0, errMTPShort
	}
	if pt[0] != MTPStatusOK {
		return pt[0], ref, 0, nil
	}
	if len(pt) < MTPHelloAckSize {
		return 0, ref, 0, errMTPShort
	}
	copy(ref[:], pt[1:])
	return pt[0], ref, pt[1+chatSelectorSize], nil
}
