package tgbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"time"
)

// ErrNotConnected: no client is running, so there is nothing to ask.
var ErrNotConnected = errors.New("tgbridge: not connected")

// Bridge owns one Telegram connection over the tunnel. The relay only moves
// bytes; the auth key never leaves this machine.
type Bridge struct {
	mu     sync.Mutex
	client *telegram.Client
	api    *tg.Client
	// stop cancels the run loop; nil when not running.
	stop context.CancelFunc
	done chan struct{}
	// notifyDef caches the account-wide notification defaults for this
	// connection; notifyOK says whether they have been read yet.
	notifyDef NotifyDefaults
	notifyOK  bool
	// notifyAt is when the last READ FAILED, for spacing out retries.
	notifyAt time.Time
}

// NewBridge creates an idle bridge. Call Start to connect.
func NewBridge() *Bridge { return &Bridge{} }

// Start connects in the background and returns once the connection is up (or
// immediately with an error). The caller keeps using the Bridge; Stop ends it.
func (b *Bridge) Start(ctx context.Context, opts Options) error {
	b.mu.Lock()
	if b.stop != nil {
		b.mu.Unlock()
		return errors.New("tgbridge: already running")
	}
	client, err := NewClient(opts)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	ready := make(chan error, 1)
	done := make(chan struct{})
	b.client = client
	b.stop = cancel
	b.done = done
	b.notifyOK, b.notifyAt = false, time.Time{} // a new connection re-reads them
	b.mu.Unlock()

	go func() {
		defer close(done)
		err := client.Run(runCtx, func(ctx context.Context) error {
			b.mu.Lock()
			b.api = client.API()
			b.mu.Unlock()
			ready <- nil
			// Hold the connection open until the caller stops us.
			<-ctx.Done()
			return ctx.Err()
		})
		b.mu.Lock()
		b.api = nil
		b.mu.Unlock()
		// If Run failed before the callback, unblock Start.
		select {
		case ready <- err:
		default:
		}
	}()

	select {
	case err := <-ready:
		if err != nil && !errors.Is(err, context.Canceled) {
			b.Stop()
			return err
		}
		return nil
	case <-ctx.Done():
		b.Stop()
		return ctx.Err()
	}
}

// Stop closes the connection and waits for the loop to exit.
func (b *Bridge) Stop() {
	b.mu.Lock()
	stop, done := b.stop, b.done
	b.stop, b.done = nil, nil
	b.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	if done != nil {
		<-done
	}
}

// Running reports whether a connection is up.
func (b *Bridge) Running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.api != nil
}

func (b *Bridge) snapshot() (*telegram.Client, *tg.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.api == nil || b.client == nil {
		return nil, nil, ErrNotConnected
	}
	return b.client, b.api, nil
}

// AuthStatus reports whether this session is signed in, and as whom.
func (b *Bridge) AuthStatus(ctx context.Context) (*auth.Status, error) {
	client, _, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	return client.Auth().Status(ctx)
}

// SendCode asks Telegram to send a login code to the user's own device. The
// code never passes through the relay or through us.
func (b *Bridge) SendCode(ctx context.Context, phone string) (string, string, error) {
	client, _, err := b.snapshot()
	if err != nil {
		return "", "", err
	}
	sent, err := client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return "", "", err
	}
	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return "", "", fmt.Errorf("tgbridge: unexpected sent-code type %T", sent)
	}
	return code.PhoneCodeHash, deliveryOf(code.Type), nil
}

// deliveryOf names where Telegram actually put the code. Without it the user is
// told only that a code was sent, and the commonest case by far -- Telegram
// delivering it INSIDE an app the user is already signed in to, not by SMS --
// looks exactly like a code that never arrived.
func deliveryOf(t tg.AuthSentCodeTypeClass) string {
	switch t.(type) {
	case *tg.AuthSentCodeTypeApp:
		return "app"
	case *tg.AuthSentCodeTypeSMS, *tg.AuthSentCodeTypeSMSWord, *tg.AuthSentCodeTypeSMSPhrase:
		return "sms"
	case *tg.AuthSentCodeTypeFragmentSMS:
		return "fragment"
	case *tg.AuthSentCodeTypeFirebaseSMS:
		return "sms"
	case *tg.AuthSentCodeTypeCall:
		return "call"
	case *tg.AuthSentCodeTypeFlashCall, *tg.AuthSentCodeTypeMissedCall:
		return "missedcall"
	case *tg.AuthSentCodeTypeEmailCode, *tg.AuthSentCodeTypeSetUpEmailRequired:
		return "email"
	default:
		return ""
	}
}

// Ping keeps the connection alive; Telegram drops one idle for about a minute,
// and gotd's own keepalive runs too close to that to win over a slow tunnel.
//
// gotd writes the ping straight to its connection without checking that one
// exists, so pinging during a reconnect (every data-centre migration) panics
// inside the library. A keepalive must never be able to take the app down.
func (b *Bridge) Ping(ctx context.Context) error {
	client, _, err := b.snapshot()
	if err != nil {
		return err
	}
	return safeCall(func() error { return client.Ping(ctx) })
}

// safeCall turns a panic inside gotd into an ordinary error. Callers already
// treat a failure as "the connection is gone", which is exactly what a panic
// there means.
func safeCall(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tgbridge: connection unusable: %v", r)
		}
	}()
	return fn()
}

// ResendCode asks for the code through Telegram's next delivery method,
// reusing the existing hash.
func (b *Bridge) ResendCode(ctx context.Context, phone, codeHash string) (string, string, error) {
	client, _, err := b.snapshot()
	if err != nil {
		return "", "", err
	}
	sent, err := client.Auth().ResendCode(ctx, phone, codeHash)
	if err != nil {
		return "", "", err
	}
	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return "", "", fmt.Errorf("tgbridge: unexpected resend type %T", sent)
	}
	return code.PhoneCodeHash, deliveryOf(code.Type), nil
}

// SignIn completes the code step. ErrPasswordNeeded means the account has
// two-step verification on and SignInPassword is next.
func (b *Bridge) SignIn(ctx context.Context, phone, code, codeHash string) error {
	client, _, err := b.snapshot()
	if err != nil {
		return err
	}
	_, err = client.Auth().SignIn(ctx, phone, code, codeHash)
	return err
}

// PasswordHint is the reminder stored with the two-step password.
func (b *Bridge) PasswordHint(ctx context.Context) string {
	_, api, err := b.snapshot()
	if err != nil {
		return ""
	}
	pw, err := api.AccountGetPassword(ctx)
	if err != nil {
		return ""
	}
	hint, _ := pw.GetHint()
	return hint
}

// SignInPassword completes two-step verification.
func (b *Bridge) SignInPassword(ctx context.Context, password string) error {
	client, _, err := b.snapshot()
	if err != nil {
		return err
	}
	_, err = client.Auth().Password(ctx, password)
	return err
}

// LogOut ends the session on Telegram's side as well, so the stored auth key
// cannot be reused. The caller still has to delete the session file.
func (b *Bridge) LogOut(ctx context.Context) error {
	_, api, err := b.snapshot()
	if err != nil {
		return err
	}
	_, err = api.AuthLogOut(ctx)
	return err
}
