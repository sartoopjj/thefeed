package tgbridge

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// newLogger bridges gotd's zap logging into the app's own log sink. gotd is
// otherwise silent: a failed MTProto handshake just turns into a backoff
// retry, so the caller sees a stall with no reason anywhere.
func newLogger(sink func(string)) *zap.Logger {
	enc := zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
		MessageKey:     "msg",
		LevelKey:       "level",
		NameKey:        "logger",
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
	})
	return zap.New(zapcore.NewCore(enc, zapcore.AddSync(writerFunc(sink)), zapcore.InfoLevel))
}

// writerFunc adapts a log callback to io.Writer for zap.
type writerFunc func(string)

func (w writerFunc) Write(p []byte) (int, error) {
	w(string(p))
	return len(p), nil
}

func (w writerFunc) Sync() error { return nil }
