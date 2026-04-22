package protocol

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

const (
	maxDNSLabelLen = 63
	maxDNSNameLen  = 253 // without trailing dot

	// SendChannel is the special channel number used for upstream message sending.
	// When a query has channel == SendChannel, the block field encodes the target
	// channel number, and additional data labels carry the encrypted message text.
	SendChannel uint16 = 0xFFFE

	// AdminChannel is the special channel number for admin commands (add/remove
	// channels, hard refresh). The encrypted payload is "password\ncmd\narg".
	AdminChannel uint16 = 0xFFFD

	// UpstreamInitChannel starts a chunked upstream session for admin/send payloads.
	UpstreamInitChannel uint16 = 0xFFFC
	// UpstreamDataChannel carries one chunk of a chunked upstream session.
	UpstreamDataChannel uint16 = 0xFFFB

	// VersionChannel serves latest release version with random suffix.
	VersionChannel uint16 = 0xFFFA

	// TitlesChannel serves per-channel human-readable display names.
	TitlesChannel uint16 = 0xFFF9

	// MaxUpstreamBlockPayload keeps uploaded query chunks comfortably below DNS
	// name limits across typical domains and resolver paths.
	MaxUpstreamBlockPayload = 8
	// MaxUpstreamBlocks bounds the amount of server-side session state.
	MaxUpstreamBlocks = 128
)

// UpstreamKind identifies the payload carried by a chunked upstream session.
type UpstreamKind byte

const (
	UpstreamKindSend  UpstreamKind = 1
	UpstreamKindAdmin UpstreamKind = 2
)

// AdminCmd identifies admin commands carried in upstream admin payloads.
type AdminCmd byte

const (
	AdminCmdAddChannel    AdminCmd = 1
	AdminCmdRemoveChannel AdminCmd = 2
	AdminCmdListChannels  AdminCmd = 3
	AdminCmdRefresh       AdminCmd = 4
)

// UpstreamInit describes a chunked upstream session.
type UpstreamInit struct {
	SessionID     uint16
	TotalBlocks   uint8
	Kind          UpstreamKind
	TargetChannel uint8
}

// QueryEncoding controls how DNS query subdomains are encoded.
type QueryEncoding int

const (
	// QuerySingleLabel uses base32 in a single DNS label (default, stealthier).
	QuerySingleLabel QueryEncoding = iota
	// QueryMultiLabel uses hex split across multiple DNS labels.
	QueryMultiLabel
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// EncodeQuery creates a DNS query subdomain for the given channel and block.
// Single-label (default): [base32_encrypted].domain
// Multi-label:            [hex_part1].[hex_part2].domain
// All queries are encrypted to prevent DPI detection.
func EncodeQuery(queryKey [KeySize]byte, channel, block uint16, domain string, mode QueryEncoding) (string, error) {
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return "", fmt.Errorf("empty domain")
	}

	payload := make([]byte, QueryPayloadSize)

	if _, err := rand.Read(payload[:QueryPaddingSize]); err != nil {
		return "", fmt.Errorf("random padding: %w", err)
	}

	binary.BigEndian.PutUint16(payload[QueryPaddingSize:], channel)
	binary.BigEndian.PutUint16(payload[QueryPaddingSize+QueryChannelSize:], block)

	encrypted, err := encryptQueryBlock(queryKey, payload)
	if err != nil {
		return "", fmt.Errorf("encrypt query: %w", err)
	}

	// Append 0–4 random suffix bytes so query length varies per request.
	// The decoder strips these by only using the first aes.BlockSize bytes.
	suffixLen, _ := rand.Int(rand.Reader, big.NewInt(5)) // [0,4]
	suffix := make([]byte, int(suffixLen.Int64()))
	rand.Read(suffix) //nolint:errcheck — non-critical randomness
	ciphertext := append(encrypted, suffix...)

	switch mode {
	case QueryMultiLabel:
		h := hex.EncodeToString(ciphertext)
		labels := splitMultiLabel(h)
		return joinQName(labels, domain)
	default:
		encoded := strings.ToLower(b32.EncodeToString(ciphertext))
		return joinQName([]string{encoded}, domain)
	}
}

