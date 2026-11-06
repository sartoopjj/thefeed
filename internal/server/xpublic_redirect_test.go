package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const redirectTestRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
  <item>
    <title>hello</title>
    <link>https://x.com/test/status/1930000000000000001</link>
    <guid>https://x.com/test/status/1930000000000000001</guid>
    <description><![CDATA[<p>hello</p>]]></description>
    <pubDate>Mon, 30 Mar 2026 04:45:00 +0000</pubDate>
  </item>
</channel></rss>`

// newRedirectReader returns a reader for one instance whose client trusts the
// test TLS server.
func newRedirectReader(t *testing.T, instance string, tlsSrv *httptest.Server) *XPublicReader {
	t.Helper()
	xr := NewXPublicReader([]string{"test"}, NewFeed(nil), 15, 1, instance)
	xr.client.Transport = tlsSrv.Client().Transport
	return xr
}

// Instances configured as http:// (including the built-in fallback) answer
// with a redirect to their https:// address. Refusing every redirect made
// those instances look dead.
func TestXRSSFollowsRedirectToHTTPS(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(redirectTestRSS))
	}))
	defer tlsSrv.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tlsSrv.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer plain.Close()

	msgs, _, err := newRedirectReader(t, plain.URL, tlsSrv).fetchAccount(context.Background(), "test")
	if err != nil {
		t.Fatalf("a 301 to https was not followed: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hello" {
		t.Fatalf("got %+v", msgs)
	}
}

// A redirect may only lead to https: an instance must not be able to bounce
// the server to a plain-http address, such as an internal service.
func TestXRSSRefusesRedirectToPlainHTTP(t *testing.T) {
	hit := false
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(redirectTestRSS))
	}))
	defer inner.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+r.URL.Path, http.StatusFound)
	}))
	defer tlsSrv.Close()

	if _, _, err := newRedirectReader(t, tlsSrv.URL, tlsSrv).fetchAccount(context.Background(), "test"); err == nil {
		t.Fatal("followed a redirect to plain http")
	}
	if hit {
		t.Fatal("the plain-http target was requested")
	}
}

// A loop ends after a few hops instead of spinning.
func TestXRSSRedirectLoopEnds(t *testing.T) {
	hops := 0
	var tlsSrv *httptest.Server
	tlsSrv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, tlsSrv.URL+r.URL.Path, http.StatusFound)
	}))
	defer tlsSrv.Close()

	if _, _, err := newRedirectReader(t, tlsSrv.URL, tlsSrv).fetchAccount(context.Background(), "test"); err == nil {
		t.Fatal("a redirect loop reported success")
	}
	if hops > xRSSMaxRedirects+1 {
		t.Fatalf("followed %d hops", hops)
	}
}
