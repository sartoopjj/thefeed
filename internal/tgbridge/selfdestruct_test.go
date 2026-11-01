package tgbridge

import (
	"testing"

	"github.com/gotd/td/tg"
)

// Self-destructing media is never offered for forwarding (Telegram refuses it)
// and, once its time is up, says so rather than naming a file that is gone.
func TestSelfDestructingMedia(t *testing.T) {
	msg := func(media tg.MessageMediaClass) Message {
		m := &tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 1}}
		m.SetMedia(media)
		out, _ := messageFrom(m, nil)
		return out
	}
	timedPhoto := &tg.MessageMediaPhoto{}
	timedPhoto.SetPhoto(&tg.Photo{ID: 1})
	timedPhoto.SetTTLSeconds(10)
	expiredPhoto := &tg.MessageMediaPhoto{}
	expiredPhoto.SetTTLSeconds(10)
	expiredVideo := &tg.MessageMediaDocument{}
	expiredVideo.SetTTLSeconds(10)
	plain := &tg.MessageMediaPhoto{}
	plain.SetPhoto(&tg.Photo{ID: 1})

	for _, c := range []struct {
		name   string
		media  tg.MessageMediaClass
		kind   string
		timed  bool
		locked bool
	}{
		{"timed photo", timedPhoto, MediaPhoto, true, true},
		{"expired photo", expiredPhoto, MediaPhotoExpired, true, true},
		{"expired video", expiredVideo, MediaVideoExpired, true, true},
		{"ordinary photo", plain, MediaPhoto, false, false},
	} {
		got := msg(c.media)
		if got.Media != c.kind || got.Timed != c.timed || got.Protected != c.locked {
			t.Errorf("%s: media=%q timed=%v protected=%v", c.name, got.Media, got.Timed, got.Protected)
		}
	}
}
