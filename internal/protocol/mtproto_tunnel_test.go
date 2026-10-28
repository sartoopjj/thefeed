package protocol

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestMTPOpenRoundTrip(t *testing.T) {
	dc, err := ParseMTPOpen(BuildMTPOpen(2))
	if err != nil || dc != 2 {
		t.Fatalf("open round trip: dc=%d err=%v", dc, err)
	}
	if _, err := ParseMTPOpen([]byte{MTPOpOpen}); err == nil {
		t.Fatal("truncated OPEN must not parse")
	}
	if _, err := ParseMTPOpen([]byte{MTPOpData, 2}); err == nil {
		t.Fatal("wrong op must not parse as OPEN")
	}
}

func TestMTPOpenReplyRoundTrip(t *testing.T) {
	st, chunk, err := ParseMTPOpenReply(BuildMTPOpenReply(MTPStatusOK, 12))
	if err != nil || st != MTPStatusOK || chunk != 12 {
		t.Fatalf("st=%d chunk=%d err=%v", st, chunk, err)
	}
	// A refusal carries no chunk size and must still parse.
	st, _, err = ParseMTPOpenReply([]byte{MTPStatusBadDC})
	if err != nil || st != MTPStatusBadDC {
		t.Fatalf("st=%d err=%v", st, err)
	}
}

func TestMTPDataRoundTrip(t *testing.T) {
	chunk := []byte("mtproto")
	req, err := ParseMTPRequest(BuildMTPData(1000, 2000, chunk))
	if err != nil {
		t.Fatal(err)
	}
	if req.Op != MTPOpData || req.UpSeq != 1000 || req.DownAck != 2000 {
		t.Fatalf("got %+v", req)
	}
	if !bytes.Equal(req.Chunk, chunk) {
		t.Fatalf("chunk = %q", req.Chunk)
	}
}

func TestMTPPollRoundTrip(t *testing.T) {
	req, err := ParseMTPRequest(BuildMTPPoll(12345))
	if err != nil {
		t.Fatal(err)
	}
	if req.Op != MTPOpPoll || req.DownAck != 12345 || len(req.Chunk) != 0 {
		t.Fatalf("got %+v", req)
	}
}

