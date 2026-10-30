package web

import (
	"bytes"
	"compress/flate"
	"io"
	"os"
	"runtime"
	"testing"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Media over DNS is STREAMED, so the whole-payload cap in the client does not
// cover it: this path pipes compressed blocks into a decompressor and copies
// the result straight out, with a tee into a growing buffer for the disk
// cache. Unbounded, a resolver's few hundred kilobytes inflate into gigabytes
// -- teeBuf grows until the process dies, and the browser is handed a stream
// that never ends.
//
// The bound is the size the file was announced as. A real file never exceeds
// its own declared length, so nothing legitimate is refused.
func TestStreamedMediaCannotInflatePastItsDeclaredSize(t *testing.T) {
	// 64 MiB of zeros in a few hundred KB on the wire.
	var bomb bytes.Buffer
	w, _ := flate.NewWriter(&bomb, flate.BestCompression)
	zeros := make([]byte, 1<<20)
	for i := 0; i < 64; i++ {
		w.Write(zeros)
	}
	w.Close()

	const declared = 4096 // what block 0 claimed the file was

	rc, err := client.DecompressMediaReader(bytes.NewReader(bomb.Bytes()), protocol.MediaCompressionDeflate)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	guarded := &cappedReader{r: rc, left: declared, max: declared}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	n, err := io.Copy(io.Discard, guarded)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatalf("%d bytes on the wire inflated to %d unchallenged while claiming to be %d",
			bomb.Len(), n, declared)
	}
	if n > declared {
		t.Errorf("passed through %d bytes past a declared size of %d", n, declared)
	}
	// Refusing it has to be cheap, or it is the same denial by another route.
	if mb := (after.TotalAlloc - before.TotalAlloc) / (1 << 20); mb > 64 {
		t.Errorf("allocated %d MiB to refuse a %d-byte file", mb, declared)
	}
}

// The declared size is an exact bound, so a file of exactly that length must
// come through whole -- this is the normal case for every real download.
func TestStreamedMediaOfExactlyItsDeclaredSizePassesWhole(t *testing.T) {
	// Not compressible, like real media: a jpeg or mp4 is already compressed.
	payload := make([]byte, 300000)
	for i := range payload {
		payload[i] = byte(i*31 + i/251)
	}
	var comp bytes.Buffer
	w, _ := flate.NewWriter(&comp, flate.BestSpeed)
	w.Write(payload)
	w.Close()

	rc, err := client.DecompressMediaReader(bytes.NewReader(comp.Bytes()), protocol.MediaCompressionDeflate)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	size := int64(len(payload))
	got, err := io.ReadAll(&cappedReader{r: rc, left: size, max: size})
	if err != nil {
		t.Fatalf("a real file of its declared size was refused: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %d bytes, want %d -- the stream was altered", len(got), len(payload))
	}
}

// With no declared size there is nothing to compare against, so the fallback
// is what the blocks could even carry -- still proportional, never a fixed
// ceiling, because how large a file a server relays is its operator's choice.
func TestNoDeclaredSizeFallsBackToWhatTheBlocksCouldCarry(t *testing.T) {
	// One block of 600 bytes cannot hold a gigabyte however it is compressed.
	oneBlock := protocol.InflateCap(1 * protocol.MaxBlockPayload)
	if oneBlock > 8<<20 {
		t.Errorf("a single block allows %d bytes of output", oneBlock)
	}
	// A server relaying 35 MB files splits them across many blocks, and must
	// not be penalised for it.
	blocks := (35 << 20) / protocol.MaxBlockPayload
	if big := protocol.InflateCap(blocks * protocol.MaxBlockPayload); big < 35<<20 {
		t.Errorf("a 35 MB file in %d blocks is capped at %d, below its own size", blocks, big)
	}
}

// The guard above is only worth anything if the handler actually uses it. A
// unit test on cappedReader keeps passing when the wiring is reverted to a
// bare DecompressMediaReader, which is how the client-side version of this
// fix sat unapplied once already.
func TestStreamingHandlerWrapsTheDecompressor(t *testing.T) {
	src, err := os.ReadFile("media.go")
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(src, []byte("func (s *Server) handleMediaGet"))
	if i < 0 {
		t.Fatal("handleMediaGet not found")
	}
	fn := src[i:]
	if j := bytes.Index(fn, []byte("\n}\n")); j > 0 {
		fn = fn[:j]
	}
	if !bytes.Contains(fn, []byte("cappedReader{")) {
		t.Error("handleMediaGet decompresses without a cap: a resolver's few hundred " +
			"kilobytes inflate into gigabytes through teeBuf and the response")
	}
}

// A short read must FAIL rather than truncate: a silently cut-off file would
// be streamed to the browser and, worse, written to the disk cache as if it
// were whole, so every later view serves the broken copy from disk.
func TestCappedReaderFailsRatherThanTruncating(t *testing.T) {
	src := bytes.Repeat([]byte("x"), 100)
	n, err := io.Copy(io.Discard, &cappedReader{r: bytes.NewReader(src), left: 50, max: 50})
	if err == nil {
		t.Fatal("an oversized stream was truncated silently")
	}
	if n > 50 {
		t.Errorf("passed %d bytes through a cap of 50", n)
	}
}