// splitMultiLabel splits a hex string into two labels of randomised, unequal length.
// The first label is between 12 and (len-4) chars so the second is at least 4 chars.
// This makes query labels look less uniform across requests.
func splitMultiLabel(h string) []string {
	if len(h) <= 8 {
		return []string{h}
	}
	// first label: random length in [minFirst, len-4]
	minFirst := 8
	maxFirst := len(h) - 4
	if maxFirst <= minFirst {
		maxFirst = minFirst + 1
	}
	// crypto/rand for the split point; fall back to midpoint on error
	split := (len(h) + 1) / 2 // default: slightly off-centre
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(maxFirst-minFirst+1))); err == nil {
		split = minFirst + int(n.Int64())
	}
	return []string{h[:split], h[split:]}
}

func joinQName(labels []string, domain string) (string, error) {
	for _, l := range labels {
		if len(l) == 0 {
			return "", fmt.Errorf("empty label")
		}
		if len(l) > maxDNSLabelLen {
			return "", fmt.Errorf("label too long: %d", len(l))
		}
	}

	qname := strings.Join(append(labels, domain), ".")
	if len(qname) > maxDNSNameLen {
		return "", fmt.Errorf("query name too long: %d", len(qname))
	}
	return qname, nil
}

// DecodeQuery parses and decrypts a DNS query subdomain.
// Auto-detects plain-text (c<N>b<M>), single-label base32, or multi-label hex encoding.
func DecodeQuery(queryKey [KeySize]byte, qname, domain string) (channel, block uint16, err error) {
	qname = strings.TrimSuffix(qname, ".")
	domain = strings.TrimSuffix(domain, ".")

	suffix := "." + domain
	if !strings.HasSuffix(strings.ToLower(qname), strings.ToLower(suffix)) {
		return 0, 0, fmt.Errorf("domain mismatch: %q does not end with %q", qname, suffix)
	}

	encoded := qname[:len(qname)-len(suffix)]

	// Try base32 (single-label: no dots or dots stripped)
	b32str := strings.ReplaceAll(encoded, ".", "")
	if ct, e := b32.DecodeString(strings.ToUpper(b32str)); e == nil {
		return parseQueryCiphertext(queryKey, ct)
	}

	// Fall back to hex (multi-label: dots stripped)
	hexStr := strings.ReplaceAll(encoded, ".", "")
	if ct, e := hex.DecodeString(hexStr); e == nil {
		return parseQueryCiphertext(queryKey, ct)
	}

	// For multi-label data queries (header_b32.data_hex...), the concatenated
	// string is neither valid base32 nor valid hex. Try the first label alone
	// — it contains the AES-ECB encrypted header with channel and block.
	if parts := strings.SplitN(encoded, ".", 2); len(parts) == 2 {
		if ct, e := b32.DecodeString(strings.ToUpper(parts[0])); e == nil {
			return parseQueryCiphertext(queryKey, ct)
		}
		if ct, e := hex.DecodeString(parts[0]); e == nil {
			return parseQueryCiphertext(queryKey, ct)
		}
	}

	return 0, 0, fmt.Errorf("decode query: invalid encoding")
}

func parseQueryCiphertext(queryKey [KeySize]byte, ciphertext []byte) (channel, block uint16, err error) {
	plaintext, err := decryptQueryBlock(queryKey, ciphertext)
	if err != nil {
		return 0, 0, fmt.Errorf("decrypt: %w", err)
	}
	channel = binary.BigEndian.Uint16(plaintext[QueryPaddingSize:])
	block = binary.BigEndian.Uint16(plaintext[QueryPaddingSize+QueryChannelSize:])
	return channel, block, nil
}

