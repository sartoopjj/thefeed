package server

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// relayOn builds a relay that serves the given domains. It needs no DC: these
// tests only exercise domain wiring, never a tunnel.
func relayOn(t *testing.T, domains []string) *MTProtoRelay {
	t.Helper()
	ek, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var qk [protocol.KeySize]byte
	r := NewMTProtoRelay(MTProtoConfig{Enabled: true, Domains: domains, Ek: ek, QueryKey: qk})
	if r == nil {
		t.Fatal("relay not created")
	}
	t.Cleanup(r.closeAll)
	return r
}

// The tunnel and the messenger share the cell encoding but not the key space,
// so a shared domain would route cells to the wrong subsystem, where they
// simply fail to open. Reject the overlap at wiring time instead.
func TestSetMTProtoRelayRejectsDomainCollisions(t *testing.T) {
	t.Run("feed domain", func(t *testing.T) {
		s := newTestDNSServer(t, "t.example.com")
		if err := s.SetMTProtoRelay(relayOn(t, []string{"t.example.com"})); err == nil {
			t.Fatal("accepted an mtproto domain equal to the feed domain")
		}
	})
	t.Run("extra feed domain", func(t *testing.T) {
		s := newTestDNSServer(t, "t.example.com")
		s.SetExtraDomains([]string{"x.example.net"})
		if err := s.SetMTProtoRelay(relayOn(t, []string{"x.example.net"})); err == nil {
			t.Fatal("accepted an mtproto domain equal to an extra feed domain")
		}
	})
	t.Run("chat domain", func(t *testing.T) {
		s := newTestDNSServer(t, "t.example.com")
		s.chatDomains = []string{"c.example.com"}
		if err := s.SetMTProtoRelay(relayOn(t, []string{"C.EXAMPLE.COM"})); err == nil {
			t.Fatal("accepted an mtproto domain equal to a chat domain (case-insensitively)")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		s := newTestDNSServer(t, "t.example.com")
		if err := s.SetMTProtoRelay(relayOn(t, []string{"m.example.com", "m.example.com"})); err == nil {
			t.Fatal("accepted the same mtproto domain twice")
		}
	})
	t.Run("nothing usable", func(t *testing.T) {
		s := newTestDNSServer(t, "t.example.com")
		if err := s.SetMTProtoRelay(relayOn(t, []string{"  ", ""})); err == nil {
			t.Fatal("accepted a relay with no usable domain")
		}
		if s.mtproto != nil {
			t.Fatal("relay attached despite the error")
		}
	})
}

func TestSetMTProtoRelayAttaches(t *testing.T) {
	s := newTestDNSServer(t, "t.example.com")
	if err := s.SetMTProtoRelay(relayOn(t, []string{"m.example.com", " n.example.com. "})); err != nil {
		t.Fatal(err)
	}
	if s.mtproto == nil {
		t.Fatal("relay not attached")
	}
	for _, d := range []string{"m.example.com", "n.example.com"} {
		if !s.isMTProtoDomain(d) {
			t.Fatalf("%s not recognised as an mtproto domain", d)
		}
	}
	if s.isMTProtoDomain("t.example.com") {
		t.Fatal("feed domain treated as mtproto")
	}
}

// A tunnel sub-domain nested under the feed domain must win the longest-suffix
// match, or its cells would be run through DecodeQuery and refused.
func TestMatchDomainPrefersNestedMTProtoDomain(t *testing.T) {
	s := newTestDNSServer(t, "example.com")
	if err := s.SetMTProtoRelay(relayOn(t, []string{"m.example.com"})); err != nil {
		t.Fatal(err)
	}
	got, ok := s.matchDomain("abc.m.example.com.")
	if !ok {
		t.Fatal("no domain matched")
	}
	if got != "m.example.com" {
		t.Fatalf("matched %q, want the mtproto sub-domain", got)
	}
	if !s.isMTProtoDomain(got) {
		t.Fatal("match not routed to the relay")
	}
	// A plain feed name still belongs to the feed.
	if got, _ := s.matchDomain("abc.example.com."); got != "example.com" {
		t.Fatalf("feed query matched %q", got)
	}
}

// Queries on a tunnel domain must be counted separately, not as chat or feed.
func TestMTProtoQueriesCountedSeparately(t *testing.T) {
	rep := &hourlyFetchReport{
		perChannel:  map[uint16]*channelFetchStats{},
		perResolver: map[string]int64{},
		perDomain:   map[string]int64{},
	}
	recordReportQuery(rep, reportEvent{mtproto: true, domain: "m.example.com", resolver: "1.1.1.1"})
	recordReportQuery(rep, reportEvent{chat: true, domain: "c.example.com", resolver: "1.1.1.1"})

	if rep.mtprotoQueries != 1 {
		t.Fatalf("mtprotoQueries = %d, want 1", rep.mtprotoQueries)
	}
	if rep.chatQueries != 1 {
		t.Fatalf("chatQueries = %d, want 1", rep.chatQueries)
	}
	if rep.perChannel[0] != nil {
		t.Fatal("a tunnel query was also booked against a feed channel")
	}
}

// The capability bit is what lets a client mark a config as tunnel-capable
// without probing it, so it has to survive the real metadata encoding -- and
// it must be independent of the chat bit.
func TestFeedAdvertisesMTProtoIndependentlyOfChat(t *testing.T) {
	readMeta := func(f *Feed) *protocol.Metadata {
		t.Helper()
		data, err := f.GetBlock(protocol.MetadataChannel, 0)
		if err != nil {
			t.Fatal(err)
		}
		meta, err := protocol.ParseMetadata(data)
		if err != nil {
			t.Fatal(err)
		}
		return meta
	}

	feed := NewFeed([]string{"c"})
	if m := readMeta(feed); m.MTProtoAvailable {
		t.Fatal("a fresh server advertises the tunnel")
	}

	feed.SetMTProtoAvailable(true)
	m := readMeta(feed)
	if !m.MTProtoAvailable {
		t.Fatal("tunnel not advertised after enabling it")
	}
	if m.ChatAvailable {
		t.Fatal("enabling the tunnel also advertised the messenger")
	}

	// Chat on top: both bits coexist.
	feed.SetChatAvailable(true)
	if m := readMeta(feed); !m.MTProtoAvailable || !m.ChatAvailable {
		t.Fatalf("both should be advertised, got mtproto=%v chat=%v", m.MTProtoAvailable, m.ChatAvailable)
	}

	// And turning the tunnel off must not take chat with it.
	feed.SetMTProtoAvailable(false)
	if m := readMeta(feed); m.MTProtoAvailable || !m.ChatAvailable {
		t.Fatalf("after disabling the tunnel: mtproto=%v chat=%v", m.MTProtoAvailable, m.ChatAvailable)
	}
}

// Routing is not enough: a domain the mux never registers is answered REFUSED
// before handleQuery runs, so every tunnel sub-domain has to be in zones().
func TestZonesCoverEveryRoutedDomain(t *testing.T) {
	s := newTestDNSServer(t, "t.example.com")
	s.SetExtraDomains([]string{"x.example.net"})
	s.chatDomains = []string{"c.example.com"}
	if err := s.SetMTProtoRelay(relayOn(t, []string{"m.example.com", "n.example.com"})); err != nil {
		t.Fatal(err)
	}

	zones := map[string]bool{}
	for _, z := range s.zones() {
		zones[z] = true
	}
	for _, want := range []string{
		"t.example.com", "x.example.net", // feed
		"c.example.com",                  // chat
		"m.example.com", "n.example.com", // tunnel
	} {
		if !zones[want] {
			t.Errorf("%s is routed but not served: the mux would answer REFUSED", want)
		}
	}
	// Every zone must also be a domain the router recognises.
	for _, z := range s.zones() {
		if _, ok := s.matchDomain("abc." + z + "."); !ok {
			t.Errorf("zone %s is served but the router does not match it", z)
		}
	}
}
