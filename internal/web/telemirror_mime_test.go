package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Bytes from a mirror upstream must never be served as a document in this
// origin. image/svg+xml passes any "is it an image?" prefix check, and an SVG
// can carry <script>: opened directly rather than through an <img>, that
// script runs next to the messenger seed and the feed passphrase.
func TestTelemirrorMediaTypeIsSanitised(t *testing.T) {
	dangerous := []string{
		"image/svg+xml",
		"image/svg+xml; charset=utf-8",
		"IMAGE/SVG+XML",
		"text/html",
		"application/xhtml+xml",
		"image/svg",
		"application/xml",
	}
	for _, in := range dangerous {
		if got := sanitizeMediaType(in); got != "application/octet-stream" {
			t.Errorf("sanitizeMediaType(%q) = %q, want it rendered inert", in, got)
		}
	}
	// Real media still names itself, or the UI cannot display it.
	for _, in := range []string{"image/jpeg", "image/png", "image/webp", "video/mp4", "audio/mpeg"} {
		if got := sanitizeMediaType(in); got != in {
			t.Errorf("sanitizeMediaType(%q) = %q, want it kept", in, got)
		}
	}
	// A parameterised real type keeps its base.
	if got := sanitizeMediaType("image/jpeg; charset=binary"); got != "image/jpeg" {
		t.Errorf("parameterised type became %q", got)
	}
}

// The headers that make the type stick.
func TestTelemirrorImgSetsProtectiveHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writeTelemirrorImg(rec, []byte("<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>"),
		"image/svg+xml")

	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want it inert", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff: the browser may sniff the bytes and decide they are markup")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q, want it to allow nothing", csp)
	}
}