// EncodeResponse encrypts and base64-encodes a block payload for a DNS TXT response.
// Adds a 2-byte length prefix and random padding to vary response size for anti-DPI.
func EncodeResponse(responseKey [KeySize]byte, data []byte, maxPadding int) (string, error) {
	padLen := 0
	if maxPadding > 0 {
		buf := make([]byte, 1)
		rand.Read(buf)
		padLen = int(buf[0]) % (maxPadding + 1)
	}

	padded := make([]byte, PadLengthSize+len(data)+padLen)
	binary.BigEndian.PutUint16(padded, uint16(len(data)))
	copy(padded[PadLengthSize:], data)
	if padLen > 0 {
		rand.Read(padded[PadLengthSize+len(data):])
	}

	encrypted, err := Encrypt(responseKey, padded)
	if err != nil {
		return "", fmt.Errorf("encrypt response: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

// DecodeResponse base64-decodes and decrypts a DNS TXT response, stripping padding.
func DecodeResponse(responseKey [KeySize]byte, encoded string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	padded, err := Decrypt(responseKey, ciphertext)
	if err != nil {
		return nil, err
	}
	if len(padded) < PadLengthSize {
		return nil, fmt.Errorf("response too short")
	}
	dataLen := int(binary.BigEndian.Uint16(padded))
	if dataLen > len(padded)-PadLengthSize {
		return nil, fmt.Errorf("invalid data length in response: %d", dataLen)
	}
	return padded[PadLengthSize : PadLengthSize+dataLen], nil
}

// EncodeSendQuery creates a DNS query that carries an upstream message.
// Format: [header_b32].[data_b32].domain
// The header is a normal encrypted 8-byte query with channel=SendChannel and
// block=targetChannel. The data label contains GCM-encrypted message text.
// Returns an error if the message is too long for a single DNS query.
func EncodeSendQuery(queryKey [KeySize]byte, targetChannel uint16, message []byte, domain string, mode QueryEncoding) (string, error) {
	return encodeDataQuery(queryKey, SendChannel, targetChannel, message, domain, mode)
}

// EncodeAdminQuery creates a DNS query that carries an admin command to the server.
// The payload is a single AdminCmd byte followed by optional argument bytes,
// GCM-encrypted and split across DNS labels.
func EncodeAdminQuery(queryKey [KeySize]byte, cmd AdminCmd, arg []byte, domain string, mode QueryEncoding) (string, error) {
	payload := append([]byte{byte(cmd)}, arg...)
	return encodeDataQuery(queryKey, AdminChannel, 0, payload, domain, mode)
}

// encodeDataQuery builds a DNS query carrying encrypted data in additional labels.
func encodeDataQuery(queryKey [KeySize]byte, specialCh, block uint16, data []byte, domain string, mode QueryEncoding) (string, error) {
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return "", fmt.Errorf("empty domain")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty payload")
	}

	// Build header
	header := make([]byte, QueryPayloadSize)
	if _, err := rand.Read(header[:QueryPaddingSize]); err != nil {
		return "", fmt.Errorf("random padding: %w", err)
	}
	binary.BigEndian.PutUint16(header[QueryPaddingSize:], specialCh)
	binary.BigEndian.PutUint16(header[QueryPaddingSize+QueryChannelSize:], block)

	encHeader, err := encryptQueryBlock(queryKey, header)
	if err != nil {
		return "", fmt.Errorf("encrypt header: %w", err)
	}

	// Encrypt data with GCM
	encData, err := Encrypt(queryKey, data)
	if err != nil {
		return "", fmt.Errorf("encrypt message: %w", err)
	}

	// Encode header and data
	headerStr := strings.ToLower(b32.EncodeToString(encHeader))
	dataStr := strings.ToLower(hex.EncodeToString(encData))

	// Validate total query name fits in DNS limits (253 chars max)
	// Each data label adds len+1 (for dot), header adds len+1, domain adds len+1
	totalLen := len(headerStr) + 1 + len(dataStr) + (len(dataStr) / maxDNSLabelLen) + 1 + len(domain)
	if totalLen > 253 {
		return "", fmt.Errorf("message too large for DNS query (%d chars, max 253)", totalLen)
	}

	// Split data into DNS labels (max 63 chars each)
	var dataLabels []string
	for len(dataStr) > maxDNSLabelLen {
		dataLabels = append(dataLabels, dataStr[:maxDNSLabelLen])
		dataStr = dataStr[maxDNSLabelLen:]
	}
	if len(dataStr) > 0 {
		dataLabels = append(dataLabels, dataStr)
	}

	// Build query name: header.data1.data2...dataN.domain
	allLabels := append([]string{headerStr}, dataLabels...)
	return joinQName(allLabels, domain)
}

// DecodeSendQuery decodes a send-message DNS query. Returns the target channel
// number and decrypted message text.
func DecodeSendQuery(queryKey [KeySize]byte, qname, domain string) (targetChannel uint16, message []byte, err error) {
	qname = strings.TrimSuffix(qname, ".")
	domain = strings.TrimSuffix(domain, ".")

	suffix := "." + domain
	if !strings.HasSuffix(strings.ToLower(qname), strings.ToLower(suffix)) {
		return 0, nil, fmt.Errorf("domain mismatch")
	}

	encoded := qname[:len(qname)-len(suffix)]
	parts := strings.Split(encoded, ".")
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("send query needs at least header + data labels")
	}

	// Decode header (first label)
	headerLabel := parts[0]
	headerCT, err := b32.DecodeString(strings.ToUpper(headerLabel))
	if err != nil {
		// Try hex fallback
		headerCT, err = hex.DecodeString(headerLabel)
		if err != nil {
			return 0, nil, fmt.Errorf("decode header: %w", err)
		}
	}

	plaintext, err := decryptQueryBlock(queryKey, headerCT)
	if err != nil {
		return 0, nil, fmt.Errorf("decrypt header: %w", err)
	}

	ch := binary.BigEndian.Uint16(plaintext[QueryPaddingSize:])
	if ch != SendChannel {
		return 0, nil, fmt.Errorf("not a send query (channel=%d)", ch)
	}
	targetChannel = binary.BigEndian.Uint16(plaintext[QueryPaddingSize+QueryChannelSize:])

	// Decode data labels (remaining labels, concatenated hex)
	dataHex := strings.Join(parts[1:], "")
	dataCT, err := hex.DecodeString(dataHex)
	if err != nil {
		return 0, nil, fmt.Errorf("decode data: %w", err)
	}

	message, err = Decrypt(queryKey, dataCT)
	if err != nil {
		return 0, nil, fmt.Errorf("decrypt message: %w", err)
	}

	return targetChannel, message, nil
}

// DecodeAdminQuery decodes an admin command DNS query and returns the command and argument.
func DecodeAdminQuery(queryKey [KeySize]byte, qname, domain string) (cmd AdminCmd, arg []byte, err error) {
	qname = strings.TrimSuffix(qname, ".")
	domain = strings.TrimSuffix(domain, ".")

	suffix := "." + domain
	if !strings.HasSuffix(strings.ToLower(qname), strings.ToLower(suffix)) {
		return 0, nil, fmt.Errorf("domain mismatch")
	}

	encoded := qname[:len(qname)-len(suffix)]
	parts := strings.Split(encoded, ".")
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("admin query needs at least header + data labels")
	}

	headerLabel := parts[0]
	headerCT, err := b32.DecodeString(strings.ToUpper(headerLabel))
	if err != nil {
		headerCT, err = hex.DecodeString(headerLabel)
		if err != nil {
			return 0, nil, fmt.Errorf("decode header: %w", err)
		}
	}

	plaintext, err := decryptQueryBlock(queryKey, headerCT)
	if err != nil {
		return 0, nil, fmt.Errorf("decrypt header: %w", err)
	}

	ch := binary.BigEndian.Uint16(plaintext[QueryPaddingSize:])
	if ch != AdminChannel {
		return 0, nil, fmt.Errorf("not an admin query (channel=%d)", ch)
	}

	dataHex := strings.Join(parts[1:], "")
	dataCT, err := hex.DecodeString(dataHex)
	if err != nil {
		return 0, nil, fmt.Errorf("decode data: %w", err)
	}

	payload, err := Decrypt(queryKey, dataCT)
	if err != nil {
		return 0, nil, fmt.Errorf("decrypt payload: %w", err)
	}

	if len(payload) == 0 {
		return 0, nil, fmt.Errorf("empty admin payload")
	}
	cmd = AdminCmd(payload[0])
	if len(payload) > 1 {
		arg = payload[1:]
	}
	return cmd, arg, nil
}

