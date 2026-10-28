// Package tgbridge speaks Telegram's MTProto over thefeed's DNS tunnel.
//
// It is deliberately separate from the project's own messenger (internal/web
// chat) and from the server-side Telegram reader: this is a CLIENT talking to
// Telegram as the user, where the relay only moves bytes and never holds the
// user's keys.
package tgbridge

import (
	"context"
	"errors"
	"net"

	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/transport"
)

// Dialer opens a byte stream to one Telegram data centre over the tunnel.
// The relay addresses data centres by id, so that is all this needs.
type Dialer func(ctx context.Context, dc int) (net.Conn, error)

// ErrNoCDNOverTunnel: CDN data centres serve media on a different flow and the
// relay deliberately allows only primary DCs, so asking for one is a bug
// rather than something to retry.
var ErrNoCDNOverTunnel = errors.New("tgbridge: CDN data centres are not reachable over the tunnel")

// tunnelResolver is a dcs.Resolver for the tunnel.
//
// gotd's own dcs.Plain is the wrong seam here: its DialFunc receives a
// host:port taken from Telegram's DC list and may race several candidates,
// while the tunnel is addressed by DC id and has exactly one path per DC.
// Implementing Resolver directly means no address list, no racing dial, and no
// obfuscation layer -- the relay carries the raw stream and the transport
// handshake stays end-to-end between this client and Telegram.
type tunnelResolver struct {
	dial  Dialer
	proto dcs.Protocol
}

// NewResolver returns a dcs.Resolver that reaches Telegram through dial.
//
// The transport protocol is abridged, not gotd's default intermediate: over
// DNS every byte is part of a query name, and abridged spends 1 length byte
// per message where intermediate spends 4.
func NewResolver(dial Dialer) (dcs.Resolver, error) {
	if dial == nil {
		return nil, errors.New("tgbridge: no dialer")
	}
	return tunnelResolver{dial: dial, proto: transport.Abridged}, nil
}

func (r tunnelResolver) connect(ctx context.Context, dc int) (transport.Conn, error) {
	conn, err := r.dial(ctx, dc)
	if err != nil {
		return nil, err
	}
	tc, err := r.proto.Handshake(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return tc, nil
}

func (r tunnelResolver) Primary(ctx context.Context, dc int, _ dcs.List) (transport.Conn, error) {
	return r.connect(ctx, dc)
}

// MediaOnly falls back to the primary path: the relay connects to a DC's
// primary address, which serves media requests too, just without the
// media-optimised endpoint.
func (r tunnelResolver) MediaOnly(ctx context.Context, dc int, _ dcs.List) (transport.Conn, error) {
	return r.connect(ctx, dc)
}

func (r tunnelResolver) CDN(_ context.Context, _ int, _ dcs.List) (transport.Conn, error) {
	return nil, ErrNoCDNOverTunnel
}
