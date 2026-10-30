package protocol

import (
	"bytes"
	"compress/flate"
	"runtime"
	"testing"
)

// Compressed feed data arrives from whoever answered the DNS query, and the
// passphrase that authorises answering is public by design. Deflate expands
// about a thousand times, so a few hundred kilobytes on the wire inflate into
// gigabytes: the client is killed by the operating system, on a device the
// attacker never touched.
func TestDecompressRefusesABombAtTheCap(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(compressionDeflate)
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	zeros := make([]byte, 1<<20)
	for i := 0; i < 64; i++ { // 64 MiB in
		w.Write(zeros)
	}
	w.Close()

	// The cap scales with what arrived -- no fixed ceiling, because how large
	// a payload a server sends is its operator's choice.
	cap := InflateCap(buf.Len())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := DecompressMessagesLimited(buf.Bytes(), cap)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatalf("%d bytes on the wire inflated past the cap unchallenged (%d bytes out)",
			buf.Len(), len(out))
	}
	// Refusing it must also be cheap: inflating the whole stream to discover
	// it is too big would be the same denial by another route. The bug this
	// guards allocated 5 GiB for 300 KB on the wire, so the bar is wide --
	// deliberately, because -race inflates these counters and a tight bound
	// here fails for a reason that has nothing to do with the property.
	if mb := (after.TotalAlloc - before.TotalAlloc) / (1 << 20); mb > 256 {
		t.Errorf("allocated %d MiB to refuse a %d MiB cap", mb, cap>>20)
	}

	// The ratio is what is bounded, so a server that relays big files is not
	// penalised: a 35 MB payload gets a cap far above itself, while the bomb
	// above -- a few hundred KB claiming gigabytes -- is still refused.
	if big := InflateCap(35 << 20); big < 35<<20 {
		t.Errorf("a 35 MB payload would be capped at %d, below its own size", big)
	}
	if small := InflateCap(1024); small < 4<<20 {
		t.Errorf("a small payload got a cap of %d, too tight to be useful", small)
	}

	// And a stream inside the cap still decompresses.
	var ok bytes.Buffer
	ok.WriteByte(compressionDeflate)
	w2, _ := flate.NewWriter(&ok, flate.BestCompression)
	w2.Write([]byte("a real channel's messages"))
	w2.Close()
	got, err := DecompressMessagesLimited(ok.Bytes(), InflateCap(ok.Len()))
	if err != nil || string(got) != "a real channel's messages" {
		t.Fatalf("a normal payload was refused: %q %v", got, err)
	}
}
