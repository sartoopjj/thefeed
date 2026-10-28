package web

import (
	"os/exec"
	"testing"
)

// TestJSI18NCoverage runs the node checks over i18n.js: every t('key') used in
// the frontend must exist in every language table. t() falls back to the key
// itself, so a missing one renders as raw text (the update dialog shipped
// showing a literal "update_downloading"). Skipped when node isn't installed.
func TestJSI18NCoverage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/i18n.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("i18n coverage failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// A Telegram message comes from whoever chose to send one, so nothing in it
// may become code. Run with the rest of the suite, not by hand.
func TestJSTelegramXSS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_xss.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("telegram XSS checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// The reaction a user sees the instant they tap, before Telegram answers. A
// wrong guess here shows a count that is simply untrue. Run with the rest of
// the suite, not by hand.
func TestJSTelegramReactions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_react.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("reaction count checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// Message order after a send settles from the send's own answer. Run with the
// rest of the suite, not by hand.
func TestJSTelegramSendOrder(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_order.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("send order checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// Which messages a counter refresh asks about: a cost decision as much as a
// correctness one, since every id costs bytes in the answer. Run with the rest
// of the suite, not by hand.
func TestJSTelegramStatsSelection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_stats.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("stats selection checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// Where the "new messages" line goes, and that it stays put while someone is
// reading it. Run with the rest of the suite, not by hand.
func TestJSTelegramNewMessageLine(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_newsep.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("new-messages line checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// Fetching the rest of a long conversation list: the background pager has to
// be armed on a normal start, not only on signing in. Run with the suite.
func TestJSTelegramDialogPaging(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_paging.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("dialog paging checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// Every cache the backend reports must have a label, an icon and a colour on
// the storage screen. Run with the rest of the suite, not by hand.
func TestJSStorageRowsAreComplete(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/storage_rows.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("storage row checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// What the progress strip says once every cell has been sent and the answer is
// still coming back. Run with the rest of the suite, not by hand.
func TestJSTelegramProgressLabel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_progress.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("progress label checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// Signing out must not take the operation lock, or pressing it during a fetch
// does nothing. Run with the rest of the suite, not by hand.
func TestJSTelegramLogout(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_logout.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("logout checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// Every field the backend sends on a conversation row must be taken when the
// row is refreshed. Run with the rest of the suite, not by hand.
func TestJSTelegramDialogFieldsRefresh(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_dialog_fields.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("dialog field checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// The progress strip must not move the lists when it comes and goes. Run with
// the rest of the suite, not by hand.
func TestJSTelegramProgressDoesNotJump(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_nojump.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("progress jump checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// A muted conversation must look muted. Run with the rest of the suite, not
// by hand.
func TestJSTelegramMutedRow(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_muted_row.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("muted row checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// Each ⟳ must refresh the thing it sits on. Run with the rest of the suite,
// not by hand.
func TestJSTelegramRefreshTarget(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_refresh_target.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("refresh target checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// The progress strip must not strobe when operations are fast. Run with the
// rest of the suite, not by hand.
func TestJSTelegramProgressHold(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/tel_prog_hold.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("progress hold checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}

// ✓ must be able to become ✓✓ on its own. Run with the rest of the suite, not
// by hand.
func TestJSChatDeliveryTicks(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "jstest/chat_ticks.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("chat tick checks failed:\n%s", out)
	}
	t.Logf("%s", out)
}
