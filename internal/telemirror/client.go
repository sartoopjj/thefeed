package telemirror

import (
	"context"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"crypto/x509"
	"errors"
	utls "github.com/refraction-networking/utls"
)

const (
	minRequestInterval = 1500 * time.Millisecond
	maxBodySize        = 5 << 20
	dialTimeout        = 8 * time.Second
	tlsTimeout         = 8 * time.Second
	requestTimeout     = 20 * time.Second
)

// proxyHosts are the Google-Translate proxy hostnames for Telegram's
// public web preview, tried in order. All serve the identical
// /s/<channel> widget HTML. On a per-host 404 the fetch falls through to
// the next entry; t-me stays last as a fallback.
var proxyHosts = []string{
	"telegram-me.translate.goog",  // telegram.me
	"telegram-dog.translate.goog", // telegram.dog
	"t-me.translate.goog",         // t.me
}

// sniUseHost is a proxyAttempt.sni sentinel meaning "use the request's
// own host as the TLS SNI" — for the non-fronted and direct paths, where
// the SNI must match the host actually being addressed.
const sniUseHost = ""

// fingerprint identifies a TLS ClientHello shape. Restrictive DPI
// systems fail to match Go's stock fingerprint; we have to mimic
// something they already permit. We try multiple presets per request
// because we can't know in advance which one is currently whitelisted.
type fingerprint struct {
	name string
	id   utls.ClientHelloID
	// nodeTLS12 == true → use the custom Node-mimic spec (TLS 1.2 only,
	// exact cipher list from the reference). HelloChrome_Auto sends a
	// TLS 1.3-capable ClientHello which the firewall may drop on sight.
	nodeTLS12 bool
}

var fingerprints = []fingerprint{
	// First — TLS 1.2-only with the reference's exact cipher list.
	// This is the closest thing to the bytes Node's OpenSSL emits.
	{name: "node-tls12", nodeTLS12: true},
	// Real Chrome (TLS 1.3 capable). Most likely to match a generic
	// "browser" allow-list in the firewall.
	{name: "chrome", id: utls.HelloChrome_Auto},
	// Firefox is sometimes whitelisted separately.
	{name: "firefox", id: utls.HelloFirefox_Auto},
	// Older Chrome (TLS 1.2-only era).
	{name: "chrome-72", id: utls.HelloChrome_72},
	// iOS Safari — different fingerprint family entirely.
	{name: "ios", id: utls.HelloIOS_Auto},
}

// proxyAttempt is one (IP, SNI, fingerprint, sl, tl) combination.
// When sni differs from proxyHost it's domain fronting: TLS handshake
// looks like a connection to the (whitelisted) front host while the
// HTTP layer talks to the real one.
type proxyAttempt struct {
	ip  string
	sni string
	fp  fingerprint
	sl  string
	tl  string
}

// SNI host used for domain fronting. www.google.com is widely
// reachable from restricted networks while translate.google.com /
// the *.translate.goog proxy host may be filtered — fronting via
// www.google.com is the only path that passes TLS-SNI inspection on
// those networks.
const frontSNI = "www.google.com"

var proxyAttempts = []proxyAttempt{
	// 1) www.google.com fronting on Google IPs — most likely to pass DPI.
	{ip: "216.239.38.120", sni: frontSNI, fp: fingerprints[0], sl: "auto", tl: "fa"}, // node-tls12 + front
	{ip: "216.239.38.120", sni: frontSNI, fp: fingerprints[1], sl: "auto", tl: "fa"}, // chrome + front
	{ip: "142.250.191.196", sni: frontSNI, fp: fingerprints[1], sl: "fa", tl: "en"},  // chrome + front
	{ip: "142.250.184.196", sni: frontSNI, fp: fingerprints[2], sl: "ru", tl: "en"},  // firefox + front
	{ip: "142.250.74.14", sni: frontSNI, fp: fingerprints[1], sl: "ar", tl: "en"},    // chrome + front

	// 2) Pure (non-fronted) — for environments where the translate.goog SNI is allowed.
	{ip: "216.239.38.120", sni: sniUseHost, fp: fingerprints[0], sl: "auto", tl: "fa"},
	{ip: "216.239.38.120", sni: sniUseHost, fp: fingerprints[1], sl: "auto", tl: "fa"},

	// 3) Direct (DNS lookup) with Chrome — the path that's known to work
	// with VPN. Sits last because it depends on DNS.
	{ip: "", sni: sniUseHost, fp: fingerprints[1], sl: "auto", tl: "fa"},
}

