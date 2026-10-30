package web

import (
	"io"
	"strings"
	"testing"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Hostile and malformed Host headers. The guard decides whether a request is
// answered at all, so anything it mishandles is either a refused real user or
// an accepted rebinding attempt.
func TestHostAllowedOnNastyInput(t *testing.T) {
	allowed := allowedHosts("127.0.0.1")

	// Must be refused. Each is a way of dressing up a name we do not serve.
	for _, h := range []string{
		"", ":", ":8080", "::", "[::]", "[", "]",
		"evil.com", "evil.com:80",
		"localhost.evil.com", "localhost.evil.com:8080",
		"127.0.0.1.evil.com", "127.0.0.1.evil.com:8080",
		"evil.com:8080:9090",
		"127.0.0.1@evil.com", "evil.com#127.0.0.1",
		"notlocalhost", "localhostx", "xlocalhost",
		"0.0.0.0", "0.0.0.0:8080",
		"192.168.1.5", "10.0.0.1:8080", "169.254.169.254",
		strings.Repeat("a", 300) + ".evil.com",
		"local\x00host", "localhost\n",
		"localhost ", " localhost",
	} {
		if hostAllowed(h, allowed) {
			t.Errorf("hostAllowed(%q) = true; that name is not this machine", h)
		}
	}

	// Must be accepted. Each is a real way the app is opened.
	for _, h := range []string{
		"127.0.0.1", "127.0.0.1:8080", "127.0.0.1:65535",
		"localhost", "localhost:8080", "LOCALHOST:8080",
		"[::1]", "[::1]:8080", "::1",
		"app.localhost", "app.localhost:38000",
		"127.0.0.53", "127.1.2.3:9",
	} {
		if !hostAllowed(h, allowed) {
			t.Errorf("hostAllowed(%q) = false; that is this machine", h)
		}
	}
}

// THEFEED_EXTRA_HOSTS is operator text, so it has to survive sloppiness
// without either dropping a real entry or widening into a suffix match.
func TestExtraHostsParsing(t *testing.T) {
	t.Setenv("THEFEED_EXTRA_HOSTS", "  Feed.Example.COM , ,,  feed.home.lan  ,")
	allowed := allowedHosts("127.0.0.1")
	for _, h := range []string{"feed.example.com", "FEED.EXAMPLE.COM:443", "feed.home.lan:8080"} {
		if !hostAllowed(h, allowed) {
			t.Errorf("a listed name was refused: %q", h)
		}
	}
	// Listing a name must not list everything that ends in it.
	for _, h := range []string{"evil-feed.example.com", "feed.example.com.evil.net", "home.lan"} {
		if hostAllowed(h, allowed) {
			t.Errorf("%q was accepted; listing a name is not listing its suffixes", h)
		}
	}
}

// cappedReader sits between the network and both the browser and the disk
// cache, so its edges matter: an off-by-one either truncates a real file or
// lets an extra byte through.
func TestCappedReaderEdges(t *testing.T) {
	// Exactly at the cap: whole, clean EOF.
	src := strings.Repeat("a", 64)
	got, err := io.ReadAll(&cappedReader{r: strings.NewReader(src), left: 64, max: 64})
	if err != nil || string(got) != src {
		t.Errorf("exactly-at-cap: %d bytes, err=%v", len(got), err)
	}
	// One byte over: refused, and never more than the cap handed on.
	n, err := io.Copy(io.Discard, &cappedReader{r: strings.NewReader(src + "b"), left: 64, max: 64})
	if err == nil {
		t.Error("one byte over the cap was accepted")
	}
	if n > 64 {
		t.Errorf("passed %d bytes through a cap of 64", n)
	}
	// A cap of zero: nothing may pass, and an empty source is still clean.
	if _, err := io.Copy(io.Discard, &cappedReader{r: strings.NewReader("x"), left: 0, max: 0}); err == nil {
		t.Error("a zero cap let a byte through")
	}
	if _, err := io.ReadAll(&cappedReader{r: strings.NewReader(""), left: 0, max: 0}); err != nil {
		t.Errorf("an empty source under a zero cap errored: %v", err)
	}
	// A zero-length read request must not be mistaken for the end.
	cr := &cappedReader{r: strings.NewReader("abc"), left: 3, max: 3}
	if n, err := cr.Read(nil); n != 0 || err != nil {
		t.Errorf("Read(nil) = %d, %v", n, err)
	}
	if got, err := io.ReadAll(cr); err != nil || string(got) != "abc" {
		t.Errorf("after Read(nil): %q %v", got, err)
	}
}

// InflateCap is handed a length that came off the wire, so it must stay sane
// at the extremes rather than wrap into a useless bound.
func TestInflateCapExtremes(t *testing.T) {
	for _, in := range []int{0, 1, -1, -1 << 20} {
		if got := protocol.InflateCap(in); got < 4<<20 {
			t.Errorf("InflateCap(%d) = %d, below the floor", in, got)
		}
	}
	// Proportional in the middle, and never below the input itself.
	for _, in := range []int{1 << 20, 35 << 20, 100 << 20} {
		if got := protocol.InflateCap(in); got < in {
			t.Errorf("InflateCap(%d) = %d, below its own input", in, got)
		}
	}
	// A huge length must not overflow into a negative or tiny cap.
	if got := protocol.InflateCap(1 << 40); got < 1<<40 {
		t.Errorf("InflateCap(1<<40) = %d, which wrapped", got)
	}
}

// The ack watermark decides what gets freed, so duplicates and extremes must
// not move it backwards.
func TestAckWatermarkEdges(t *testing.T) {
	a := addr(1)
	k := client.ChatAddressString(a)
	// Duplicates and a zero seq alongside a real one.
	got := ackWatermarks([]client.ChatIncoming{
		{From: a, Seq: 0}, {From: a, Seq: 5}, {From: a, Seq: 5}, {From: a, Seq: 3},
	})
	if got[k] != 5 {
		t.Errorf("watermark = %d, want 5", got[k])
	}
	// The top of the range must not wrap.
	got = ackWatermarks([]client.ChatIncoming{{From: a, Seq: ^uint32(0)}, {From: a, Seq: 1}})
	if got[k] != ^uint32(0) {
		t.Errorf("watermark = %d, want max", got[k])
	}
	// A peer whose only entry is seq 0 gets no watermark, which would be an
	// inbox slot that can never be acknowledged -- except that seq 0 cannot
	// reach an inbox. The store keeps a per-pair LastAccepted starting at 0
	// and treats seq == LastAccepted as an already-delivered duplicate
	// (chatstore.go, CommitMessage), so the first storable seq is 1. Checked
	// against the real service: a send at seq 0 leaves the inbox empty.
	//
	// Pinned rather than defended against: if that ever changes, this says
	// what else has to change with it.
	if got := ackWatermarks([]client.ChatIncoming{{From: a, Seq: 0}}); len(got) != 0 {
		t.Error("seq 0 now produces a watermark; if seq 0 can also reach an " +
			"inbox, an entry at seq 0 would be unackable")
	}
}
