package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The API must answer only at an address this machine is actually reachable
// at -- in EVERY binding, not just the default one.
//
// Anything else is DNS rebinding: the user opens any page, the attacker's
// domain re-resolves to this server, and every request that page makes is
// same-origin as far as the browser is concerned. Sec-Fetch-Site says
// "same-origin" and the cross-site guard waves it through; GETs were never
// checked there at all, and GET /api/chat/seed returns the messenger recovery
// code while GET /api/config returns the feed passphrase.
//
// The attack always arrives under a NAME the attacker owns, never under this
// machine's own address -- which is why the check still works when the server
// is deliberately exposed to the LAN.
func TestHostGuardRejectsRebinding(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "0.0.0.0", "192.168.1.5", ""} {
		reached := false
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
		guard := hostGuard(inner, bind)

		for _, method := range []string{http.MethodGet, http.MethodPost} {
			reached = false
			req := httptest.NewRequest(method, "/api/chat/seed", nil)
			req.Host = "attacker.example:18099"
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			rec := httptest.NewRecorder()
			guard.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden || reached {
				t.Errorf("bind %q: %s from a rebound page got %d (reached=%v), want 403",
					bind, method, rec.Code, reached)
			}
		}

		// Loopback always works: that is how the app itself is opened.
		for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
			reached = false
			req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			guard.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !reached {
				t.Errorf("bind %q: Host %q got %d, want 200", bind, host, rec.Code)
			}
		}
	}
}

// Exposing to the LAN on purpose must keep working -- reached by this
// machine's own address, which is how a phone on the same network opens it.
func TestHostGuardAllowsTheLANAddressItIsBoundTo(t *testing.T) {
	reached := false
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })

	// An explicit --host is answered at that address.
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Host = "192.168.1.5:8080"
	rec := httptest.NewRecorder()
	hostGuard(inner, "192.168.1.5").ServeHTTP(rec, req)
	if !reached {
		t.Error("--host 192.168.1.5 refused its own address")
	}

	// A wildcard bind answers at any address this machine actually has.
	var lanIP string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				lanIP = ipn.IP.String()
				break
			}
		}
	}
	if lanIP == "" {
		t.Skip("no non-loopback interface on this machine")
	}
	reached = false
	req = httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Host = lanIP + ":8080"
	rec = httptest.NewRecorder()
	hostGuard(inner, "0.0.0.0").ServeHTTP(rec, req)
	if !reached {
		t.Errorf("a wildcard bind refused this machine's own address %s", lanIP)
	}
}

// A name the operator reaches the server by cannot be inferred, so it is
// stated -- and only takes effect when stated.
func TestHostGuardExtraHosts(t *testing.T) {
	reached := false
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Host = "feed.home.lan:8080"
	rec := httptest.NewRecorder()
	hostGuard(inner, "0.0.0.0").ServeHTTP(rec, req)
	if reached {
		t.Error("an unlisted name was accepted")
	}

	t.Setenv("THEFEED_EXTRA_HOSTS", "feed.home.lan, other.example")
	reached = false
	rec = httptest.NewRecorder()
	hostGuard(inner, "0.0.0.0").ServeHTTP(rec, req)
	if !reached {
		t.Error("a listed name was still refused")
	}
}

func TestHostAllowedMatching(t *testing.T) {
	allowed := allowedHosts("127.0.0.1")
	for _, h := range []string{
		"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "127.0.0.1",
		"localhost", "app.localhost:38000", "127.0.0.53:8080", "::1",
	} {
		if !hostAllowed(h, allowed) {
			t.Errorf("hostAllowed(%q) = false, want true", h)
		}
	}
	for _, h := range []string{
		"attacker.example:18099", "evil.com", "", "192.168.1.5:8080",
		"10.0.0.1:8080",
		// The suffix trap: a name ENDING in the allowed one is not it.
		"localhost.evil.com:8080", "notlocalhost", "0.0.0.0:8080",
	} {
		if hostAllowed(h, allowed) {
			t.Errorf("hostAllowed(%q) = true, want false", h)
		}
	}
}

// Behind a reverse proxy that forwards the original name -- nginx with
// `proxy_set_header Host $host;` -- a correct setup hits the guard. The
// refusal has to say what to do, or it is an unexplained 403 on a working
// deployment.
func TestHostGuardRefusalExplainsItself(t *testing.T) {
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Host = "feed.example.com"
	rec := httptest.NewRecorder()
	hostGuard(inner, "127.0.0.1").ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "THEFEED_EXTRA_HOSTS") {
		t.Errorf("the refusal does not name the setting that fixes it: %q", body)
	}

	// And with the name listed, the same request goes through.
	t.Setenv("THEFEED_EXTRA_HOSTS", "feed.example.com")
	reached := false
	rec = httptest.NewRecorder()
	hostGuard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }),
		"127.0.0.1").ServeHTTP(rec, req)
	if !reached {
		t.Error("the documented fix does not work")
	}
}
