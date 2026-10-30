package telemirror

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// What the certificate check must accept and refuse, against the real network.
//
// The point of the check is to stop the censor on the path from answering
// instead of Google. The point of NOT checking the content host is that the
// fronted attempts are the only ones that work on a censored network, and
// Google picks the certificate from the SNI -- so demanding one for
// *.translate.goog refuses every single one of them.
//
// Both halves are asserted here because getting one right while breaking the
// other is exactly what happened: verifying against the host looked stricter
// and silently disabled domain fronting altogether.
//
// Needs the network; skipped when it is not there (CI sandboxes, offline).
func TestFrontedCertificateCheck(t *testing.T) {
	if os.Getenv("THEFEED_NET_TESTS") == "" {
		t.Skip("set THEFEED_NET_TESTS=1 to run checks that need the internet")
	}

	peek := func(ip, sni string) ([][]byte, error) {
		d := &net.Dialer{Timeout: 10 * time.Second}
		raw, err := d.Dial("tcp", net.JoinHostPort(ip, "443"))
		if err != nil {
			return nil, err
		}
		defer raw.Close()
		conn := tls.Client(raw, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		if err := conn.Handshake(); err != nil {
			return nil, err
		}
		defer conn.Close()
		var der [][]byte
		for _, c := range conn.ConnectionState().PeerCertificates {
			der = append(der, c.Raw)
		}
		return der, nil
	}

	// Every attempt in the real list, checked the way the dialer checks it.
	fronted, plain := 0, 0
	for _, ap := range proxyAttempts {
		if ap.ip == "" {
			continue // the direct path resolves by DNS; nothing fixed to dial
		}
		host := proxyHosts[0]
		sni := ap.sni
		if sni == sniUseHost {
			sni = host
		}
		der, err := peek(ap.ip, sni)
		if err != nil {
			t.Logf("skipping %s/%s: %v", ap.ip, sni, err)
			continue
		}
		if err := verifyAgainstName(sni)(der, nil); err != nil {
			t.Errorf("the dialer would refuse a genuine Google connection "+
				"(ip=%s sni=%s): %v -- this is the path that works under "+
				"censorship, so refusing it takes the Mirror offline", ap.ip, sni, err)
			continue
		}
		if ap.sni == sniUseHost {
			plain++
		} else {
			fronted++
		}

		// And the name the content comes from is NOT what the certificate
		// says, on a fronted attempt. Asserting this keeps the comment
		// honest: anyone "tightening" the check to the host has to see that
		// it cannot hold.
		if ap.sni != sniUseHost {
			if err := verifyAgainstName(host)(der, nil); err == nil {
				t.Errorf("ip=%s now serves a certificate valid for %s under a "+
					"%s SNI; the check could be tightened", ap.ip, host, sni)
			}
		}
	}
	if fronted == 0 {
		t.Error("no fronted attempt was reachable, so the main property went untested")
	}
	if plain == 0 {
		t.Log("no non-fronted attempt was reachable")
	}
}

// A certificate from somewhere else must be refused however well-formed it
// is -- that is the whole of what the check buys.
func TestCertificateForAnotherNameIsRefused(t *testing.T) {
	if os.Getenv("THEFEED_NET_TESTS") == "" {
		t.Skip("set THEFEED_NET_TESTS=1 to run checks that need the internet")
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	raw, err := d.Dial("tcp", "example.com:443")
	if err != nil {
		t.Skipf("no network: %v", err)
	}
	defer raw.Close()
	conn := tls.Client(raw, &tls.Config{ServerName: "example.com", InsecureSkipVerify: true})
	if err := conn.Handshake(); err != nil {
		t.Skipf("handshake: %v", err)
	}
	defer conn.Close()
	var der [][]byte
	for _, c := range conn.ConnectionState().PeerCertificates {
		der = append(der, c.Raw)
	}
	if err := verifyAgainstName(frontSNI)(der, nil); err == nil {
		t.Fatal("a certificate for example.com passed as www.google.com")
	}
}

// The failure modes that need no network: nothing presented, and garbage
// presented. Both reached the renderer before the check existed.
func TestCertificateCheckRejectsMissingAndMalformed(t *testing.T) {
	check := verifyAgainstName(frontSNI)
	if err := check(nil, nil); err == nil {
		t.Error("a connection with no certificate was accepted")
	}
	if err := check([][]byte{{0x30, 0x00, 0xff}}, nil); err == nil {
		t.Error("an unparseable certificate was accepted")
	} else if !strings.Contains(err.Error(), "bad certificate") {
		t.Errorf("malformed certificate reported as %v", err)
	}
	// A self-signed certificate for the right name must not pass either: the
	// chain has to reach a real CA, or the censor just issues their own.
	self := selfSigned(t, frontSNI)
	if err := check([][]byte{self}, nil); err == nil {
		t.Error("a self-signed certificate for the front name was accepted")
	}
}

func selfSigned(t *testing.T, name string) []byte {
	t.Helper()
	// Generated with the standard library so the test carries no fixture.
	der, err := makeSelfSigned(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x509.ParseCertificate(der); err != nil {
		t.Fatalf("generated certificate does not parse: %v", err)
	}
	return der
}

func makeSelfSigned(name string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
}
