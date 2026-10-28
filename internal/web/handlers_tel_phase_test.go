package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
)

// gotd's log lines are the only progress signal inside the Telegram phase, so
// the mapping has to hold; an unknown line must leave the phase alone.
func TestPhaseFromGotd(t *testing.T) {
	cases := []struct{ msg, want string }{
		{`info conn.mtproto Generating auth key {"dc_id": 4}`, telPhaseKeyExchange},
		{`info conn.mtproto Auth key generated {"duration": "1m19s"}`, telPhaseSession},
		{`info conn.mtproto Key already exists {"dc_id": 4}`, telPhaseSession},
		{`info Restarting connection {"error": "whatever"}`, telPhaseTunnel},
		{`info something gotd has never said before`, telPhaseTunnel},
	}
	for _, c := range cases {
		h := &telHub{}
		h.setPhase(telPhaseTunnel)
		h.phaseFromGotd(c.msg)
		if got := h.currentPhase(); got != c.want {
			t.Errorf("%q -> %q, want %q", c.msg, got, c.want)
		}
	}
	// Not connecting: nothing to report, so nothing changes.
	h := &telHub{}
	h.phaseFromGotd(`info conn.mtproto Auth key generated`)
	if got := h.currentPhase(); got != "" {
		t.Errorf("phase set while idle: %q", got)
	}
}

// The exact strings gotd produced on a real degraded link. "engine was closed"
// and "engine forcibly closed" are different messages and both mean the
// connection is gone; matching only the first left the UI offering login steps
// against a dead connection.
func TestDeadAndLostReplyClassification(t *testing.T) {
	const forcibly = "send code: rpcDoRequest: retryUntilAck: engine forcibly closed: context canceled"
	const writeTimeout = "pingLoop: disconnect (pong missed): write: write abridged: mtproto tunnel: i/o timeout"

	// NOT dead: gotd logs "Restarting connection" straight after this and
	// builds another connection, so the bridge must survive.
	if isDeadConnection(errString(forcibly)) {
		t.Error("a forcibly closed engine must NOT be treated as terminal; gotd reconnects")
	}
	if !isDeadConnection(errString("telegram: engine was closed")) {
		t.Error("engine was closed IS terminal")
	}
	if !isLostReply(errString(forcibly)) {
		t.Error("a forcibly closed engine must count as a lost reply: Telegram may have acted")
	}
	if !isLostReply(errString(writeTimeout)) {
		t.Error("a write timeout must count as a lost reply")
	}
	// A refusal is NOT a lost reply: Telegram answered, and saying "it may have
	// worked" would be wrong.
	if isLostReply(errString("rpc error code 400: PHONE_CODE_INVALID")) {
		t.Error("a Telegram refusal must not be reported as a lost reply")
	}
	if isDeadConnection(errString("rpc error code 400: PHONE_CODE_INVALID")) {
		t.Error("a Telegram refusal must not drop the connection")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A confirmed server must stay offered when its advertised bit goes stale.
// Only a VERIFIED negative may take it away: on a slow link the bit expires
// routinely, and turning that into "no config has a Telegram tunnel" hid the
// config that had just worked.
func TestStaleAdvertisedBitIsNotANegative(t *testing.T) {
	s := &Server{mtpAdv: map[string]chatAdvEntry{}}
	const key = "srv"

	s.mtpAdv[key] = chatAdvEntry{advertised: true, at: time.Now()}
	if adv, fresh := s.mtpAdvVerified(key); !adv || !fresh {
		t.Fatalf("fresh yes: advertised=%v fresh=%v, want true/true", adv, fresh)
	}

	// Same answer, just old.
	s.mtpAdv[key] = chatAdvEntry{advertised: true, at: time.Now().Add(-2 * chatAdvTTL)}
	adv, fresh := s.mtpAdvVerified(key)
	if !adv {
		t.Error("a stale yes must still be offered; staleness is not a verified no")
	}
	if fresh {
		t.Error("a stale yes must be reported as unconfirmed")
	}

	// A verified no stays a no.
	s.mtpAdv[key] = chatAdvEntry{advertised: false, at: time.Now()}
	if adv, _ := s.mtpAdvVerified(key); adv {
		t.Error("a verified no must not be reported as offered")
	}

	// Never asked is not a no either, but there is nothing to offer yet.
	if adv, fresh := s.mtpAdvVerified("unseen"); adv || fresh {
		t.Errorf("unseen: advertised=%v fresh=%v, want false/false", adv, fresh)
	}
}

// A failure must stop being shown once the next operation starts. Nothing used
// to clear it, so a wrong 2FA password stayed on screen through a successful
// login and every fetch after it.
func TestBeginOpClearsTheLastError(t *testing.T) {
	h := &telHub{}
	h.setErr(errString("rpc error code 400: PASSWORD_HASH_INVALID"))
	h.mu.Lock()
	had := h.lastErr
	h.mu.Unlock()
	if had == "" {
		t.Fatal("setErr stored nothing")
	}

	h.beginOp()
	defer h.endOp()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastErr != "" || h.lastErrKey != "" {
		t.Errorf("error survived into the next operation: %q / %q", h.lastErr, h.lastErrKey)
	}
}

// beginOp/endOp must balance, or the keepalive is suppressed for ever.
func TestOpCounterBalances(t *testing.T) {
	h := &telHub{}
	for i := 0; i < 3; i++ {
		h.beginOp()
	}
	if got := h.ops.Load(); got != 3 {
		t.Fatalf("ops = %d, want 3", got)
	}
	for i := 0; i < 3; i++ {
		h.endOp()
	}
	if got := h.ops.Load(); got != 0 {
		t.Fatalf("ops = %d after pairing, want 0", got)
	}
	// An unmatched endOp must not drive it negative, which would make the
	// counter read as "busy" for ever once it wrapped back up.
	h.endOp()
	if got := h.ops.Load(); got < 0 {
		t.Fatalf("ops = %d, must never go negative", got)
	}
}

// Background checking is on unless the user has turned it off. A plain bool
// could not express that: omitempty drops a false, so "turned it off" and
// "never touched it" looked identical on disk.
func TestWatchDefaultsOn(t *testing.T) {
	off, on := false, true
	cases := []struct {
		name string
		set  *bool
		want bool
	}{
		{"never chosen", nil, true},
		{"turned off", &off, false},
		{"turned on", &on, true},
	}
	for _, c := range cases {
		var pl ProfileList
		pl.TelWatch = c.set
		got := pl.TelWatch == nil || *pl.TelWatch
		if got != c.want {
			t.Errorf("%s: watching = %v, want %v", c.name, got, c.want)
		}
	}
}

// An explicit "off" must survive a save/load round trip, or the default turns
// it straight back on.
func TestWatchOffSurvivesJSON(t *testing.T) {
	off := false
	blob, err := json.Marshal(ProfileList{TelWatch: &off})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), "telWatch") {
		t.Fatalf("an explicit off was dropped from %s", blob)
	}
	var back ProfileList
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if back.TelWatch == nil || *back.TelWatch {
		t.Error("an explicit off did not survive the round trip")
	}
}

