package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// The capability payload never changes, yet fetching it is a step the user
// watches ("finding server") on every connect, reconnects included -- several
// DNS round trips, each retried up to chatProbeAttempts times on a bad link.
//
// Caching it is safe in a way that caching a SESSION would not be:
//
//   - it carries no secret. The x25519 key in it is the server's PUBLIC
//     handshake key, already readable by anyone who can answer DNS.
//   - it is signed, and a cached copy goes through exactly the same
//     verification as a fetched one. A tampered file is rejected, so the file
//     cannot be used to substitute an attacker's relay key.
//   - it is disposable. Anything wrong with it costs one re-fetch.
type mtpInfoCacheFile struct {
	// Raw and Extra are the bytes as fetched, kept verbatim so the signature
	// can be re-checked rather than trusted.
	Raw   []byte `json:"raw"`
	Extra []byte `json:"extra"`
	// Key pins the cache to the server key it was verified against: a config
	// whose key changed must not be served a payload signed by the old one.
	Key []byte `json:"key"`
	At  int64  `json:"at"`
}

// mtpInfoTTL re-reads the payload now and then so a rotated relay key is
// picked up without the user having to do anything. Long, because the cost of
// being a little stale is one failed handshake and a re-fetch.
const mtpInfoTTL = 24 * time.Hour

// LoadMTProtoInfo returns the cached capability payload for this server, or
// fetches and stores one. path is a file in the caller's data directory; an
// empty path simply skips the cache.
func LoadMTProtoInfo(ctx context.Context, f *Fetcher, path string) (*protocol.MTPInfo, error) {
	if f == nil {
		return nil, ErrMTProtoNoServerKey
	}
	if path != "" {
		if info := readMTPInfoCache(f, path); info != nil {
			return info, nil
		}
	}
	info, raw, extra, err := fetchMTProtoInfoRaw(ctx, f)
	if err != nil {
		return nil, err
	}
	if path != "" {
		writeMTPInfoCache(f, path, raw, extra)
	}
	return info, nil
}

// ForgetMTProtoInfo drops the cached payload. Called when a handshake fails
// with the key it advertised: the stored copy is then the prime suspect, and
// one wasted re-fetch beats a client that can never reconnect.
func ForgetMTProtoInfo(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

func readMTPInfoCache(f *Fetcher, path string) *protocol.MTPInfo {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c mtpInfoCacheFile
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	if len(c.Key) == 0 || f.serverPubKey == nil || string(c.Key) != string(f.serverPubKey) {
		return nil // a different server, or a rotated pin
	}
	if c.At <= 0 || time.Since(time.Unix(c.At, 0)) > mtpInfoTTL {
		return nil
	}
	// The same check a fetched payload gets. A file someone edited fails here.
	info, err := f.verifiedMTPInfo(c.Raw, c.Extra)
	if err != nil {
		return nil
	}
	return info
}

func writeMTPInfoCache(f *Fetcher, path string, raw, extra []byte) {
	if f.serverPubKey == nil {
		return
	}
	data, err := json.Marshal(mtpInfoCacheFile{
		Raw: raw, Extra: extra, Key: f.serverPubKey, At: time.Now().Unix(),
	})
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}