// User-Agent list copied verbatim from the reference's `this.userAgents`.
var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:126.0) Gecko/20100101 Firefox/126.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:125.0) Gecko/20100101 Firefox/125.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:124.0) Gecko/20100101 Firefox/124.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:127.0) Gecko/20100101 Firefox/127.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:126.0) Gecko/20100101 Firefox/126.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:125.0) Gecko/20100101 Firefox/125.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.2592.81",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36 Edg/125.0.2592.102",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36 Edg/124.0.2478.67",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0",
	"Mozilla/5.0 (X11; Linux x86_64; rv:126.0) Gecko/20100101 Firefox/126.0",
	"Mozilla/5.0 (Linux; Android 13; SM-G998B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
}

// Client mirrors HttpClient from teleMirror's http-client.js.
type Client struct {
	rateMu      sync.Mutex
	lastRequest time.Time

	// Index into proxyAttempts of the last attempt that succeeded.
	// Subsequent fetches try this first so image loads (parallel from
	// the browser) don't each pay the cost of walking through every
	// failed fronting variant.
	stickyMu      sync.Mutex
	stickyAttempt int
}

func NewClient() *Client { return &Client{stickyAttempt: -1} }

// orderedAttempts returns proxyAttempts with the last successful one
// moved to the front. Used to short-circuit the fronting walk on
// repeat fetches (e.g. parallel image loads after the channel HTML
// already established a working path).
func (c *Client) orderedAttempts() []proxyAttempt {
	c.stickyMu.Lock()
	idx := c.stickyAttempt
	c.stickyMu.Unlock()
	if idx < 0 || idx >= len(proxyAttempts) {
		return proxyAttempts
	}
	out := make([]proxyAttempt, 0, len(proxyAttempts))
	out = append(out, proxyAttempts[idx])
	for i, ap := range proxyAttempts {
		if i != idx {
			out = append(out, ap)
		}
	}
	return out
}

func (c *Client) markSuccess(idx int) {
	c.stickyMu.Lock()
	c.stickyAttempt = idx
	c.stickyMu.Unlock()
}

// FetchHTML returns the rendered widget HTML for the username.
func (c *Client) FetchHTML(ctx context.Context, username string) (string, error) {
	return c.fetchHTMLWithBefore(ctx, username, 0)
}

// FetchHTMLBefore fetches the widget filtered to posts strictly older
// than beforeID. Used for "Load older" pagination.
func (c *Client) FetchHTMLBefore(ctx context.Context, username string, beforeID int) (string, error) {
	if beforeID <= 0 {
		return c.FetchHTML(ctx, username)
	}
	return c.fetchHTMLWithBefore(ctx, username, beforeID)
}