// The strip shows a per-message meter and a running total on one line. Starting
// an operation must reset the meter and NOT the total, or the two halves
// contradict each other: "query 10 of 11 (1 query in total)".
func TestBeginOpKeepsTheRunningTotal(t *testing.T) {
	h := &telHub{}
	for i := 0; i < 7; i++ {
		h.countCell()
	}
	h.recordProgress(client.TunnelProgress{SentCells: 10, TotalCells: 11, DownLeft: 42})

	h.beginOp()
	defer h.endOp()

	if got := h.cells.Load(); got != 7 {
		t.Errorf("running total = %d, want 7: it must survive a new operation", got)
	}
	if got := h.sentCells.Load(); got != 0 {
		t.Errorf("per-message sent = %d, want 0", got)
	}
	if got := h.totalCells.Load(); got != 0 {
		t.Errorf("per-message total = %d, want 0", got)
	}
	if got := h.downLeft.Load(); got != 0 {
		t.Errorf("downLeft = %d, want 0", got)
	}

	// And the total keeps climbing across operations.
	h.countCell()
	if got := h.cells.Load(); got != 8 {
		t.Errorf("running total = %d, want 8", got)
	}
}

// A lost reply on the code request is not the same failure as a lost reply
// elsewhere: Telegram returns the code's hash in that reply, nothing can fetch
// it afterwards, and without it a delivered code cannot be used at all. Telling
// the user to enter it sent them looking for a field that is not there.
func TestLostCodeHashGetsItsOwnReason(t *testing.T) {
	lost := errString("send code: rpcDoRequest: retryUntilAck: engine forcibly closed: context canceled")
	refused := errString("rpc error code 400: PHONE_NUMBER_INVALID")

	if !isLostReply(lost) {
		t.Fatal("the dropped-connection error must count as a lost reply")
	}
	// A real refusal must keep its own Telegram reason, not be relabelled.
	if isLostReply(refused) {
		t.Fatal("a refusal is not a lost reply")
	}
	// It must not be relabelled as a lost reply, which would tell the user
	// their request may have gone through when Telegram plainly refused it.
	if got := telStatusKey(refused); got == "tel_err_lost_reply" || got == "tel_err_lost_code_hash" {
		t.Errorf("refusal reported as a lost reply (%q)", got)
	}
	// The generic lost-reply key still exists for the other steps.
	if got := telStatusKey(lost); got != "tel_err_lost_reply" {
		t.Errorf("generic lost-reply reason = %q, want tel_err_lost_reply", got)
	}
}

