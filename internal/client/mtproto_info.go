package client

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Tunnel-discovery outcomes, kept apart so the UI can tell "this server has no
// tunnel" (long backoff) from "we could not reach it" (retry soon) from "its
// capability data did not verify" (never usable, loudly).
var (
	// ErrMTProtoNoServerKey: the config pins no server key, so a signed
	// capability payload cannot be verified. Fail-closed.
	ErrMTProtoNoServerKey = errors.New("mtproto: config has no pinned server key")
	// ErrMTProtoNotOffered: this server serves no tunnel capability payload.
	ErrMTProtoNotOffered = errors.New("mtproto: server does not offer the tunnel")
	// ErrMTProtoServerDisabled: the server advertises the tunnel but has it off.
	ErrMTProtoServerDisabled = errors.New("mtproto: tunnel disabled by the server")
	// ErrMTProtoUnreachable: a transient fetch failure.
	ErrMTProtoUnreachable = errors.New("mtproto: server unreachable")
	// ErrMTProtoUnverified: the capability payload failed signature checks.
	ErrMTProtoUnverified = errors.New("mtproto: capability payload not verified")
)

// maxMTPInfoBlocks caps how much a server can make us fetch for its own
// capability payload.
const maxMTPInfoBlocks = 4

// FetchMTProtoInfo discovers the tunnel on this config: it fetches the signed
// capability payload and verifies it against the pinned server key before
// returning anything. The payload carries the server's x25519 handshake key,
// so an unverified copy would let anyone who can answer DNS stand in for the
// server — it is therefore fail-closed, never best-effort.
func FetchMTProtoInfo(ctx context.Context, f *Fetcher) (*protocol.MTPInfo, error) {
	info, _, _, err := fetchMTProtoInfoRaw(ctx, f)
	return info, err
}

// fetchMTProtoInfoRaw also hands back the bytes it verified, so a caller that
// wants to cache the payload stores exactly what the signature covers and can
// re-check it later instead of trusting its own file.
func fetchMTProtoInfoRaw(ctx context.Context, f *Fetcher) (*protocol.MTPInfo, []byte, []byte, error) {
	if f == nil {
		return nil, nil, nil, errors.New("mtproto: no fetcher")
	}
	if f.serverPubKey == nil {
		return nil, nil, nil, ErrMTProtoNoServerKey
	}

	block0, err := f.fetchBlockAttempts(ctx, protocol.MTProtoInfoChannel, 0, chatProbeAttempts)
	if err != nil || len(block0) < 2 {
		// A server with no tunnel answers not-found here, which clients
		// negative-cache; we cannot distinguish that from a dead network on
		// first contact, so report "not offered" and let the caller back off.
		return nil, nil, nil, ErrMTProtoNotOffered
	}
	total := int(binary.BigEndian.Uint16(block0))
	if total < 1 {
		total = 1
	}
	if total > maxMTPInfoBlocks {
		total = maxMTPInfoBlocks
	}

	blocks := make([][]byte, total)
	blocks[0] = block0
	var extraRaw []byte
	bctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var blkErr error
	var errMu sync.Mutex
	for i := 1; i < total; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b, err := f.FetchBlock(bctx, protocol.MTProtoInfoChannel, uint16(idx))
			if err != nil {
				errMu.Lock()
				if blkErr == nil {
					blkErr = err
					cancel()
				}
				errMu.Unlock()
				return
			}
			blocks[idx] = b
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if b, err := f.fetchBlockAttempts(bctx, protocol.MTProtoInfoChannel, uint16(total), chatProbeAttempts); err == nil {
			extraRaw = b
		}
	}()
	wg.Wait()
	cancel()
	if blkErr != nil {
		return nil, nil, nil, ErrMTProtoUnreachable
	}

	var raw []byte
	for _, b := range blocks {
		raw = append(raw, b...)
	}
	info, err := f.verifiedMTPInfo(raw, extraRaw)
	if err != nil {
		return nil, nil, nil, err
	}
	return info, raw, extraRaw, nil
}

// verifiedMTPInfo checks a capability payload and parses it. Shared by the
// fetch above and by the on-disk cache, so a stored copy can never take a
// shortcut around the signature.
func (f *Fetcher) verifiedMTPInfo(raw, extraRaw []byte) (*protocol.MTPInfo, error) {
	if len(raw) < 2 {
		return nil, ErrMTProtoNotOffered
	}
	// Fail-closed on BOTH a bad signature and a missing one: unlike chat there
	// is no "seen before" state here, and the payload carries a key.
	if verr := f.verifyExtraBytes(protocol.MTProtoInfoChannel, raw, extraRaw); verr != nil {
		return nil, ErrMTProtoUnverified
	}

	info, err := protocol.ParseMTPInfo(raw[2:])
	if err != nil {
		return nil, ErrMTProtoNotOffered
	}
	if !info.Enabled {
		return nil, ErrMTProtoServerDisabled
	}
	if !info.Usable() {
		return nil, ErrMTProtoNotOffered
	}
	return info, nil
}
