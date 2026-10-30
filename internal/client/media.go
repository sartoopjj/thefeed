package client

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"sync"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// DecompressMediaReader wraps r per the given compression.
func DecompressMediaReader(r io.Reader, compression protocol.MediaCompression) (io.ReadCloser, error) {
	switch compression {
	case protocol.MediaCompressionNone:
		return io.NopCloser(r), nil
	case protocol.MediaCompressionGzip:
		return gzip.NewReader(r)
	case protocol.MediaCompressionDeflate:
		return flate.NewReader(r), nil
	}
	return nil, fmt.Errorf("unsupported media compression: %d", compression)
}

func decompressMediaBytes(body []byte, compression protocol.MediaCompression) ([]byte, error) {
	rc, err := DecompressMediaReader(bytes.NewReader(body), compression)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// BOUNDED: these bytes came off the network and deflate expands about a
	// thousand times, so a small answer can inflate until the process dies.
	//
	// The bound scales with what ARRIVED rather than being a fixed ceiling:
	// how large a file a server relays is its operator's choice -- 100 KB on
	// one, 35 MB on another -- and a number written here would be wrong for
	// every server that chose differently. What no real file does is expand
	// thirty-twofold.
	limit := protocol.InflateCap(len(body))
	out, err := io.ReadAll(io.LimitReader(rc, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > limit {
		return nil, fmt.Errorf("media expands past %d bytes from %d on the wire, which no real file does",
			limit, len(body))
	}
	return out, nil
}

// MediaProgress reports per-block progress (completed of total). May be
// invoked from a background goroutine.
type MediaProgress func(completed, total int)

// MediaBlockHeaderLen re-exports the protocol header length so callers in
// the web layer don't have to import the protocol package twice.
const MediaBlockHeaderLen = protocol.MediaBlockHeaderLen

// ErrMediaHashMismatch indicates the assembled bytes don't match the
// expected CRC32. The caller must discard the returned bytes.
var ErrMediaHashMismatch = fmt.Errorf("media content hash mismatch")

// mediaBlockOuterRetries is the per-block retry budget the media path adds
// on top of FetchBlock's own internal retries. A ~200-block file can lose
// individual blocks repeatedly; without this, one persistent bad block
// kills the whole download even though FetchBlock would succeed on a
// later attempt.
const mediaBlockOuterRetries = 5

func (f *Fetcher) fetchMediaBlock(ctx context.Context, channel, block uint16) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < mediaBlockOuterRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		data, err := f.FetchBlock(ctx, channel, block)
		if err == nil {
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
	}
	return nil, lastErr
}

// FetchMedia returns the assembled bytes of a media blob served on a media
// channel, optionally verifying expectedCRC32.
func (f *Fetcher) FetchMedia(ctx context.Context, channel uint16, blockCount uint16, expectedCRC32 uint32, progress MediaProgress) ([]byte, error) {
	if !protocol.IsMediaChannel(channel) {
		return nil, fmt.Errorf("channel %d is outside media range", channel)
	}
	if blockCount == 0 {
		return nil, nil
	}

	type blockResult struct {
		idx  int
		data []byte
		err  error
	}

	results := make(chan blockResult, blockCount)
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for i := 0; i < int(blockCount); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results <- blockResult{idx: idx, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			data, err := f.fetchMediaBlock(ctx, channel, uint16(idx))
			results <- blockResult{idx: idx, data: data, err: err}
		}(i)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	ordered := make([][]byte, blockCount)
	completed := 0
	var progMu sync.Mutex
	for r := range results {
		if r.err != nil {
			if r.err == ctx.Err() {
				return nil, r.err
			}
			return nil, fmt.Errorf("media channel %d block %d: %w", channel, r.idx, r.err)
		}
		ordered[r.idx] = r.data
		completed++
		if progress != nil {
			progMu.Lock()
			progress(completed, int(blockCount))
			progMu.Unlock()
		}
	}

	if len(ordered) == 0 || len(ordered[0]) < protocol.MediaBlockHeaderLen {
		return nil, fmt.Errorf("media channel %d: malformed block 0", channel)
	}
	header, err := protocol.DecodeMediaBlockHeader(ordered[0][:protocol.MediaBlockHeaderLen])
	if err != nil {
		return nil, fmt.Errorf("media channel %d: %w", channel, err)
	}
	if expectedCRC32 != 0 && header.CRC32 != expectedCRC32 {
		return nil, ErrMediaHashMismatch
	}

	// Concatenate all block bytes after the header.
	total := len(ordered[0]) - protocol.MediaBlockHeaderLen
	for i := 1; i < len(ordered); i++ {
		total += len(ordered[i])
	}
	body := make([]byte, 0, total)
	body = append(body, ordered[0][protocol.MediaBlockHeaderLen:]...)
	for i := 1; i < len(ordered); i++ {
		body = append(body, ordered[i]...)
	}

	// Decompress per the header.
	out, err := decompressMediaBytes(body, header.Compression)
	if err != nil {
		return nil, fmt.Errorf("decompress media channel %d: %w", channel, err)
	}
	if expectedCRC32 != 0 {
		if got := crc32.ChecksumIEEE(out); got != expectedCRC32 {
			return nil, ErrMediaHashMismatch
		}
	}
	return out, nil
}

// FetchMediaBlocksStream fetches blocks [startBlock, startBlock+count) and
// writes each block's raw bytes to w in order as soon as they become
// contiguous. No header parsing; callers slice off the protocol header
// themselves and decompress as appropriate. Cancelling ctx aborts both
// in-flight DNS queries and pending writes.
func (f *Fetcher) FetchMediaBlocksStream(ctx context.Context, channel, startBlock, count uint16, w io.Writer, progress MediaProgress) error {
	if !protocol.IsMediaChannel(channel) {
		return fmt.Errorf("channel %d is outside media range", channel)
	}
	if count == 0 {
		return nil
	}

	type blockResult struct {
		idx  int
		data []byte
		err  error
	}
	results := make(chan blockResult, count)
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for i := 0; i < int(count); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results <- blockResult{idx: idx, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			data, err := f.fetchMediaBlock(ctx, channel, uint16(int(startBlock)+idx))
			results <- blockResult{idx: idx, data: data, err: err}
		}(i)
	}
	go func() { wg.Wait(); close(results) }()

	pending := make(map[int][]byte)
	next := 0
	completed := 0
	for r := range results {
		if r.err != nil {
			if r.err == ctx.Err() {
				return r.err
			}
			return fmt.Errorf("media channel %d block %d: %w", channel, int(startBlock)+r.idx, r.err)
		}
		pending[r.idx] = r.data
		for {
			payload, ok := pending[next]
			if !ok {
				break
			}
			if _, werr := w.Write(payload); werr != nil {
				return werr
			}
			if flusher, ok := w.(interface{ Flush() }); ok {
				flusher.Flush()
			}
			next++
		}
		completed++
		if progress != nil {
			progress(completed, int(count))
		}
	}
	if next != int(count) {
		return fmt.Errorf("media channel %d: incomplete (%d / %d)", channel, next, count)
	}
	return nil
}