// An empty DATA cell is legal: it acks downstream bytes mid-send without
// advancing the upstream stream.
func TestMTPEmptyDataIsValid(t *testing.T) {
	req, err := ParseMTPRequest(BuildMTPData(5, 6, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Chunk) != 0 || req.UpSeq != 5 || req.DownAck != 6 {
		t.Fatalf("got %+v", req)
	}
}

func TestMTPReplyRoundTrip(t *testing.T) {
	down := bytes.Repeat([]byte{0xAB}, 500)
	r, err := ParseMTPReply(BuildMTPReply(MTPReply{
		Status: MTPStatusOK, UpAck: 300, DownSeq: 900, Down: down,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != MTPStatusOK || r.UpAck != 300 || r.DownSeq != 900 || !bytes.Equal(r.Down, down) {
		t.Fatalf("got status=%d up=%d down=%d len=%d", r.Status, r.UpAck, r.DownSeq, len(r.Down))
	}
}

func TestMTPStatusOnlyReplyParses(t *testing.T) {
	r, err := ParseMTPReply([]byte{MTPStatusNoTunnel})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != MTPStatusNoTunnel || len(r.Down) != 0 {
		t.Fatalf("got %+v", r)
	}
	if _, err := ParseMTPReply([]byte{MTPStatusOK}); err == nil {
		t.Fatal("a truncated OK reply must error, not read as empty")
	}
	if _, err := ParseMTPReply(nil); err == nil {
		t.Fatal("empty reply must not parse")
	}
}

// The header has to fit the SMALLEST budget the client's scorer can pick,
// otherwise the tunnel silently cannot send on exactly the networks where the
// compact budget is the only one that works.
func TestMTPChunkFitsEveryChatBudget(t *testing.T) {
	const compact = 8 // chatBudgetPresets["compact"]
	for _, tc := range []struct {
		name   string
		budget int
	}{
		{"compact", compact},
		{"standard", ChatCellPlainSize},
		{"wide", ChatCellPlainMax},
	} {
		if got := MTPChunkSize(tc.budget); got < MTPMinChunk {
			t.Errorf("%s budget %d: chunk = %d, want >= %d", tc.name, tc.budget, got, MTPMinChunk)
		}
	}
}

func TestMTPChunkSizeRejectsUnusableBudgets(t *testing.T) {
	// Anything that leaves less than a useful chunk must report 0, never a
	// negative or a 1-byte trickle that never finishes.
	for _, b := range []int{-5, 0, MTPDataHeader, MTPDataHeader + MTPMinChunk - 1} {
		if got := MTPChunkSize(b); got != 0 {
			t.Errorf("budget %d: chunk = %d, want 0", b, got)
		}
	}
	if got := MTPChunkSize(MTPDataHeader + MTPMinChunk); got != MTPMinChunk {
		t.Errorf("smallest usable budget: chunk = %d, want %d", got, MTPMinChunk)
	}
}

func TestMTPSeqWrapComparison(t *testing.T) {
	cases := []struct {
		name string
		a, b uint16
		less bool
	}{
		{"plain", 5, 9, true},
		{"equal", 9, 9, false},
		{"reverse", 9, 5, false},
		{"wrap forward", 65530, 4, true},   // a precedes b across the wrap
		{"wrap backward", 4, 65530, false}, // a retransmit from before the wrap
	}
	for _, tc := range cases {
		if got := MTPSeqLess(tc.a, tc.b); got != tc.less {
			t.Errorf("%s: MTPSeqLess(%d,%d) = %v", tc.name, tc.a, tc.b, got)
		}
	}
	if got := MTPSeqDelta(65530, 4); got != 10 {
		t.Errorf("delta across wrap = %d, want 10", got)
	}
	if got := MTPSeqDelta(4, 65530); got != -10 {
		t.Errorf("negative delta across wrap = %d, want -10", got)
	}
}

func TestMTPHeaderWidths(t *testing.T) {
	// The reply carries one extra pair: how much the server still holds, which
	// is what lets the client report progress instead of an unexplained wait.
	if MTPDataHeader != 5 || MTPPollHeader != 3 || MTPReplyHeader != 7 {
		t.Fatalf("headers: data=%d poll=%d reply=%d, want 5/3/7",
			MTPDataHeader, MTPPollHeader, MTPReplyHeader)
	}
}

func TestMTPInfoRoundTrip(t *testing.T) {
	in := MTPInfo{
		MinVersion: 1, MaxVersion: MTProtoVersion, Enabled: true,
		Domains: []string{"m.example.com", "m2.example.com"}, Chunk: 16,
		EkPub: bytes.Repeat([]byte{0x5A}, X25519KeySize),
	}
	got, err := ParseMTPInfo(EncodeMTPInfo(in))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.EkPub, in.EkPub) {
		t.Fatalf("ek lost: %x", got.EkPub)
	}
	if got.MinVersion != 1 || got.MaxVersion != MTProtoVersion || !got.Enabled || got.Chunk != 16 {
		t.Fatalf("got %+v", got)
	}
	if len(got.Domains) != 2 || got.Domains[0] != "m.example.com" || got.Domains[1] != "m2.example.com" {
		t.Fatalf("domains = %v", got.Domains)
	}
}

// A server that has the bridge configured but switched off must still be
// decodable, so the client says "turned off here" instead of "unsupported".
func TestMTPInfoDisabledIsNotUsable(t *testing.T) {
	got, err := ParseMTPInfo(EncodeMTPInfo(MTPInfo{
		MinVersion: 1, MaxVersion: 1, Enabled: false, Domains: []string{"m.example.com"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Usable() {
		t.Fatal("a disabled bridge must not be usable")
	}
}

func TestMTPInfoUsableChecksVersionAndDomains(t *testing.T) {
	base := MTPInfo{MinVersion: 1, MaxVersion: MTProtoVersion, Enabled: true,
		Domains: []string{"m.example.com"}, EkPub: bytes.Repeat([]byte{1}, X25519KeySize)}
	if !base.Usable() {
		t.Fatal("a matching server should be usable")
	}
	newer := base
	newer.MinVersion, newer.MaxVersion = MTProtoVersion+1, MTProtoVersion+2
	if newer.Usable() {
		t.Fatal("a server speaking only newer versions must not be usable")
	}
	noDomains := base
	noDomains.Domains = nil
	if noDomains.Usable() {
		t.Fatal("without domains there is nowhere to send cells")
	}
	noKey := base
	noKey.EkPub = nil
	if noKey.Usable() {
		t.Fatal("without the server key there is no way to seal a session")
	}
	var nilInfo *MTPInfo
	if nilInfo.Usable() {
		t.Fatal("nil info must not be usable")
	}
}

// A newer server may add fields; an older client must skip them, not fail.
func TestMTPInfoSkipsUnknownTLVs(t *testing.T) {
	payload := EncodeMTPInfo(MTPInfo{MinVersion: 1, MaxVersion: 1, Enabled: true,
		Domains: []string{"m.example.com"}})
	withUnknown := append(payload[:len(payload)-1], 0x7F, 0, 3, 'a', 'b', 'c', mtpInfoTLVEnd)
	got, err := ParseMTPInfo(withUnknown)
	if err != nil {
		t.Fatalf("unknown TLV broke parsing: %v", err)
	}
	if !got.Enabled || len(got.Domains) != 1 {
		t.Fatalf("fields lost around the unknown TLV: %+v", got)
	}
}

func TestMTPInfoRejectsTruncated(t *testing.T) {
	full := EncodeMTPInfo(MTPInfo{MinVersion: 1, MaxVersion: 1, Enabled: true,
		Domains: []string{"m.example.com"}})
	// Cut inside the domains value: the length byte now overruns the buffer.
	if _, err := ParseMTPInfo(full[:len(full)-6]); err == nil {
		t.Fatal("a truncated payload must not parse")
	}
}

// The bridge bit must ride alongside the chat bit without disturbing it.
func TestMetadataMTProtoBitIsIndependent(t *testing.T) {
	for _, tc := range []struct{ chat, mtp bool }{
		{false, false}, {true, false}, {false, true}, {true, true},
	} {
		m := &Metadata{ChatAvailable: tc.chat, MTProtoAvailable: tc.mtp, TelegramLoggedIn: true}
		got, err := ParseMetadata(SerializeMetadata(m))
		if err != nil {
			t.Fatal(err)
		}
		if got.ChatAvailable != tc.chat || got.MTProtoAvailable != tc.mtp || !got.TelegramLoggedIn {
			t.Fatalf("chat=%v mtp=%v -> %+v", tc.chat, tc.mtp, got)
		}
	}
}

func TestMTPHelloFragmentRoundTrip(t *testing.T) {
	eph := bytes.Repeat([]byte{0xC3}, X25519KeySize)
	cells, err := SplitMTPHello(eph, ChatCellPayloadSize, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) < 2 {
		t.Fatalf("a %d-byte key in %d-byte cells should need >1 fragment, got %d",
			len(eph), ChatCellPayloadSize, len(cells))
	}
	frags := map[uint32][]byte{}
	for i, c := range cells {
		frags[uint32(i)] = c
	}
	got, ok := JoinMTPHello(frags, ChatCellPayloadSize)
	if !ok {
		t.Fatal("complete set did not reassemble")
	}
	if !bytes.Equal(got, eph) {
		t.Fatal("reassembled key differs")
	}
}

// The handshake is the only unsealed path, so its cells must give an observer
// nothing: identical lengths, no framing byte at a fixed offset, and a random
// (never zero) tail on the last fragment.
func TestMTPHelloFragmentsLookRandom(t *testing.T) {
	eph := bytes.Repeat([]byte{0xC3}, X25519KeySize)
	cells, err := SplitMTPHello(eph, ChatCellPayloadSize, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cells {
		if len(c) != ChatCellPayloadSize {
			t.Fatalf("fragment %d is %d bytes; varying lengths are a fingerprint", i, len(c))
		}
	}
	last := len(cells) - 1
	used := X25519KeySize - last*ChatCellPayloadSize
	if used >= ChatCellPayloadSize {
		t.Skip("key divides evenly; no pad to check")
	}
	again, err := SplitMTPHello(eph, ChatCellPayloadSize, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cells[last][used:], again[last][used:]) {
		t.Fatal("padding repeated across splits: it is not random")
	}
	if allZeroBytes(cells[last][used:]) {
		t.Fatal("zero padding leaves a visible run in the query name")
	}
}

func allZeroBytes(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return len(b) > 0
}

func TestMTPHelloJoinRejectsIncomplete(t *testing.T) {
	eph := bytes.Repeat([]byte{1}, X25519KeySize)
	cells, _ := SplitMTPHello(eph, ChatCellPayloadSize, rand.Reader)
	if _, ok := JoinMTPHello(map[uint32][]byte{0: cells[0]}, ChatCellPayloadSize); ok {
		t.Fatal("a partial set reassembled")
	}
	// Right count, wrong indices: fragment 1 never arrived.
	if _, ok := JoinMTPHello(map[uint32][]byte{0: cells[0], 99: cells[1]}, ChatCellPayloadSize); ok {
		t.Fatal("a set with a missing index reassembled")
	}
	if _, ok := JoinMTPHello(map[uint32][]byte{}, 0); ok {
		t.Fatal("a zero payload size reassembled")
	}
}

func TestMTPHelloRejectsBadInput(t *testing.T) {
	full := bytes.Repeat([]byte{1}, X25519KeySize)
	if _, err := SplitMTPHello(bytes.Repeat([]byte{1}, 10), ChatCellPayloadSize, rand.Reader); err == nil {
		t.Fatal("a key of the wrong length must be refused")
	}
	if _, err := SplitMTPHello(full, 0, rand.Reader); err == nil {
		t.Fatal("a zero payload size must be refused")
	}
	// Too small a payload needs more fragments than the server will hold.
	if _, err := SplitMTPHello(full, 1, rand.Reader); err == nil {
		t.Fatal("an absurd fragment count must be refused")
	}
}

func TestMTPHelloAckRoundTrip(t *testing.T) {
	var ref [ChatSelectorSize]byte
	copy(ref[:], []byte{9, 8, 7})
	st, got, chunk, err := ParseMTPHelloAck(BuildMTPHelloAck(MTPStatusOK, ref, 16))
	if err != nil || st != MTPStatusOK || got != ref || chunk != 16 {
		t.Fatalf("st=%d ref=%v chunk=%d err=%v", st, got, chunk, err)
	}
	// A refusal carries no reference.
	st, _, _, err = ParseMTPHelloAck([]byte{MTPStatusBusy})
	if err != nil || st != MTPStatusBusy {
		t.Fatalf("st=%d err=%v", st, err)
	}
	if _, _, _, err := ParseMTPHelloAck([]byte{MTPStatusOK, 1}); err == nil {
		t.Fatal("a truncated OK ack must not parse")
	}
}

// The two users of the cell layer must live in different key spaces, or a chat
// session could drive a tunnel.
func TestMTPSessionKeyDiffersFromChat(t *testing.T) {
	eph, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	var qk [KeySize]byte
	qk[0] = 7
	chatKey, err := ChatSessionKey(eph, peer.PublicKey().Bytes(), 1, qk)
	if err != nil {
		t.Fatal(err)
	}
	mtpKey, err := MTPSessionKey(eph, peer.PublicKey().Bytes(), 1, qk)
	if err != nil {
		t.Fatal(err)
	}
	if chatKey == mtpKey {
		t.Fatal("chat and tunnel derive the same session key from one exchange")
	}
	// Both sides of the same exchange must still agree within their own space.
	peerSide, err := MTPSessionKey(peer, eph.PublicKey().Bytes(), 1, qk)
	if err != nil {
		t.Fatal(err)
	}
	if peerSide != mtpKey {
		t.Fatal("tunnel ECDH does not agree between the two sides")
	}
}

// Regression: a DATA cell's chunk must survive the seal byte-for-byte. The
// cell layer zero-pads to the budget, and a tunnel chunk has no length field,
// so padding a DATA cell would silently append zero bytes to the user's
// stream — the relay would then write them to Telegram.
func TestMTPDataCellIsSealedExactly(t *testing.T) {
	var ks [KeySize]byte
	var ref [ChatSelectorSize]byte
	ks[0], ref[0] = 0x11, 0x22

	for _, chunk := range [][]byte{
		[]byte("abc"),               // the smallest useful chunk
		[]byte("partial write"),     // shorter than a full cell
		bytes.Repeat([]byte{7}, 16), // a full chunk at the widest budget
	} {
		plain := BuildMTPData(5, 9, chunk)
		pad := MTPCellPad(plain, ChatCellPlainMax, ChatJitterMax)
		if pad != len(plain) {
			t.Fatalf("DATA padded to %d, want its exact %d bytes", pad, len(plain))
		}
		sealed, err := SealChatCellPayloadN(ks, ref, 3, plain, pad)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := OpenChatCellPayload(ks, ref, 3, sealed)
		if err != nil {
			t.Fatal(err)
		}
		req, err := ParseMTPRequest(opened)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(req.Chunk, chunk) {
			t.Fatalf("chunk round-trip: got %q (%d bytes), want %q (%d)",
				req.Chunk, len(req.Chunk), chunk, len(chunk))
		}
	}
}

// Control ops carry fixed-size fields, so they keep the uniform padding that
// stops them standing out by length — and POLL is shorter than the cell
// layer's minimum, so it must be padded to be sealable at all.
func TestMTPControlCellsStayPadded(t *testing.T) {
	for _, plain := range [][]byte{
		BuildMTPPoll(4), BuildMTPOpen(2), BuildMTPClose(),
	} {
		pad := MTPCellPad(plain, ChatCellPlainSize, 2)
		if pad != ChatCellPlainSize+2 {
			t.Fatalf("op %d padded to %d, want the uniform %d", plain[0], pad, ChatCellPlainSize+2)
		}
		if pad < ChatCellPlainMin {
			t.Fatalf("op %d pads below the cell minimum", plain[0])
		}
	}
}
func TestMTPSealedOpsAreVersionBound(t *testing.T) {
	eph, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	server, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatal(err)
	}
	var qk [KeySize]byte
	v1, err := MTPSessionKey(eph, server.PublicKey().Bytes(), MTProtoVersion, qk)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := MTPSessionKey(eph, server.PublicKey().Bytes(), MTProtoVersion+1, qk)
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 {
		t.Fatal("two wire versions derive the same session key")
	}

	var ref [ChatSelectorSize]byte
	sealed, err := SealChatCellPayloadN(v1, ref, 1, BuildMTPPoll(0), ChatCellPlainSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenChatCellPayload(v2, ref, 1, sealed); err == nil {
		t.Fatal("a cell sealed at one version opened at another")
	}
}

// A client that does not know the relay is on TEST data centres fails deep in
// the MTProto handshake with "wrong DC" — impossible to diagnose from the UI.
// The capability payload has to carry it.
func TestMTPInfoCarriesTestDCs(t *testing.T) {
	for _, want := range []bool{false, true} {
		in := MTPInfo{
			MinVersion: MTProtoVersion, MaxVersion: MTProtoVersion,
			Enabled: true, Domains: []string{"m.example.com"},
			Chunk: 16, EkPub: make([]byte, X25519KeySize), TestDCs: want,
		}
		got, err := ParseMTPInfo(EncodeMTPInfo(in))
		if err != nil {
			t.Fatal(err)
		}
		if got.TestDCs != want {
			t.Fatalf("TestDCs = %v, want %v", got.TestDCs, want)
		}
		// The rest must survive the new field.
		if !got.Enabled || got.Chunk != 16 || len(got.Domains) != 1 {
			t.Fatalf("payload damaged: %+v", got)
		}
	}
}

// The reply says how much the server still holds, so a client waiting on a
// large answer can show what is left instead of a spinner.
func TestReplyCarriesRemainingBytes(t *testing.T) {
	raw := BuildMTPReply(MTPReply{
		Status: MTPStatusOK, UpAck: 11, DownSeq: 22, DownLeft: 1234,
		Down: []byte("partial"),
	})
	got, err := ParseMTPReply(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.DownLeft != 1234 {
		t.Errorf("downLeft = %d, want 1234", got.DownLeft)
	}
	if got.UpAck != 11 || got.DownSeq != 22 || string(got.Down) != "partial" {
		t.Errorf("reply round trip wrong: %+v", got)
	}
}
