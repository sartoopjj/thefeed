package protocol

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Profile pictures use a hybrid layout: every avatar is concatenated
// into one bundle uploaded to the GitHub relay (one file → no
// per-file rate limit), and each avatar also gets its own DNS media
// channel so partial fetches over DNS still display.
//
// Wire layout of ProfilePicsChannel (after the block-count prefix the
// Feed layer adds):
//
//	bundleSize    uint32
//	bundleCRC     uint32
//	relayCount    uint8       — N
//	relays        [N]u8       — bool per relay (RelayDNS=0, RelayGitHub=1, …)
//	count         uint16
//	entries:
//	    usernameLen uint8
//	    username    [usernameLen]byte
//	    offset      uint32     — within the GitHub bundle
//	    size        uint32
//	    crc         uint32     — CRC32 of bundle[offset:offset+size]
//	    mime        uint8      — 0=jpeg, 1=png, 2=webp
//	    dnsChannel  uint16     — 0 if not on DNS
//	    dnsBlocks   uint16
type ProfilePicsBundleHeader struct {
	BundleSize uint32
	BundleCRC  uint32
	// One bool per relay constant. RelayGitHub here means the bundle
	// is on GitHub; RelayDNS for the bundle is rare (per-entry DNS
	// channels handle the DNS path).
	Relays []bool
}

// HasRelay reports whether the relay at idx is set. Out of range returns false.
func (h ProfilePicsBundleHeader) HasRelay(idx int) bool {
	if idx < 0 || idx >= len(h.Relays) {
		return false
	}
	return h.Relays[idx]
}

// ProfilePicEntry points at one avatar via either the GitHub bundle
// (Offset/Size into the concatenated blob) or its own DNS channel
// (DNSChannel/DNSBlocks). Both paths verify the same Size + CRC.
type ProfilePicEntry struct {
	Username   string
	Offset     uint32
	Size       uint32
	CRC        uint32
	MIME       uint8
	DNSChannel uint16
	DNSBlocks  uint16
}

// MIME tag values.
const (
	ProfilePicMimeJPEG uint8 = 0
	ProfilePicMimePNG  uint8 = 1
	ProfilePicMimeWebP uint8 = 2
)

// On-the-wire byte counts.
const (
	profilePicEntryFixed   = 4 + 4 + 4 + 1 + 2 + 2 // offset+size+crc+mime+dnsCh+dnsBlk
	profilePicsHeaderFixed = 4 + 4 + 1             // bundleSize+bundleCRC+relayCount
)

// ProfilePicsBundle is the directory (header + entries). The bundle
// bytes themselves live in the referenced media channel / relay.
type ProfilePicsBundle struct {
	Header  ProfilePicsBundleHeader
	Entries []ProfilePicEntry
}

// EncodeProfilePicsBundle serialises the directory.
func EncodeProfilePicsBundle(b ProfilePicsBundle) []byte {
	relayCount := len(b.Header.Relays)
	if relayCount > 255 {
		relayCount = 255
	}
	size := profilePicsHeaderFixed + relayCount + 2 /*entry count*/
	for _, e := range b.Entries {
		n := len(e.Username)
		if n > 255 {
			n = 255
		}
		size += 1 + n + profilePicEntryFixed
	}
	buf := make([]byte, size)
	off := 0
	binary.BigEndian.PutUint32(buf[off:], b.Header.BundleSize)
	off += 4
	binary.BigEndian.PutUint32(buf[off:], b.Header.BundleCRC)
	off += 4
	buf[off] = byte(relayCount)
	off++
	for i := 0; i < relayCount; i++ {
		if b.Header.Relays[i] {
			buf[off] = 1
		}
		off++
	}
	binary.BigEndian.PutUint16(buf[off:], uint16(len(b.Entries)))
	off += 2
	for _, e := range b.Entries {
		nb := []byte(e.Username)
		if len(nb) > 255 {
			nb = nb[:255]
		}
		buf[off] = byte(len(nb))
		off++
		copy(buf[off:], nb)
		off += len(nb)
		binary.BigEndian.PutUint32(buf[off:], e.Offset)
		off += 4
		binary.BigEndian.PutUint32(buf[off:], e.Size)
		off += 4
		binary.BigEndian.PutUint32(buf[off:], e.CRC)
		off += 4
		buf[off] = e.MIME
		off++
		binary.BigEndian.PutUint16(buf[off:], e.DNSChannel)
		off += 2
		binary.BigEndian.PutUint16(buf[off:], e.DNSBlocks)
		off += 2
	}
	return buf
}

