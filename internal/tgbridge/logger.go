package tgbridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/log"
)

// newLogger bridges gotd's logging into the app's own log sink. gotd is
// otherwise silent: a failed MTProto handshake just turns into a backoff
// retry, so the caller sees a stall with no reason anywhere.
//
// gotd used to take a *zap.Logger and now takes its own two-method interface.
// Our sink is a single func(string), so implementing that interface here is
// less code than adapting a logging library to it.
func newLogger(sink func(string)) log.Logger { return sinkLogger{sink: sink} }

// sinkLogger renders a structured record as one line for a plain-text sink.
type sinkLogger struct{ sink func(string) }

// Enabled drops debug records. gotd calls this BEFORE building attributes,
// so refusing here keeps a chatty level off the hot path entirely rather
// than formatting records we would only discard.
func (s sinkLogger) Enabled(_ context.Context, level log.Level) bool {
	return s.sink != nil && level >= log.LevelInfo
}

func (s sinkLogger) Log(_ context.Context, level log.Level, msg string, attrs ...log.Attr) {
	if s.sink == nil {
		return
	}
	var b strings.Builder
	b.WriteString(strings.ToLower(level.String()))
	b.WriteString("\t")
	b.WriteString(msg)
	for _, a := range attrs {
		b.WriteString("\t")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(fmt.Sprint(a.Value.Any()))
	}
	s.sink(b.String())
}
