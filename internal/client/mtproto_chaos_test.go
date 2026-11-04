package client

import (
	"bytes"
	"context"
	"crypto/ecdh"
	cryptoRand "crypto/rand"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/protocol"
	"github.com/sartoopjj/thefeed/internal/server"
)

// The real client and the real relay over a network that loses answers and
// hands cells over again late, with the tunnel re-dialled on the same session
// again and again, as gotd does. Every message must come back intact.
func TestTunnelSurvivesLateCopiesAcrossRedials(t *testing.T) {
	ek, err := ecdh.X25519().GenerateKey(cryptoRand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	qk, _, err := protocol.DeriveKeys("chaos-pass")
	if err != nil {
		t.Fatal(err)
	}
	const mtpDomain = "m.example.com"
	dc := newEchoDC(t)
	relay := server.NewMTProtoRelay(server.MTProtoConfig{
		Enabled: true, Domains: []string{mtpDomain}, Ek: ek, QueryKey: qk,
		IdleTTL: time.Minute, DialTimeout: 5 * time.Second,
		DCAddrs: map[uint8]string{2: dc.addr()},
	})

	rng := rand.New(rand.NewSource(7))
	var rmu sync.Mutex
	chance := func(p float64) bool { rmu.Lock(); defer rmu.Unlock(); return rng.Float64() < p }
	delay := func() time.Duration {
		rmu.Lock()
		defer rmu.Unlock()
		return time.Duration(rng.Intn(250)) * time.Millisecond
	}
	var late sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); late.Wait() }()

	deliver := func(qname string) ([]byte, error) {
		sel, ctr, payload, err := protocol.DecodeChatCell(qk, qname, mtpDomain)
		if err != nil {
			return nil, err
		}
		return relay.HandleCell(sel, ctr, payload, mtpDomain, time.Now()), nil
	}
	query := func(qctx context.Context, qname string) ([]byte, error) {
		resp, err := deliver(qname)
		if err != nil {
			return nil, err
		}
		if chance(0.5) { // a resolver hands it over again, later
			late.Add(1)
			go func(d time.Duration) {
				defer late.Done()
				select {
				case <-time.After(d):
					deliver(qname)
				case <-ctx.Done():
				}
			}(delay())
		}
		if chance(0.1) { // the answer is lost on the way back
			return nil, errors.New("lost")
		}
		return resp, nil
	}

	info := relay.Info()
	sess, err := NewMTProtoSession(query, qk, protocol.QuerySingleLabel, &info, protocol.ChatCellPlainSize)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		hctx, c := context.WithTimeout(ctx, 3*time.Second)
		err = sess.Handshake(hctx)
		c()
		if err == nil {
			break
		}
		if i == 20 {
			t.Fatalf("handshake: %v", err)
		}
	}

	for round := 0; round < 15; round++ {
		conn, err := DialTunnel(ctx, sess.Exchange, 2, TunnelOptions{
			Chunk: sess.Chunk(), IdlePoll: 20 * time.Millisecond, MaxIdlePoll: 200 * time.Millisecond, Window: 4,
		})
		if err != nil {
			t.Fatalf("round %d: dial: %v", round, err)
		}
		body := make([]byte, 3*sess.Chunk()+5)
		cryptoRand.Read(body)
		msg := abridgedFrame(body)
		conn.SetDeadline(time.Now().Add(20 * time.Second))
		if _, err := conn.Write(msg); err != nil {
			t.Fatalf("round %d: write: %v", round, err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("round %d: read: %v", round, err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("round %d: the message came back corrupted", round)
		}
		conn.Close()
	}
}