// DecodeProfilePicsBundle parses bytes produced by EncodeProfilePicsBundle.
func DecodeProfilePicsBundle(data []byte) (ProfilePicsBundle, error) {
	var out ProfilePicsBundle
	if len(data) < profilePicsHeaderFixed+2 {
		return out, fmt.Errorf("profile-pics bundle too short: %d bytes", len(data))
	}
	off := 0
	out.Header.BundleSize = binary.BigEndian.Uint32(data[off:])
	off += 4
	out.Header.BundleCRC = binary.BigEndian.Uint32(data[off:])
	off += 4
	relayCount := int(data[off])
	off++
	if off+relayCount+2 > len(data) {
		return out, fmt.Errorf("profile-pics bundle: truncated relay list")
	}
	if relayCount > 0 {
		out.Header.Relays = make([]bool, relayCount)
		for i := 0; i < relayCount; i++ {
			out.Header.Relays[i] = data[off] != 0
			off++
		}
	}
	count := int(binary.BigEndian.Uint16(data[off:]))
	off += 2
	out.Entries = make([]ProfilePicEntry, 0, count)
	for i := 0; i < count; i++ {
		if off >= len(data) {
			return out, fmt.Errorf("profile-pics: truncated at entry %d", i)
		}
		nameLen := int(data[off])
		off++
		if off+nameLen+profilePicEntryFixed > len(data) {
			return out, fmt.Errorf("profile-pics: truncated entry %d body", i)
		}
		name := string(data[off : off+nameLen])
		off += nameLen
		offset := binary.BigEndian.Uint32(data[off:])
		off += 4
		sz := binary.BigEndian.Uint32(data[off:])
		off += 4
		cr := binary.BigEndian.Uint32(data[off:])
		off += 4
		mime := data[off]
		off++
		dnsCh := binary.BigEndian.Uint16(data[off:])
		off += 2
		dnsBlk := binary.BigEndian.Uint16(data[off:])
		off += 2
		out.Entries = append(out.Entries, ProfilePicEntry{
			Username:   name,
			Offset:     offset,
			Size:       sz,
			CRC:        cr,
			MIME:       mime,
			DNSChannel: dnsCh,
			DNSBlocks:  dnsBlk,
		})
	}
	return out, nil
}

// VerifyEntry returns bundle[entry.Offset:entry.Offset+entry.Size] if
// the slice is in-range and its CRC32-IEEE matches entry.CRC. The
// hash check is what stops a misaligned bundle from serving the wrong
// avatar under a username.
func VerifyEntry(bundle []byte, entry ProfilePicEntry) ([]byte, error) {
	end := uint64(entry.Offset) + uint64(entry.Size)
	if end > uint64(len(bundle)) {
		return nil, fmt.Errorf("entry %q out of range: offset=%d size=%d bundle=%d",
			entry.Username, entry.Offset, entry.Size, len(bundle))
	}
	slice := bundle[entry.Offset:end]
	if uint32(len(slice)) != entry.Size {
		return nil, fmt.Errorf("entry %q size mismatch: have %d want %d",
			entry.Username, len(slice), entry.Size)
	}
	if got := crc32.ChecksumIEEE(slice); got != entry.CRC {
		return nil, fmt.Errorf("entry %q crc mismatch: have %08x want %08x",
			entry.Username, got, entry.CRC)
	}
	return slice, nil
}