func (c *Client) fetchHTMLWithBefore(ctx context.Context, username string, beforeID int) (string, error) {
	username = SanitizeUsername(username)
	if username == "" {
		return "", ErrEmptyUsername
	}
	var lastErr error
	for _, host := range proxyHosts {
		body, edgeBlocked, err := c.fetchHTMLFromHost(ctx, host, username, beforeID)
		if err == nil {
			return body, nil
		}
		lastErr = err
		// A 404 from Google's edge means this proxy host is no longer
		// served; another alias may still work, so try the next host. Any
		// other failure is transport-level — a different host on the same
		// network won't fare better, so stop.
		if !edgeBlocked {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("telemirror: all attempts exhausted")
	}
	return "", lastErr
}

// fetchHTMLFromHost walks the transport attempts for a single proxy host.
// edgeBlocked is true when Google's edge answered but returned 404 (the
// host is no longer proxied), signalling the caller to try the next host.
func (c *Client) fetchHTMLFromHost(ctx context.Context, host, username string, beforeID int) (body string, edgeBlocked bool, err error) {
	var lastErr error
	for i, ap := range c.orderedAttempts() {
		if i > 0 {
			select {
			case <-ctx.Done():
				return "", false, ctx.Err()
			case <-time.After(1 * time.Second):
			}
		}
		if err := c.waitForRate(ctx); err != nil {
			return "", false, err
		}
		ua := userAgents[mrand.IntN(len(userAgents))]
		b, status, err := c.do(ctx, ap, host, username, ua, beforeID)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d (%s) host %s: %w", i+1, ap.label(), host, err)
			continue
		}
		if status == http.StatusOK && b != "" {
			c.markSuccess(attemptIndex(ap))
			return b, false, nil
		}
		if status >= 300 && status < 400 {
			// A live public channel is served 200 directly; the proxy only
			// redirects (302 → t.me) when the channel doesn't exist, was
			// renamed, or is private. Every host agrees, so report
			// not-found rather than walking on or chasing the dead host.
			return "", false, ErrChannelNotFound
		}
		if status == http.StatusNotFound {
			// Google's edge routes by Host header before applying the
			// per-host 404, so every transport attempt would return the
			// same 404 — no point walking the rest for this host.
			return "", true, fmt.Errorf("host %s not served (status 404)", host)
		}
		lastErr = fmt.Errorf("attempt %d (%s) host %s status %d", i+1, ap.label(), host, status)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("telemirror: all attempts exhausted for host %s", host)
	}
	return "", false, lastErr
}

func attemptIndex(target proxyAttempt) int {
	for i, ap := range proxyAttempts {
		if ap == target {
			return i
		}
	}
	return -1
}

func (ap proxyAttempt) label() string {
	ip := ap.ip
	if ip == "" {
		ip = "direct"
	}
	suffix := ""
	if ap.sni == frontSNI {
		suffix = " front=" + ap.sni
	}
	return ip + "/" + ap.fp.name + suffix
}

// stripH2 removes "h2" from any ALPN extension in spec. We can only
// speak HTTP/1.1 over uTLS reliably (Go's net/http needs a *tls.Conn
// to upgrade to HTTP/2, which uTLS doesn't provide), so we have to
// stop the server from picking h2.
func stripH2(spec *utls.ClientHelloSpec) {
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			filtered := alpn.AlpnProtocols[:0]
			for _, p := range alpn.AlpnProtocols {
				if p != "h2" {
					filtered = append(filtered, p)
				}
			}
			if len(filtered) == 0 {
				filtered = []string{"http/1.1"}
			}
			alpn.AlpnProtocols = filtered
		}
	}
}

// presetSpec extracts a ClientHelloSpec from a uTLS preset and patches
// out h2. We use HelloCustom + this spec everywhere so net/http
// doesn't end up trying to read HTTP/2 frames as HTTP/1.1.
func presetSpec(id utls.ClientHelloID) (*utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(id)
	if err != nil {
		return nil, err
	}
	stripH2(&spec)
	return &spec, nil
}

// nodeTLS12Spec is a ClientHelloSpec that mimics Node.js with
// `secureProtocol: 'TLSv1_2_method'` plus the exact cipher list from
// teleMirror's createCustomAgent. TLS 1.2 only — no supported_versions
// extension, no TLS 1.3 hints. Restrictive DPI systems seem to allow
// this shape because Node's OpenSSL produces it.
func nodeTLS12Spec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS12,
		CipherSuites: []uint16{
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
		},
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519, utls.CurveP256, utls.CurveP384, utls.CurveP521,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}}, // uncompressed
			&utls.SessionTicketExtension{},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				utls.PSSWithSHA384,
				utls.PKCS1WithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA512,
				utls.PKCS1WithSHA1,
				utls.ECDSAWithSHA1,
			}},
			&utls.SCTExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
		},
	}
}

