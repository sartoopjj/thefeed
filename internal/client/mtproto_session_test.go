package client

import (
	"errors"
	"testing"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// Busy reaches callers as ErrMTProtoBusy, from the handshake and from an open.
func TestBusyIsTyped(t *testing.T) {
	if !errors.Is(mtpStatusError(protocol.MTPStatusBusy), ErrMTProtoBusy) || !errors.Is(openStatusError(protocol.MTPStatusBusy), ErrMTProtoBusy) {
		t.Fatal("busy is not ErrMTProtoBusy")
	}
}
