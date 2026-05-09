// Package mobile is the gomobile-bind entry point used by the iOS
// (and Mac Catalyst) Swift app. It wraps internal/web.Server so the
// HTTP server runs in-process — iOS does not allow bundled child
// executables, so the Android subprocess approach can't be reused.
package mobile

import (
	"errors"
	"net"
	"strconv"
	"sync"

	"github.com/sartoopjj/thefeed/internal/web"
)

// Server is a running thefeed-client instance bound to 127.0.0.1.
type Server struct {
	web  *web.Server
	ln   net.Listener
	port int

	mu      sync.Mutex
	stopped bool
	doneErr error
	done    chan struct{}
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

	ws, err := web.New(dataDir, port, "127.0.0.1", "")
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	s := &Server{
		web:  ws,
		ln:   ln,
		port: port,
		done: make(chan struct{}),
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

// Port returns the listening port (0 after Stop).
func (s *Server) Port() int {
	if s == nil {
		return 0
	}
	return s.port
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
	_ = s.ln.Close()
	<-s.done
}
