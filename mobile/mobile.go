// Package mobile is the gomobile-bind entry point used by the iOS
// Swift app and the Android app. It wraps internal/web.Server so the
// HTTP server runs in-process via a JNI .so — no subprocess, no exec
// from nativeLibraryDir, no PIE/page-size/SELinux pitfalls.
package mobile

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/sartoopjj/thefeed/internal/web"
)

// Server is a running thefeed-client instance bound to 127.0.0.1.
type Server struct {
	web      *web.Server
	ln       net.Listener
	port     int
	password string

	mu      sync.Mutex
	stopped bool
	doneErr error
	done    chan struct{}
}

// MessageNotifier is implemented on the native side (Kotlin/Swift). The Go
// background chat poll loop calls OnNewMessages when it discovers new messages,
// so the native app can post a system notification while it's backgrounded
// (the in-app, foreground case is handled by the web UI). The implementation
// must be cheap and non-blocking — it runs on a Go poll goroutine.
type MessageNotifier interface {
	OnNewMessages(count int)
}

// SetMessageNotifier registers (or, with nil, clears) the native new-message
// notifier. Safe to call after the server is running.
func (s *Server) SetMessageNotifier(n MessageNotifier) {
	if s == nil || s.web == nil {
		return
	}
	if n == nil {
		s.web.SetNewMessageHandler(nil)
		return
	}
	s.web.SetNewMessageHandler(func(count int) { n.OnNewMessages(count) })
}

// NewServer starts a server on 127.0.0.1. preferredPort=0 picks a
// kernel-assigned port; a positive value is tried first and falls
// back to kernel-assigned on bind failure. dataDir must be a writable
// app-private directory (e.g. NSDocumentDirectory on iOS).
func NewServer(dataDir string, preferredPort int) (*Server, error) {
	if dataDir == "" {
		return nil, errors.New("mobile: dataDir is empty")
	}
	var ln net.Listener
	var err error
	if preferredPort > 0 {
		ln, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(preferredPort)))
	}
	if ln == nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// Loopback is not private on a phone: any other app with the INTERNET
	// permission can scan these ports and read the messenger seed, the feed
	// passphrase and the chat history -- and the service starts before the
	// lock screen, so it could do it while the app was locked. So the server
	// runs with HTTP Basic auth, under a secret generated fresh every launch
	// that only this process and its own WebView ever see (see Password).
	//
	// Same mechanism the desktop --password uses; the only difference is who
	// chooses the secret.
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		_ = ln.Close()
		return nil, err
	}
	password := base64.RawURLEncoding.EncodeToString(secret[:])

	ws, err := web.New(dataDir, port, "127.0.0.1", password)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	s := &Server{
		web:      ws,
		ln:       ln,
		port:     port,
		password: password,
		done:     make(chan struct{}),
	}
	go func() {
		err := ws.Serve(ln)
		s.mu.Lock()
		s.doneErr = err
		s.mu.Unlock()
		close(s.done)
	}()
	return s, nil
}

// NewAndroidServer is NewServer for per-ABI APK builds: the update
// prompt points at the matching ABI asset.
func NewAndroidServer(dataDir string, preferredPort int) (*Server, error) {
	os.Setenv("THEFEED_ANDROID_APK", "1")
	return NewServer(dataDir, preferredPort)
}

// NewAndroidPlayServer is NewServer for the Google Play build: it disables the
// in-app updater end to end. Deliberately does NOT set THEFEED_ANDROID_APK —
// that flag is what points the updater at a GitHub APK asset.
func NewAndroidPlayServer(dataDir string, preferredPort int) (*Server, error) {
	os.Setenv("THEFEED_STORE_BUILD", "1")
	return NewServer(dataDir, preferredPort)
}

// NewAndroidUniversalServer is NewServer for the universal APK build:
// the update prompt keeps the user on the universal asset.
func NewAndroidUniversalServer(dataDir string, preferredPort int) (*Server, error) {
	os.Setenv("THEFEED_ANDROID_APK", "1")
	os.Setenv("THEFEED_ANDROID_UNIVERSAL", "1")
	return NewServer(dataDir, preferredPort)
}

// Port returns the listening port (0 after Stop).
func (s *Server) Port() int {
	if s == nil {
		return 0
	}
	return s.port
}

// Password is the per-launch HTTP Basic secret every request to this server
// must carry. Any user name is accepted; only the password is checked.
//
// The native side answers the server's auth challenge with it -- on Android
// from WebViewClient.onReceivedHttpAuthRequest, on iOS from the WKWebView
// navigation delegate's didReceive(challenge:). Those fire for sub-resources
// too, so one implementation covers the page, its scripts, its images, its
// XHRs and its event stream. Another app on the device sees 401.
//
// Never write it anywhere another app can read -- app-private storage or an
// in-process value only. It changes every launch, so nothing should persist
// it beyond the process either.
func (s *Server) Password() string {
	if s == nil {
		return ""
	}
	return s.password
}

// Stop closes the listener and waits for serve to return.
func (s *Server) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	// Shutdown cancels the fetcher/checker/chat goroutines and closes the
	// HTTP server; without it those goroutines leak across every
	// background→foreground cycle and keep writing the shared profiles.json
	// while the app is suspended. It also closes the listener for us, but we
	// still close ln explicitly in case the server never reached serve().
	if s.web != nil {
		s.web.Shutdown()
	}
	_ = s.ln.Close()
	<-s.done
}