// EncodeUpstreamInitQuery creates a compact single-label query that registers
// a chunked upstream session. All init data is packed into the AES-ECB header:
//
//	[0:2] session_id, [2] total_blocks, [3] kind,
//	[4:6] channel=UpstreamInitChannel, [6] target_channel, [7] 0
//
// No GCM data labels — just one 26-char base32 label + domain.
func EncodeUpstreamInitQuery(queryKey [KeySize]byte, init UpstreamInit, domain string, mode QueryEncoding) (string, error) {
	if init.SessionID == 0 {
		return "", fmt.Errorf("session id is required")
	}
	if init.TotalBlocks == 0 || int(init.TotalBlocks) > MaxUpstreamBlocks {
		return "", fmt.Errorf("invalid block count: %d", init.TotalBlocks)
	}
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return "", fmt.Errorf("empty domain")
	}

	payload := make([]byte, QueryPayloadSize)
	binary.BigEndian.PutUint16(payload[0:], init.SessionID)
	payload[2] = init.TotalBlocks
	payload[3] = byte(init.Kind)
	binary.BigEndian.PutUint16(payload[QueryPaddingSize:], UpstreamInitChannel)
	payload[6] = init.TargetChannel
	// payload[7] = 0 (zero-padded)

	encrypted, err := encryptQueryBlock(queryKey, payload)
	if err != nil {
		return "", fmt.Errorf("encrypt init: %w", err)
	}

	encoded := strings.ToLower(b32.EncodeToString(encrypted))
	return joinQName([]string{encoded}, domain)
}

