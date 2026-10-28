package tgbridge

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gotd/td/proto/codec"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/dcs"
)

// The relay addresses data centres by id, so the resolver must pass the id
// straight through -- no address list, no racing dial.
func TestResolverDialsRequestedDC(t *testing.T) {
	var gotDC int
	r, err := NewResolver(func(_ context.Context, dc int) (net.Conn, error) {
		gotDC = dc
		cl, srv := net.Pipe()
		// Drain the transport tag the handshake writes.
		go io.Copy(io.Discard, srv)
		return cl, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := r.Primary(context.Background(), 4, dcs.List{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if gotDC != 4 {
		t.Fatalf("dialed DC %d, want 4", gotDC)
	}
}

// Abridged, not gotd's default intermediate: over DNS every byte is part of a
// query name, and abridged spends 1 length byte per message where
// intermediate spends 4.
func TestResolverUsesAbridgedTransport(t *testing.T) {
	tagCh := make(chan byte, 1)
	r, err := NewResolver(func(_ context.Context, _ int) (net.Conn, error) {
		cl, srv := net.Pipe()
		go func() {
			var b [1]byte
			if _, err := io.ReadFull(srv, b[:]); err == nil {
				tagCh <- b[0]
			}
			io.Copy(io.Discard, srv)
		}()
		return cl, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := r.Primary(context.Background(), 2, dcs.List{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := <-tagCh; got != codec.AbridgedClientStart[0] {
		t.Fatalf("transport tag %#x, want the abridged tag %#x", got, codec.AbridgedClientStart[0])
	}
}

// Media requests ride the primary path: the relay connects to a DC's primary
// address, which answers them too.
func TestResolverMediaOnlyUsesPrimaryPath(t *testing.T) {
	var calls int
	r, _ := NewResolver(func(_ context.Context, dc int) (net.Conn, error) {
		calls++
		cl, srv := net.Pipe()
		go io.Copy(io.Discard, srv)
		return cl, nil
	})
	conn, err := r.MediaOnly(context.Background(), 5, dcs.List{})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if calls != 1 {
		t.Fatalf("%d dials, want 1", calls)
	}
}

// CDN DCs speak a different flow and the relay allows primary DCs only, so
// this is a hard no rather than something to retry.
func TestResolverRefusesCDN(t *testing.T) {
	r, _ := NewResolver(func(_ context.Context, _ int) (net.Conn, error) {
		t.Error("CDN request reached the dialer")
		return nil, errors.New("unreachable")
	})
	if _, err := r.CDN(context.Background(), 2, dcs.List{}); !errors.Is(err, ErrNoCDNOverTunnel) {
		t.Fatalf("err = %v, want ErrNoCDNOverTunnel", err)
	}
}

// A failed dial must not leak a half-open connection, and a failed handshake
// must close the conn it was given.
func TestResolverClosesConnOnHandshakeFailure(t *testing.T) {
	closed := make(chan struct{})
	r, _ := NewResolver(func(_ context.Context, _ int) (net.Conn, error) {
		cl, srv := net.Pipe()
		srv.Close() // writing the tag will fail
		go func() { <-closed }()
		return &notifyConn{Conn: cl, onClose: func() { close(closed) }}, nil
	})
	if _, err := r.Primary(context.Background(), 2, dcs.List{}); err == nil {
		t.Fatal("expected the handshake to fail")
	}
	select {
	case <-closed:
	default:
		t.Fatal("connection leaked after a failed handshake")
	}
}

type notifyConn struct {
	net.Conn
	onClose func()
	done    bool
}

func (c *notifyConn) Close() error {
	if !c.done {
		c.done = true
		c.onClose()
	}
	return c.Conn.Close()
}

func TestNewResolverRequiresDialer(t *testing.T) {
	if _, err := NewResolver(nil); err == nil {
		t.Fatal("accepted a nil dialer")
	}
}

func TestNewClientValidatesOptions(t *testing.T) {
	dial := func(context.Context, int) (net.Conn, error) { return nil, nil }
	sess := SessionFile{Path: filepath.Join(t.TempDir(), "s.json")}
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"no api id", Options{APIHash: "h", Dial: dial, Session: sess}},
		{"no api hash", Options{APIID: 1, Dial: dial, Session: sess}},
		{"no session", Options{APIID: 1, APIHash: "h", Dial: dial}},
		{"no dialer", Options{APIID: 1, APIHash: "h", Session: sess}},
	} {
		if _, err := NewClient(tc.opts); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	if _, err := NewClient(Options{APIID: 1, APIHash: "h", Dial: dial, Session: sess}); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
}

// The session holds the MTProto auth key: whoever has it IS the user on
// Telegram, so it must never be world-readable.
func TestSessionFileRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "session.json")
	s := SessionFile{Path: path}

	if _, err := s.LoadSession(context.Background()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session: err = %v, want session.ErrNotFound", err)
	}
	if s.LoggedIn() {
		t.Fatal("reported logged in with no session file")
	}

	want := []byte(`{"auth_key":"secret"}`)
	if err := s.StoreSession(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-trip gave %q", got)
	}
	if !s.LoggedIn() {
		t.Fatal("stored a session but LoggedIn is false")
	}

	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("session mode %o, want 600", perm)
		}
	}
}
