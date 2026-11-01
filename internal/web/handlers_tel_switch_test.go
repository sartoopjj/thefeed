package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sartoopjj/thefeed/internal/tgbridge"
)

// switchServer is a hub with two tunnel configs, A live; every connect it
// starts fails at once, so nothing reaches the network.
func switchServer(t *testing.T) (*Server, *telHub) {
	t.Helper()
	s := newTelTestServer(t)
	if err := s.saveProfiles(&ProfileList{Profiles: []Profile{
		{Nickname: "a", Config: Config{Domain: "a.example.com", ServerKey: "ka"}},
		{Nickname: "b", Config: Config{Domain: "b.example.com", ServerKey: "kb"}},
	}}); err != nil {
		t.Fatal(err)
	}
	h := s.tel
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	h.mu.Lock()
	h.ctx = dead
	h.bridge = &tgbridge.Bridge{}
	h.serverKey = chatServerKey("a.example.com")
	h.mu.Unlock()
	return s, h
}

func connectTo(s *Server, domain string) map[string]any {
	rec := httptest.NewRecorder()
	s.handleTelConnect(rec, httptest.NewRequest(http.MethodPost, "/api/tel/connect",
		strings.NewReader(`{"server":"`+domain+`"}`)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// Picking another config while one is up switches to it; picking the same one
// leaves the live link alone.
func TestTelConnectToAnotherConfigSwitches(t *testing.T) {
	s, h := switchServer(t)
	live := h.bridge
	if out := connectTo(s, "a.example.com"); out["already"] != true {
		t.Fatalf("same config: %v", out)
	}
	if h.bridge != live {
		t.Fatal("the same config dropped the live link")
	}
	before := h.connGen
	if out := connectTo(s, "b.example.com"); out["connecting"] != true || out["already"] == true {
		t.Fatalf("another config: %v", out)
	}
	h.mu.Lock()
	bridge, gen := h.bridge, h.connGen
	h.mu.Unlock()
	if bridge != nil {
		t.Fatal("the old link is still the hub's")
	}
	if gen == before {
		t.Fatal("the old link's attempt was not superseded")
	}
}

// A second tap on a config being connected to does not start another attempt.
func TestTelConnectSameConfigWhileConnecting(t *testing.T) {
	s, h := switchServer(t)
	h.mu.Lock()
	h.bridge, h.serverKey = nil, ""
	h.connecting, h.connectingKey = true, chatServerKey("b.example.com")
	gen := h.connGen
	h.mu.Unlock()
	if out := connectTo(s, "b.example.com"); out["connecting"] != true {
		t.Fatalf("%v", out)
	}
	if h.connGen != gen {
		t.Fatal("a repeated tap restarted the attempt")
	}
}

// Only the current attempt may clear the connect flags.
func TestTelSupersededAttemptLeavesFlags(t *testing.T) {
	_, h := switchServer(t)
	h.mu.Lock()
	h.connecting, h.connectingKey, h.connGen = true, "b", 5
	h.mu.Unlock()
	if h.connectDone(4) {
		t.Fatal("an old attempt finished the new one")
	}
	if !h.connecting || h.connectingKey != "b" {
		t.Fatal("an old attempt cleared the new one's flags")
	}
	if !h.connectDone(5) || h.connecting || h.connectingKey != "" {
		t.Fatal("the current attempt did not finish")
	}
}

// Cancelling a connect ends it for the hub at once; its own cleanup no longer
// runs, so nothing else would.
func TestTelCancelClearsConnecting(t *testing.T) {
	_, h := switchServer(t)
	h.mu.Lock()
	h.connecting, h.connectingKey = true, "b"
	h.mu.Unlock()
	h.disconnect()
	if h.connecting || h.connectingKey != "" {
		t.Fatal("still connecting after a cancel")
	}
}

// A superseded attempt cannot install itself.
func TestTelStaleAttemptDoesNotConnect(t *testing.T) {
	_, h := switchServer(t)
	h.mu.Lock()
	gen := h.connGen
	h.mu.Unlock()
	err := h.connect(context.Background(), Config{Domain: "b.example.com"}, "b", gen+1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if h.connectCancel != nil {
		t.Fatal("a stale attempt took the cancel slot")
	}
}

// A call failing on a bridge already replaced must not take the new one down.
func TestTelDeadOldBridgeKeepsTheNewOne(t *testing.T) {
	_, h := switchServer(t)
	old, cur := &tgbridge.Bridge{}, h.bridge
	h.dropDeadBridge(&bridgeErr{old, tgbridge.ErrNotConnected})
	if h.bridge != cur {
		t.Fatal("an old bridge's failure dropped the current one")
	}
	h.dropDeadBridge(&bridgeErr{cur, tgbridge.ErrNotConnected})
	if h.bridge != nil {
		t.Fatal("the current bridge's own failure did not drop it")
	}
}

// Nor may it start a rebuild of the old config over a switch.
func TestTelRebuildSkipsAReplacedBridge(t *testing.T) {
	_, h := switchServer(t)
	h.mu.Lock()
	h.lastCfg, h.lastNick = Config{Domain: "a.example.com"}, "a"
	h.mu.Unlock()
	next := &tgbridge.Bridge{}
	err := h.callBridge(context.Background(), true, func(*tgbridge.Bridge) error {
		h.mu.Lock()
		h.bridge = next // the user switched while this call ran
		h.mu.Unlock()
		return tgbridge.ErrNotConnected
	})
	if !errors.Is(err, tgbridge.ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if h.bridge != next || h.connecting {
		t.Fatal("the old config was rebuilt over the switch")
	}
}

// Picking another config while one is still connecting switches too.
func TestTelConnectSwitchesWhileConnecting(t *testing.T) {
	s, h := switchServer(t)
	h.mu.Lock()
	h.bridge, h.serverKey = nil, ""
	h.connecting, h.connectingKey = true, chatServerKey("a.example.com")
	gen := h.connGen
	h.mu.Unlock()
	if out := connectTo(s, "b.example.com"); out["connecting"] != true {
		t.Fatalf("%v", out)
	}
	if h.connGen == gen {
		t.Fatal("the attempt for the other config was left running")
	}
}