func (c *Client) waitForRate(ctx context.Context) error {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	wait := minRequestInterval - time.Since(c.lastRequest)
	if wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	c.lastRequest = time.Now()
	return nil
}

// verifyAgainstName builds the certificate check for a fronted connection.
//
// The name checked is the SNI, which on a fronted attempt is the front
// (www.google.com) and not the host the content comes from. That is not a
// shortcut, it is the only name that CAN be checked: Google's frontend picks
// the certificate from the SNI, so a fronted connection is handed a
// www.google.com certificate and nothing else. Demanding one for
// *.translate.goog fails every fronted attempt -- which is to say, every
// attempt that works on a censored network.
//
// What this does buy is the thing that matters: only someone holding a
// certificate a public CA issued for the front can complete the handshake.
// The censor on the path cannot, so they can no longer answer with a page of
// their own, which is what no verification at all allowed. Beyond that the
// transport trusts Google's frontend to route the Host header to the real
// host -- the same trust the Mirror already places in it to proxy Telegram at
// all -- so the page itself is still treated as hostile and sanitised.
func verifyAgainstName(name string) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("telemirror: server sent no certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, der := range rawCerts {
			c, err := x509.ParseCertificate(der)
			if err != nil {
				return fmt.Errorf("telemirror: bad certificate: %w", err)
			}
			certs = append(certs, c)
		}
		inters := x509.NewCertPool()
		for _, c := range certs[1:] {
			inters.AddCert(c)
		}
		_, err := certs[0].Verify(x509.VerifyOptions{
			DNSName:       name,
			Intermediates: inters,
		})
		if err != nil {
			return fmt.Errorf("telemirror: certificate is not valid for %s: %w", name, err)
		}
		return nil
	}
}

// dialTLSFor returns a DialTLSContext closure preconfigured for the given
// proxy attempt. Connect → uTLS handshake with the chosen fingerprint and
// SNI (which may differ from the request Host for domain fronting).
func dialTLSFor(ap proxyAttempt, host string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
		target := addr
		if ap.ip != "" {
			target = net.JoinHostPort(ap.ip, "443")
		}
		rawConn, err := d.DialContext(ctx, network, target)
		if err != nil {
			return nil, err
		}
		sni := ap.sni
		if sni == sniUseHost {
			sni = host
		}
		// Certificates ARE verified -- see verifyAgainstName for which name and
		// why. Skipping verification entirely handed the connection to
		// whoever is on the path, which here is the censor, the one adversary
		// this transport exists to resist: they could answer with any page
		// they liked and the renderer puts it in the UI's origin.
		//
		// InsecureSkipVerify stays on because uTLS would otherwise reject the
		// chain itself; the check below replaces it rather than relaxing it.
		cfg := &utls.Config{
			ServerName:            sni,
			InsecureSkipVerify:    true,
			NextProtos:            []string{"http/1.1"},
			VerifyPeerCertificate: verifyAgainstName(sni),
		}
		var spec *utls.ClientHelloSpec
		if ap.fp.nodeTLS12 {
			spec = nodeTLS12Spec()
		} else {
			spec, err = presetSpec(ap.fp.id)
			if err != nil {
				_ = rawConn.Close()
				return nil, fmt.Errorf("load spec %s: %w", ap.fp.name, err)
			}
		}
		tlsConn := utls.UClient(rawConn, cfg, utls.HelloCustom)
		if err := tlsConn.ApplyPreset(spec); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("apply spec %s: %w", ap.fp.name, err)
		}
		hsCtx, cancel := context.WithTimeout(ctx, tlsTimeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(hsCtx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		return tlsConn, nil
	}
}

// transportFor returns a one-shot http.Transport for an attempt against
// the given host (used to resolve the SNI sentinel and, when non-fronted,
// as the TLS SNI).
func transportFor(ap proxyAttempt, host string) *http.Transport {
	return &http.Transport{
		DialTLSContext:        dialTLSFor(ap, host),
		ResponseHeaderTimeout: requestTimeout,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          1,
		IdleConnTimeout:       30 * time.Second,
	}
}