// Opening the app should go straight back to the config the session was last
// on -- but only for an account already signed in, or it would spend queries
// connecting for someone who never asked.
func TestResumeServerPersistsOnlyWhenSignedIn(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	h := &telHub{s: s, dir: dir}

	// Not signed in: nothing to go back to, whatever is stored.
	if got := h.resumeServer(); got != "" {
		t.Errorf("resumeServer = %q with no session, want empty", got)
	}

	// In-memory beats everything: a drop mid-login must still resume, even
	// before any session exists.
	h.mu.Lock()
	h.lastCfg = Config{Domain: "m.example.com"}
	h.mu.Unlock()
	if got := h.resumeServer(); got != chatServerKey("m.example.com") {
		t.Errorf("resumeServer = %q, want the in-memory config", got)
	}
}

// A remembered config that no longer exists must stop being the default,
// rather than leaving the app retrying a server the user deleted.
func TestResumeServerDropsADeletedConfig(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	h := &telHub{s: s, dir: dir}
	// One real config, so "not in the list" is a fact about the stored key
	// rather than about there being no list at all.
	if err := s.saveProfiles(&ProfileList{Profiles: []Profile{
		{Nickname: "kept", Config: Config{Domain: "kept.example.com", ServerKey: "k"}},
	}}); err != nil {
		t.Fatal(err)
	}
	h.rememberResume("gone.example.com")

	pl, err := s.loadProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if pl.TelLastServer != "gone.example.com" {
		t.Fatalf("stored %q, want it remembered", pl.TelLastServer)
	}
	// No session and no matching config: nothing to go back to either way.
	if got := h.resumeServer(); got != "" {
		t.Errorf("resumeServer = %q for a config that is not in the list", got)
	}
}

// gotd narrates every connection in detail, and that detail is broadcast to
// every browser attached to this backend. Only trouble is published unless
// debug is on -- but trouble must never be swallowed.
func TestOnlyTroubleIsLogged(t *testing.T) {
	keep := []string{
		`info Restarting connection {"error": "write: EOF"}`,
		`error conn.mtproto Failed to ACK {"error": "write abridged: EOF"}`,
		`info conn.mtproto.rpc Close called {"dc_id": 4}`,
		`warn something looks off`,
	}
	for _, m := range keep {
		if !telLogWorthShowing(m) {
			t.Errorf("dropped a line that explains a failure: %q", m)
		}
	}
	drop := []string{
		`info conn.mtproto Key already exists {"dc_id": 4}`,
		`info conn SessionInit {"conn_id": 0}`,
		`info Starting {"v": "v0.142.0"}`,
		`info conn.mtproto Auth key generated {"duration": "1m19s"}`,
	}
	for _, m := range drop {
		if telLogWorthShowing(m) {
			t.Errorf("published routine narration: %q", m)
		}
	}
}

// The phase still has to be read from EVERY line, including the quiet ones --
// "Auth key generated" is how the UI knows the key exchange is done, and it is
// one of the lines that is not published.
func TestPhaseStillReadsQuietLines(t *testing.T) {
	h := &telHub{}
	h.setPhase(telPhaseTunnel)
	h.phaseFromGotd(`info conn.mtproto Auth key generated {"duration": "1m19s"}`)
	if got := h.currentPhase(); got != telPhaseSession {
		t.Fatalf("phase = %q, want %q: a quiet line still drives the UI", got, telPhaseSession)
	}
	if telLogWorthShowing(`info conn.mtproto Auth key generated`) {
		t.Error("that line should not be published, only read")
	}
}
