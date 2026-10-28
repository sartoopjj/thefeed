package tgbridge

import (
	"context"
	"errors"
	"fmt"
	"github.com/cenkalti/backoff/v4"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
)

// SessionFile persists the gotd session (which holds the MTProto auth key) to
// a file. Losing it means logging in again; leaking it means someone else is
// the user on Telegram, so it is written 0600 under the data dir and must be
// part of encrypted backup -- never of a plain export.
type SessionFile struct {
	Path string
	// Seal and Open encrypt the session at rest when both are set. The bridge
	// never holds the key: the caller supplies the pair. A session is a
	// Telegram auth key, so anyone who copies this file has the account.
	Seal func([]byte) ([]byte, error)
	Open func([]byte) ([]byte, error)
}

func (s SessionFile) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, session.ErrNotFound
		}
		return nil, err
	}
	if s.Open == nil {
		return data, nil
	}
	plain, err := s.Open(data)
	if err != nil {
		// Sealed, but not with a key we have. Report the failure rather than
		// "not found": a not-found answer makes gotd start a fresh login, and
		// the first thing that login does is overwrite these bytes.
		return nil, err
	}
	return plain, nil
}

func (s SessionFile) StoreSession(_ context.Context, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	if s.Seal != nil {
		sealed, err := s.Seal(data)
		if err != nil {
			// Locked. Writing plaintext here would silently undo the
			// encryption of a file that is already sealed on disk.
			return err
		}
		data = sealed
	}
	return os.WriteFile(s.Path, data, 0o600)
}

// LoggedIn reports whether a session exists on disk. It does not prove the
// session is still valid -- Telegram can revoke it -- only that there is
// something to resume, which is what the UI needs to decide between "sign in"
// and "connecting".
func (s SessionFile) LoggedIn() bool {
	st, err := os.Stat(s.Path)
	return err == nil && st.Size() > 0
}

// gotd's defaults assume a normal network: a 1-minute key exchange, 35s to
// connect, 15s per request, and a retry every 5s. Over the tunnel one query
// carries a handful of bytes, so a key exchange is minutes and every one of
// those fires first — gotd then re-dials, throwing the progress away, and the
// login never finishes however long the user waits.
const (
	// exchangeTimeout bounds the MTProto key exchange, six round trips of a
	// few hundred bytes each.
	exchangeTimeout = 10 * time.Minute
	// dialTimeout bounds opening one connection, which here is a tunnel OPEN
	// plus its sealed session handshake.
	dialTimeout = 5 * time.Minute
	// retryInterval is how long gotd waits for a request to be acked before
	// sending the WHOLE thing again. At one query a second a 400-byte request
	// takes ~25s just to transmit, so a short interval re-sends it while it is
	// still going out and the duplicates then compete for the same budget --
	// the link collapses and nothing ever completes.
	//
	// The tunnel is already reliable (the relay acks byte offsets and unacked
	// bytes are re-sent by the transport), so an MTProto-level resend buys
	// nothing here and is set far beyond the slowest link worth supporting.
	retryInterval = 5 * time.Minute
	// maxRetries before giving up and reporting the failure to the user.
	maxRetries = 5
	// ackInterval batches the acks gotd owes Telegram. Longer than gotd's
	// 15-second default because a round trip here can exceed it, and well under
	// slowLinkSuppressFrom: that clock silences every LONG ticker, so an ack
	// interval drifting up to the threshold would silence the acks too.
	ackInterval = 30 * time.Second
	// reconnectFloor / reconnectCeiling space the rebuilding of a dropped
	// connection. A dropped connection has to be rebuilt from scratch, so the
	// first retry is prompt -- waiting only makes the user wait. The ceiling
	// is what stops a systematic failure from restarting a key exchange
	// dozens of times a minute, each attempt spending the queries the next
	// one needs.
	reconnectFloor   = time.Second
	reconnectCeiling = 30 * time.Second
)

// reconnectBackoff retries promptly, then slows if failures persist.
func reconnectBackoff() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = reconnectFloor
	b.MaxInterval = reconnectCeiling
	b.MaxElapsedTime = 0 // keep trying; the user decides when to give up
	return b
}

// migrationTimeout allows a full handshake on the new data centre over a slow
// tunnel, with room for retries.
const migrationTimeout = 5 * time.Minute

// Options configures a Telegram client that runs over the DNS tunnel.
type Options struct {
	// APIID / APIHash identify the client application to Telegram. Telegram
	// allows third-party clients; these are the app's credentials, not the
	// user's, and are not secret in the way the session is.
	APIID   int
	APIHash string
	// Dial opens a tunnel to a data centre by id.
	Dial Dialer
	// Session stores the auth key. Required: without persistence the user
	// would have to re-authenticate on every start, and each login costs a
	// code round trip through Telegram.
	Session telegram.SessionStorage
	// TestDCs talks to Telegram's test data centres. The relay must be run
	// with --mtproto-test-dcs to match, or the connection goes nowhere.
	TestDCs bool
	// NoUpdates skips the update loop. The tunnel is bandwidth-scarce, so the
	// default is to leave updates OFF and fetch explicitly.
	NoUpdates bool
	// Log, if set, receives gotd's own messages. Without it a failed MTProto
	// handshake is completely silent: gotd just retries behind a backoff and
	// the caller sees an unexplained stall.
	Log func(string)
}

// NewClient builds a Telegram client whose every byte travels through the DNS
// tunnel. Call Run on it to connect; authentication is the caller's job (the
// code arrives on the user's own device, so only they can complete it).
func NewClient(opts Options) (*telegram.Client, error) {
	if opts.APIID == 0 || opts.APIHash == "" {
		return nil, errors.New("tgbridge: api id/hash required")
	}
	if opts.Session == nil {
		return nil, errors.New("tgbridge: session storage required")
	}
	resolver, err := NewResolver(opts.Dial)
	if err != nil {
		return nil, err
	}
	tgOpts := telegram.Options{
		Resolver:       resolver,
		SessionStorage: opts.Session,
		// A migration is a WHOLE second handshake on another data centre,
		// which a number whose home DC differs always triggers.
		MigrationTimeout:    migrationTimeout,
		ExchangeTimeout:     exchangeTimeout,
		DialTimeout:         dialTimeout,
		RetryInterval:       retryInterval,
		MaxRetries:          maxRetries,
		ReconnectionBackoff: reconnectBackoff,
		NoUpdates:           opts.NoUpdates,
		// Silences gotd's ping and salt loops, which otherwise rebuild the
		// connection about once a minute on a slow link. See slowclock.go.
		Clock: newSlowLinkClock(),
		// Set explicitly so it can never collide with the intervals that clock
		// suppresses, and so acks are not delayed by gotd's 15-second default
		// when a round trip costs more than that.
		AckInterval: ackInterval,
	}
	if opts.TestDCs {
		tgOpts.DC = 2
		tgOpts.DCList = dcsTestList()
	}
	if opts.Log != nil {
		tgOpts.Logger = newLogger(opts.Log)
	}
	client := telegram.NewClient(opts.APIID, opts.APIHash, tgOpts)
	if client == nil {
		return nil, fmt.Errorf("tgbridge: client not created")
	}
	return client, nil
}