// DecodeUpstreamInitQuery parses a compact single-label upstream init query.
func DecodeUpstreamInitQuery(queryKey [KeySize]byte, qname, domain string) (*UpstreamInit, error) {
	qname = strings.TrimSuffix(qname, ".")
	domain = strings.TrimSuffix(domain, ".")

	suffix := "." + domain
	if !strings.HasSuffix(strings.ToLower(qname), strings.ToLower(suffix)) {
		return nil, fmt.Errorf("domain mismatch")
	}

	encoded := qname[:len(qname)-len(suffix)]
	label := strings.ReplaceAll(encoded, ".", "")

	ct, err := b32.DecodeString(strings.ToUpper(label))
	if err != nil {
		ct, err = hex.DecodeString(label)
		if err != nil {
			return nil, fmt.Errorf("decode init: %w", err)
		}
	}

	plaintext, err := decryptQueryBlock(queryKey, ct)
	if err != nil {
		return nil, fmt.Errorf("decrypt init: %w", err)
	}

	ch := binary.BigEndian.Uint16(plaintext[QueryPaddingSize:])
	if ch != UpstreamInitChannel {
		return nil, fmt.Errorf("not an upstream init query (channel=%d)", ch)
	}

	init := &UpstreamInit{
		SessionID:     binary.BigEndian.Uint16(plaintext[0:2]),
		TotalBlocks:   plaintext[2],
		Kind:          UpstreamKind(plaintext[3]),
		TargetChannel: plaintext[6],
	}
	if init.SessionID == 0 {
		return nil, fmt.Errorf("invalid upstream session id")
	}
	if init.TotalBlocks == 0 || int(init.TotalBlocks) > MaxUpstreamBlocks {
		return nil, fmt.Errorf("invalid upstream block count: %d", init.TotalBlocks)
	}
	if init.Kind != UpstreamKindSend && init.Kind != UpstreamKindAdmin {
		return nil, fmt.Errorf("invalid upstream kind: %d", init.Kind)
	}
	return init, nil
}

