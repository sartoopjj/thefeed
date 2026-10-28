package tgbridge

import (
	"time"

	"github.com/gotd/td/clock"
)

// gotd runs background loops that each tear the connection down on a
// 15-second deadline, and none of their intervals is reachable through
// telegram.Options:
//
//   - pingLoop returns "disconnect (pong missed)" on a late pong, killing the
//     whole connection group.
//   - saltsLoop asks for future salts under the same 15-second timeout.
//
// A ping round trip over DNS cells costs tens of seconds, so on a degraded
// link both fire, both fail, and the connection is rebuilt about once a minute
// -- losing every RPC in flight and restarting the login flow
// (AUTH_RESTART). gotd also holds its write mutex while the doomed ping write
// blocks, so real requests queue behind it.
//
// Liveness does not need them: the tunnel reports a dead far end directly
// (EOF or a lost-tunnel status), and telHub.keepAlive pings on a schedule this
// transport can meet.
//
// Silenced by INTERVAL LENGTH, not by matching gotd's numbers. mtproto has
// three tickers -- acks, pings, salts -- and only acks are frequent, so
// suppressing every long one survives gotd retuning its periods, which pinning
// to "1 minute" and "1 hour" would not.
const slowLinkSuppressFrom = 50 * time.Second

// slowLinkClock is the real clock with those loops silenced. Now and Timer are
// untouched: message IDs and every retry timer still use real time.
type slowLinkClock struct{ clock.Clock }

// newSlowLinkClock returns a clock safe for a link measured in queries per
// second rather than packets per millisecond.
func newSlowLinkClock() clock.Clock { return slowLinkClock{clock.System} }

func (c slowLinkClock) Ticker(d time.Duration) clock.Ticker {
	if d >= slowLinkSuppressFrom {
		return deadTicker{}
	}
	return c.Clock.Ticker(d)
}

// deadTicker never fires, so the loop waiting on it parks for ever.
type deadTicker struct{}

func (deadTicker) C() <-chan time.Time { return nil }
func (deadTicker) Stop()               {}
func (deadTicker) Reset(time.Duration) {}