// setBrowserHeaders applies the verbatim header set from teleMirror's
// baseHeaders (plus google-method overrides for the Translate path).
func setBrowserHeaders(req *http.Request, ua string, fronted bool) {
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,fa;q=0.8")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Cache-Control", "no-cache")
	if fronted {
		req.Header.Set("Referer", "https://translate.google.com/")
		req.Header.Set("Origin", "https://translate.google.com")
	}
}

// followPreviewRedirect is the widget fetch's redirect policy. A live
// channel's /s/ preview is served with 200 directly; the proxy redirects
// only when there is no public preview (the channel is missing, private,
// or has its web preview disabled), bouncing to the join page — which
// drops the /s/ path and/or leaves the host, and carries no widget to
// scrape. So we follow a redirect only when it stays on the same host and
// keeps the /s/ preview path (a canonical move that still yields a
// widget); every other redirect stops here with its 3xx status, which
// fetchHTMLFromHost maps to ErrChannelNotFound. This preserves any
// legitimate same-preview redirect while never chasing the dead t.me host.
func followPreviewRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 0 &&
		req.URL.Host == via[0].URL.Host &&
		strings.HasPrefix(req.URL.Path, "/s/") {
		return nil
	}
	return http.ErrUseLastResponse
}

func (c *Client) do(ctx context.Context, ap proxyAttempt, host, username, ua string, beforeID int) (string, int, error) {
	url := fmt.Sprintf(
		"https://%s/s/%s?_x_tr_sl=%s&_x_tr_tl=%s&_x_tr_hl=en&_x_tr_pto=wapp",
		host, username, ap.sl, ap.tl,
	)
	if beforeID > 0 {
		url += fmt.Sprintf("&before=%d", beforeID)
	}

	transport := transportFor(ap, host)
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{
		Transport:     transport,
		Timeout:       requestTimeout,
		CheckRedirect: followPreviewRedirect,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Host = host
	setBrowserHeaders(req, ua, ap.ip != "")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
	if err != nil {
		return "", resp.StatusCode, err
	}
	if int64(len(body)) > maxBodySize {
		return "", resp.StatusCode, fmt.Errorf("response exceeds %d bytes", maxBodySize)
	}
	return string(body), resp.StatusCode, nil
}

// FetchURL fetches an arbitrary URL through the same fronting/uTLS
// attempts used for the channel widget, but with the request Host set
// to the URL's actual host (not proxyHost) so Google's edge routes it
// to the right backend. Returns the bytes plus the upstream
// Content-Type so the caller can stream it back to the browser.
//
// Used to proxy translate.goog-rewritten image URLs (channel avatars,
// post photos/videos) without each one having to go through DNS.
func (c *Client) FetchURL(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, "", fmt.Errorf("telemirror: bad url %q", rawURL)
	}
	hostHeader := u.Host

	var lastErr error
	for i, ap := range c.orderedAttempts() {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		body, ctype, status, err := c.fetchOnce(ctx, ap, rawURL, hostHeader)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d (%s): %w", i+1, ap.label(), err)
			continue
		}
		if status == http.StatusOK {
			c.markSuccess(attemptIndex(ap))
			return body, ctype, nil
		}
		lastErr = fmt.Errorf("attempt %d (%s) status %d", i+1, ap.label(), status)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("telemirror: all attempts exhausted")
	}
	return nil, "", lastErr
}

func (c *Client) fetchOnce(ctx context.Context, ap proxyAttempt, rawURL, hostHeader string) ([]byte, string, int, error) {
	transport := transportFor(ap, hostHeader)
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: requestTimeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Host = hostHeader
	ua := userAgents[mrand.IntN(len(userAgents))]
	setBrowserHeaders(req, ua, ap.ip != "")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
	if err != nil {
		return nil, resp.Header.Get("Content-Type"), resp.StatusCode, err
	}
	if int64(len(body)) > maxBodySize {
		return nil, resp.Header.Get("Content-Type"), resp.StatusCode, fmt.Errorf("response exceeds %d bytes", maxBodySize)
	}
	return body, resp.Header.Get("Content-Type"), resp.StatusCode, nil
}
