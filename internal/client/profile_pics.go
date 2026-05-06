package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// ProfilePicsBundle is the client-side view of the profile-pic
// directory. The bundle (Size/CRC/Relays) describes the GitHub-served
// concatenated blob; per-entry DNSChannel/DNSBlocks describe an
// independent DNS fallback for that single avatar.
type ProfilePicsBundle struct {
	BundleSize uint32
	BundleCRC  uint32
	// Relays describes where the bundle is reachable, indexed by
	// RelayDNS / RelayGitHub. RelayGitHub means the bundle is on
	// GitHub. RelayDNS for the bundle is rarely true — the standard
	// DNS path uses per-entry channels (see ProfilePicEntry).
	Relays []bool

	Entries []ProfilePicEntry
}

// HasRelay forwards to the relay availability bit at idx.
func (b ProfilePicsBundle) HasRelay(idx int) bool {
	if idx < 0 || idx >= len(b.Relays) {
		return false
	}
	return b.Relays[idx]
}

// ProfilePicEntry points at one avatar in two ways:
//
//	GitHub bundle path: bytes are bundle[Offset:Offset+Size]; CRC must
//	  equal CRC32-IEEE of that slice (use protocol.VerifyEntry).
//	Per-entry DNS path: bytes live on DNS channel DNSChannel with
//	  DNSBlocks blocks. CRC and Size are checked the same way.
//
// The client picks whichever path is reachable. With the bundle path
// one HTTPS request fetches every avatar; with the DNS path each
// avatar is fetched independently so partial sets still show up.
type ProfilePicEntry struct {
	Username   string
	Offset     uint32
	Size       uint32
	CRC        uint32
	MIME       uint8
	DNSChannel uint16
	DNSBlocks  uint16
}

// MimeString returns "image/jpeg" / "image/png" / "image/webp" for the
// MIME tag, suitable for use as an HTTP Content-Type.
func (p ProfilePicEntry) MimeString() string {
	switch p.MIME {
	case protocol.ProfilePicMimePNG:
		return "image/png"
	case protocol.ProfilePicMimeWebP:
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// Extension returns ".jpg" / ".png" / ".webp" for caching on disk.
func (p ProfilePicEntry) Extension() string {
	switch p.MIME {
	case protocol.ProfilePicMimePNG:
		return ".png"
	case protocol.ProfilePicMimeWebP:
		return ".webp"
	default:
		return ".jpg"
	}
}

// FetchProfilePicDirectory pulls the bundle directory from
// ProfilePicsChannel — header (bundle metadata + relay availability) and
// per-username entries. The bundle bytes themselves are NOT fetched here;
// callers do that with FetchMedia(BundleChannel, BundleBlocks, BundleCRC)
// once and then slice locally.
//
// Returns (zero-value bundle, nil) when the server has no profile pics
// configured (or is older and doesn't know the channel).
func (f *Fetcher) FetchProfilePicDirectory(ctx context.Context) (ProfilePicsBundle, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	block0, err := f.FetchBlock(fetchCtx, protocol.ProfilePicsChannel, 0)
	if err != nil {
		return ProfilePicsBundle{}, fmt.Errorf("fetch profile-pics: %w", err)
	}
	if len(block0) < 2 {
		return ProfilePicsBundle{}, nil
	}
	totalBlocks := int(binary.BigEndian.Uint16(block0))
	payload0 := block0[2:]

	if totalBlocks <= 1 {
		return decodeProfilePicsBundle(payload0)
	}

	type res struct {
		data []byte
		err  error
	}
	results := make([]res, totalBlocks)
	results[0] = res{data: payload0}
	var wg sync.WaitGroup
	for blk := 1; blk < totalBlocks; blk++ {
		wg.Add(1)
		go func(blk int) {
			defer wg.Done()
			data, e := f.FetchBlock(fetchCtx, protocol.ProfilePicsChannel, uint16(blk))
			results[blk] = res{data: data, err: e}
		}(blk)
	}
	wg.Wait()

	var all []byte
	for _, r := range results {
		if r.err != nil {
			return ProfilePicsBundle{}, fmt.Errorf("fetch profile-pics block: %w", r.err)
		}
		all = append(all, r.data...)
	}
	return decodeProfilePicsBundle(all)
}

func decodeProfilePicsBundle(data []byte) (ProfilePicsBundle, error) {
	if len(data) == 0 {
		return ProfilePicsBundle{}, nil
	}
	pb, err := protocol.DecodeProfilePicsBundle(data)
	if err != nil {
		return ProfilePicsBundle{}, fmt.Errorf("decode profile-pics: %w", err)
	}
	out := ProfilePicsBundle{
		BundleSize: pb.Header.BundleSize,
		BundleCRC:  pb.Header.BundleCRC,
		Relays:     append([]bool(nil), pb.Header.Relays...),
	}
	out.Entries = make([]ProfilePicEntry, len(pb.Entries))
	for i, e := range pb.Entries {
		out.Entries[i] = ProfilePicEntry{
			Username:   e.Username,
			Offset:     e.Offset,
			Size:       e.Size,
			CRC:        e.CRC,
			MIME:       e.MIME,
			DNSChannel: e.DNSChannel,
			DNSBlocks:  e.DNSBlocks,
		}
	}
	return out, nil
}
