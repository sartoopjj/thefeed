package protocol

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const (
	maxDNSLabelLen = 63
	maxDNSNameLen  = 253 // without trailing dot
)

// QueryEncoding controls how DNS query subdomains are encoded.
type QueryEncoding int

const (
	// QuerySingleLabel uses base32 in a single DNS label (default, stealthier).
	QuerySingleLabel QueryEncoding = iota
	// QueryMultiLabel uses hex split across multiple DNS labels.
	QueryMultiLabel
	// QueryPlainLabel encodes channel and block as plain decimal text (no query encryption).
	// Responses are always encrypted regardless of this setting.
	QueryPlainLabel
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// EncodeQuery creates a DNS query subdomain for the given channel and block.
// Single-label (default): [base32_encrypted].domain
// Multi-label:            [hex_part1].[hex_part2].domain
// Plain-label:            c<channel>b<block>.domain  (no query encryption)
// Responses are always encrypted regardless of mode.
func EncodeQuery(queryKey [KeySize]byte, channel, block uint16, domain string, mode QueryEncoding) (string, error) {
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return "", fmt.Errorf("empty domain")
	}

	// Plain text mode: no encryption, just human-readable label.
	if mode == QueryPlainLabel {
		label := fmt.Sprintf("c%db%d", channel, block)
		return joinQName([]string{label}, domain)
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

	// Try plain-label first: c<channel>b<block> (short, no dots, all decimal)
	if ch, blk, ok := parsePlainLabel(encoded); ok {
		return ch, blk, nil
	}

	// Try base32 (single-label: no dots or dots stripped)
	b32str := strings.ReplaceAll(encoded, ".", "")
	if ct, e := b32.DecodeString(strings.ToUpper(b32str)); e == nil {
		return parseQueryCiphertext(queryKey, ct)
	}

	// Fall back to hex (multi-label: dots stripped)
	hexStr := strings.ReplaceAll(encoded, ".", "")
	ct, e := hex.DecodeString(hexStr)
	if e != nil {
		return 0, 0, fmt.Errorf("decode query: invalid encoding")
	}
	return parseQueryCiphertext(queryKey, ct)
}

// parsePlainLabel parses the plain-text query format "c<channel>b<block>".
// Returns ok=false if the string does not match this pattern.
func parsePlainLabel(s string) (channel, block uint16, ok bool) {
	if len(s) < 3 || s[0] != 'c' {
		return 0, 0, false
	}
	bi := strings.IndexByte(s[1:], 'b')
	if bi < 0 {
		return 0, 0, false
	}
	bi++ // adjust for the slice offset
	chStr, bStr := s[1:bi], s[bi+1:]
	if len(chStr) == 0 || len(bStr) == 0 {
		return 0, 0, false
	}
	ch, err1 := strconv.ParseUint(chStr, 10, 16)
	blk, err2 := strconv.ParseUint(bStr, 10, 16)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return uint16(ch), uint16(blk), true
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