// EncodeUpstreamBlockQuery encodes one chunk of a chunked upstream payload
// into a single DNS label. The first min(2, len(chunk)) bytes are embedded in
// the AES-ECB header at [6:8] (not covered by integrity check); any remaining
// bytes are appended raw after the 16-byte ciphertext. The upstream payload is
// already GCM-encrypted, so confidentiality is preserved; tampering is caught
// by GCM on reassembly.
//
//	Header: [0:2] session_id, [2] index, [3] chunk_len,
//	        [4:6] channel=UpstreamDataChannel, [6:8] chunk prefix
//	Suffix: chunk[2:] (raw, up to 6 bytes)
//
// Max label: 16 + 6 = 22 bytes → 36 base32 chars (fits in 63-char DNS label).
func EncodeUpstreamBlockQuery(queryKey [KeySize]byte, sessionID uint16, index uint8, chunk []byte, domain string, mode QueryEncoding) (string, error) {
	if sessionID == 0 {
		return "", fmt.Errorf("session id is required")
	}
	if len(chunk) == 0 {
		return "", fmt.Errorf("empty upstream block")
	}
	if len(chunk) > MaxUpstreamBlockPayload {
		return "", fmt.Errorf("upstream block too large: %d > %d", len(chunk), MaxUpstreamBlockPayload)
	}
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return "", fmt.Errorf("empty domain")
	}

	header := make([]byte, QueryPayloadSize)
	binary.BigEndian.PutUint16(header[0:], sessionID)
	header[2] = index
	header[3] = byte(len(chunk))
	binary.BigEndian.PutUint16(header[QueryPaddingSize:], UpstreamDataChannel)

	// Embed first 2 bytes of chunk in header[6:8].
	inHeader := len(chunk)
	if inHeader > 2 {
		inHeader = 2
	}
	copy(header[6:], chunk[:inHeader])

	encHeader, err := encryptQueryBlock(queryKey, header)
	if err != nil {
		return "", fmt.Errorf("encrypt header: %w", err)
	}

	// Append remaining chunk bytes (raw) after the encrypted header.
	combined := encHeader
	if len(chunk) > 2 {
		combined = append(combined, chunk[2:]...)
	}

	label := strings.ToLower(b32.EncodeToString(combined))

	return joinQName([]string{label}, domain)
}

// DecodeUpstreamBlockQuery decodes one chunk of a chunked upstream payload.
// The first 2 bytes of chunk data live in the encrypted header[6:8]; any
// remaining bytes follow the 16-byte ciphertext as raw bytes.
func DecodeUpstreamBlockQuery(queryKey [KeySize]byte, qname, domain string) (sessionID uint16, index uint8, chunk []byte, err error) {
	qname = strings.TrimSuffix(qname, ".")
	domain = strings.TrimSuffix(domain, ".")

	suffix := "." + domain
	if !strings.HasSuffix(strings.ToLower(qname), strings.ToLower(suffix)) {
		return 0, 0, nil, fmt.Errorf("domain mismatch")
	}

	encoded := qname[:len(qname)-len(suffix)]
	// Single label — no dots expected
	label := strings.ReplaceAll(encoded, ".", "")

	raw, err := b32.DecodeString(strings.ToUpper(label))
	if err != nil {
		return 0, 0, nil, fmt.Errorf("decode label: %w", err)
	}
	if len(raw) < 16 {
		return 0, 0, nil, fmt.Errorf("block query too short: %d bytes", len(raw))
	}

	plaintext, err := decryptQueryBlock(queryKey, raw[:16])
	if err != nil {
		return 0, 0, nil, fmt.Errorf("decrypt header: %w", err)
	}

	ch := binary.BigEndian.Uint16(plaintext[QueryPaddingSize:])
	if ch != UpstreamDataChannel {
		return 0, 0, nil, fmt.Errorf("not an upstream data query (channel=%d)", ch)
	}

	sessionID = binary.BigEndian.Uint16(plaintext[0:2])
	if sessionID == 0 {
		return 0, 0, nil, fmt.Errorf("invalid upstream session id")
	}
	index = plaintext[2]
	chunkLen := int(plaintext[3])
	if chunkLen == 0 || chunkLen > MaxUpstreamBlockPayload {
		return 0, 0, nil, fmt.Errorf("invalid chunk length: %d", chunkLen)
	}

	chunk = make([]byte, chunkLen)
	// First min(2, chunkLen) bytes from header[6:8].
	inHeader := chunkLen
	if inHeader > 2 {
		inHeader = 2
	}
	copy(chunk[:inHeader], plaintext[6:6+inHeader])

	// Remaining bytes from raw suffix after the 16-byte ciphertext.
	if chunkLen > 2 {
		extra := raw[16:]
		need := chunkLen - 2
		if len(extra) < need {
			return 0, 0, nil, fmt.Errorf("insufficient data bytes: have %d, need %d", len(extra), need)
		}
		copy(chunk[2:], extra[:need])
	}

	return sessionID, index, chunk, nil
}
