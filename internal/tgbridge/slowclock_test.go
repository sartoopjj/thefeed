package tgbridge

import (
	"testing"
	"time"
)

// The ping and salt loops must never tick: each tears the connection down on a
// 15-second deadline this transport cannot meet.
func TestSlowLinkClockSilencesTheLoopsThatKillTheConnection(t *testing.T) {
	c := newSlowLinkClock()
	// gotd's own values today, plus retuned ones an upgrade could bring: all
	// must stay silent, which is the point of matching on length not number.
	for _, d := range []time.Duration{time.Minute, time.Hour, 90 * time.Second, 5 * time.Minute, slowLinkSuppressFrom} {
		tk := c.Ticker(d)
		defer tk.Stop()
		select {
		case <-tk.C():
			t.Fatalf("ticker for %s fired; gotd's loop will restart the connection", d)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Every other ticker has to keep working, or gotd's acks stop.
func TestSlowLinkClockLeavesOtherTickersAlone(t *testing.T) {
	c := newSlowLinkClock()
	tk := c.Ticker(5 * time.Millisecond)
	defer tk.Stop()
	select {
	case <-tk.C():
	case <-time.After(2 * time.Second):
		t.Fatal("an unrelated ticker never fired")
	}
}

// Now must stay real: MTProto message ids are derived from it, and a skewed
// clock is rejected by Telegram.
func TestSlowLinkClockKeepsRealTime(t *testing.T) {
	if drift := time.Since(newSlowLinkClock().Now()); drift > time.Second || drift < -time.Second {
		t.Fatalf("clock drifted by %s", drift)
	}
}

// ackInterval is the discriminator newSlowLinkClock relies on: if it ever
// equals a suppressed interval, acks stop and the connection dies quietly.
func TestAckIntervalCannotBeSuppressed(t *testing.T) {
	if ackInterval >= slowLinkSuppressFrom {
		t.Fatalf("ackInterval %s is at or past the %s suppression threshold: acks would stop",
			ackInterval, slowLinkSuppressFrom)
	}
	// And prove it through the clock itself, not just the constants: a real
	// ticker, not the dead one. Waiting for it to fire would mean waiting out
	// the whole interval.
	tk := newSlowLinkClock().Ticker(ackInterval)
	defer tk.Stop()
	if _, dead := tk.(deadTicker); dead {
		t.Fatal("the ack ticker was suppressed; gotd would stop acknowledging messages")
	}
	if tk.C() == nil {
		t.Fatal("the ack ticker has no channel")
	}
}
