package tgbridge

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"context"
	"github.com/gotd/td/tg"
	"time"
)

// Telegram models channels, supergroups and forums all as "channel" and tells
// them apart by flags. The UI lists them differently, so this mapping is where
// a wrong row type would come from.
func TestPeerKindFromChannelFlags(t *testing.T) {
	for _, tc := range []struct {
		name      string
		megagroup bool
		forum     bool
		want      PeerKind
	}{
		{"broadcast channel", false, false, PeerChannel},
		{"supergroup", true, false, PeerGroup},
		{"forum", true, true, PeerForum},
		// Forum without megagroup is not a thing Telegram produces; it must
		// not silently become a forum.
		{"broadcast with forum flag", false, true, PeerChannel},
	} {
		got := peerFromChannel(&tg.Channel{ID: 7, AccessHash: 9, Title: "x", Megagroup: tc.megagroup, Forum: tc.forum})
		if got.Kind != tc.want {
			t.Errorf("%s: kind = %s, want %s", tc.name, got.Kind, tc.want)
		}
		if got.AccessHash != 9 {
			t.Errorf("%s: access hash lost", tc.name)
		}
	}
}

func TestPeerFromUserNaming(t *testing.T) {
	for _, tc := range []struct {
		name string
		user *tg.User
		want string
		kind PeerKind
	}{
		{"both names", &tg.User{ID: 1, FirstName: "Ada", LastName: "L"}, "Ada L", PeerUser},
		{"first only", &tg.User{ID: 1, FirstName: "Ada"}, "Ada", PeerUser},
		{"last only", &tg.User{ID: 1, LastName: "L"}, "L", PeerUser},
		{"username fallback", &tg.User{ID: 1, Username: "ada"}, "ada", PeerUser},
		{"bot", &tg.User{ID: 1, FirstName: "Helper", Bot: true}, "Helper", PeerBot},
	} {
		got := peerFromUser(tc.user)
		if got.Title != tc.want {
			t.Errorf("%s: title = %q, want %q", tc.name, got.Title, tc.want)
		}
		if got.Kind != tc.kind {
			t.Errorf("%s: kind = %s, want %s", tc.name, got.Kind, tc.kind)
		}
	}
}

// A legacy group has no access hash and must be addressed as InputPeerChat; a
// supergroup has one and must be addressed as InputPeerChannel. Getting this
// wrong makes every request for that chat fail.
func TestInputPeerAddressing(t *testing.T) {
	for _, tc := range []struct {
		name string
		peer Peer
		want string
	}{
		{"user", Peer{Kind: PeerUser, ID: 1, AccessHash: 2}, "*tg.InputPeerUser"},
		{"bot", Peer{Kind: PeerBot, ID: 1, AccessHash: 2}, "*tg.InputPeerUser"},
		{"channel", Peer{Kind: PeerChannel, ID: 1, AccessHash: 2}, "*tg.InputPeerChannel"},
		{"forum", Peer{Kind: PeerForum, ID: 1, AccessHash: 2}, "*tg.InputPeerChannel"},
		{"supergroup", Peer{Kind: PeerGroup, ID: 1, AccessHash: 2}, "*tg.InputPeerChannel"},
		{"legacy group", Peer{Kind: PeerGroup, ID: 1}, "*tg.InputPeerChat"},
	} {
		got := typeName(tc.peer.inputPeer())
		if got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *tg.InputPeerUser:
		return "*tg.InputPeerUser"
	case *tg.InputPeerChannel:
		return "*tg.InputPeerChannel"
	case *tg.InputPeerChat:
		return "*tg.InputPeerChat"
	}
	return "unknown"
}

// Dialogs reference entities that arrive in parallel lists; a row whose entity
// is missing must be dropped, not rendered with a blank title.
func TestDialogsFromResolvesEntities(t *testing.T) {
	res := &tg.MessagesDialogs{
		Dialogs: []tg.DialogClass{
			&tg.Dialog{Peer: &tg.PeerUser{UserID: 1}, UnreadCount: 3, TopMessage: 11},
			&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 2}, TopMessage: 22},
			&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 3}, TopMessage: 33, Pinned: true},
			// Entity not included -> must be skipped.
			&tg.Dialog{Peer: &tg.PeerUser{UserID: 99}},
		},
		Users: []tg.UserClass{&tg.User{ID: 1, FirstName: "Ada"}},
		Chats: []tg.ChatClass{
			&tg.Channel{ID: 2, Title: "News"},                  // broadcast
			&tg.Channel{ID: 3, Title: "Team", Megagroup: true}, // supergroup
		},
		Messages: []tg.MessageClass{
			&tg.Message{ID: 11, PeerID: &tg.PeerUser{UserID: 1}, Message: "hi"},
			&tg.Message{ID: 33, PeerID: &tg.PeerChannel{ChannelID: 3}, Message: "standup"},
		},
	}

	// Every kind is returned: the UI groups them into tabs, so a row dropped
	// here would make a tab permanently empty.
	got := dialogsFrom(res, NotifyDefaults{})
	if len(got) != 3 {
		t.Fatalf("got %d dialogs, want 3 (user + supergroup + channel)", len(got))
	}
	if got[0].Peer.Title != "Ada" || got[0].Unread != 3 || got[0].Preview != "hi" {
		t.Fatalf("user row wrong: %+v", got[0])
	}
	byKind := map[PeerKind]Dialog{}
	for _, d := range got {
		byKind[d.Peer.Kind] = d
	}
	if g := byKind[PeerGroup]; g.Preview != "standup" || !g.Pinned {
		t.Fatalf("supergroup row wrong: %+v", g)
	}
	if byKind[PeerChannel].Peer.Title != "News" {
		t.Fatalf("broadcast channel missing: %+v", got)
	}
}

func TestDialogsFromSliceAndUnknown(t *testing.T) {
	slice := &tg.MessagesDialogsSlice{
		Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerUser{UserID: 1}}},
		Users:   []tg.UserClass{&tg.User{ID: 1, FirstName: "Ada"}},
	}
	if got := dialogsFrom(slice, NotifyDefaults{}); len(got) != 1 {
		t.Fatalf("slice form: got %d rows, want 1", len(got))
	}
	if got := dialogsFrom(&tg.MessagesDialogsNotModified{}, NotifyDefaults{}); got != nil {
		t.Fatalf("not-modified should map to nil, got %v", got)
	}
}

// A service message is kept and marked, so the "newer than" cursor can move
// past it; the UI draws no bubble for one.
func TestMessagesFromKeepsServiceMessagesMarked(t *testing.T) {
	withMedia := &tg.Message{ID: 3, Date: 101}
	withMedia.SetMedia(&tg.MessageMediaPhoto{}) // setter: the flag gates GetMedia
	res := &tg.MessagesMessagesSlice{Messages: []tg.MessageClass{
		&tg.Message{ID: 1, Date: 100, Message: "hello", Out: true},
		&tg.MessageService{ID: 2, Date: 100},
		withMedia,
	}}
	got := messagesFrom(res)
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	if !got[0].Out || got[0].Text != "hello" || got[0].Service {
		t.Fatalf("first message wrong: %+v", got[0])
	}
	if !got[1].Service || got[1].ID != 2 || got[1].Text != "" {
		t.Fatalf("service message wrong: %+v", got[1])
	}
	if got[2].Media != MediaPhoto {
		t.Fatalf("media kind = %q, want %q", got[2].Media, MediaPhoto)
	}
}

func TestMessagesFromChannelForm(t *testing.T) {
	// Use the setter: gotd gates optional fields behind flag bits, so a
	// directly-assigned FromID would be invisible to GetFromID -- exactly as
	// it would be if we built the struct wrong in production.
	msg := &tg.Message{ID: 5, Message: "post"}
	msg.SetFromID(&tg.PeerChannel{ChannelID: 42})
	got := messagesFrom(&tg.MessagesChannelMessages{Messages: []tg.MessageClass{msg}})
	if len(got) != 1 || got[0].FromID != 42 {
		t.Fatalf("channel messages mapped wrong: %+v", got)
	}

	// A message with no sender set must simply have no sender.
	plain := messagesFrom(&tg.MessagesMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 6, Message: "anon"},
	}})
	if len(plain) != 1 || plain[0].FromID != 0 {
		t.Fatalf("absent sender mapped wrong: %+v", plain)
	}
}

// Nothing may be asked of a bridge that is not connected.
func TestBridgeRequiresConnection(t *testing.T) {
	b := NewBridge()
	if b.Running() {
		t.Fatal("idle bridge reports running")
	}
	ctx := t.Context()
	if _, err := b.Dialogs(ctx, DialogFilter{}); err != ErrNotConnected {
		t.Errorf("Dialogs: %v", err)
	}
	if _, err := b.History(ctx, Peer{}, 10, 0, 0); err != ErrNotConnected {
		t.Errorf("History: %v", err)
	}
	if err := b.DeleteChat(ctx, Peer{}, false); err != ErrNotConnected {
		t.Errorf("DeleteChat: %v", err)
	}
	if err := b.MarkRead(ctx, Peer{}, 0); err != ErrNotConnected {
		t.Errorf("MarkRead: %v", err)
	}
	if _, err := b.ResolveUsername(ctx, "someone"); err != ErrNotConnected {
		t.Errorf("ResolveUsername: %v", err)
	}
	if _, err := b.SendText(ctx, Peer{}, "x", 1, 0); err != ErrNotConnected {
		t.Errorf("SendText: %v", err)
	}
	if _, err := b.AuthStatus(ctx); err != ErrNotConnected {
		t.Errorf("AuthStatus: %v", err)
	}
	// Stop on an idle bridge is a no-op, not a panic.
	b.Stop()
}

// Paging: a full page must hand back a cursor that can address the oldest row
// again -- including its access hash, without which Telegram refuses the peer.
func TestDialogPageCursor(t *testing.T) {
	// Two rows, limit 2 => a full page, so another one probably exists. The
	// oldest row is a supergroup whose numeric id collides with the user's, to
	// prove the cursor is matched by type and id rather than id alone.
	res := &tg.MessagesDialogsSlice{
		Count: 97,
		Dialogs: []tg.DialogClass{
			&tg.Dialog{Peer: &tg.PeerUser{UserID: 5}, TopMessage: 90},
			&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 5}, TopMessage: 40},
		},
		Users: []tg.UserClass{&tg.User{ID: 5, FirstName: "Ada", AccessHash: 11}},
		Chats: []tg.ChatClass{&tg.Channel{
			ID: 5, Title: "Team", AccessHash: 22, Megagroup: true,
		}},
		Messages: []tg.MessageClass{
			&tg.Message{ID: 90, PeerID: &tg.PeerUser{UserID: 5}, Date: 900},
			&tg.Message{ID: 40, PeerID: &tg.PeerChannel{ChannelID: 5}, Date: 400},
		},
	}
	page := dialogPageFrom(res, 2, NotifyDefaults{})
	if !page.More {
		t.Fatal("a full page should report more")
	}
	if page.TotalKnown != 97 {
		t.Errorf("totalKnown = %d, want 97", page.TotalKnown)
	}
	if page.NextID != 40 || page.NextDate != 400 {
		t.Errorf("cursor = id %d date %d, want 40/400", page.NextID, page.NextDate)
	}
	if page.NextPeer.Kind != PeerGroup || page.NextPeer.AccessHash != 22 {
		t.Errorf("cursor peer = %+v, want the supergroup with hash 22", page.NextPeer)
	}

	// A short page is the end of the list: no cursor, nothing more to ask for.
	short := dialogPageFrom(res, 50, NotifyDefaults{})
	if short.More || short.NextID != 0 || short.NextPeer.ID != 0 {
		t.Errorf("short page should end paging, got %+v", short)
	}
}

// A page size is clamped where Telegram is actually called, so no caller can
// ask for an absurd one over a link where every row costs queries.
func TestPageLimitsAreClamped(t *testing.T) {
	if got := clampLimit(0, defaultDialogLimit, maxDialogLimit); got != defaultDialogLimit {
		t.Errorf("zero limit = %d, want the default %d", got, defaultDialogLimit)
	}
	if got := clampLimit(-5, defaultDialogLimit, maxDialogLimit); got != defaultDialogLimit {
		t.Errorf("negative limit = %d, want the default", got)
	}
	if got := clampLimit(10_000, defaultHistoryLimit, maxHistoryLimit); got != maxHistoryLimit {
		t.Errorf("huge limit = %d, want the cap %d", got, maxHistoryLimit)
	}
	if got := clampLimit(25, defaultHistoryLimit, maxHistoryLimit); got != 25 {
		t.Errorf("sane limit = %d, want it unchanged", got)
	}
}

// Nothing is downloaded, so the KIND of an attachment is all the user gets:
// "a sticker went past" beats an unexplained gap in the conversation.
func TestMediaKinds(t *testing.T) {
	doc := func(attrs ...tg.DocumentAttributeClass) *tg.MessageMediaDocument {
		m := &tg.MessageMediaDocument{}
		m.SetDocument(&tg.Document{ID: 1, Size: 4096, Attributes: attrs})
		return m
	}
	for _, tc := range []struct {
		name  string
		media tg.MessageMediaClass
		kind  string
		label string
	}{
		{"photo", &tg.MessageMediaPhoto{}, MediaPhoto, ""},
		{"sticker", doc(&tg.DocumentAttributeSticker{Alt: "😀"}), MediaSticker, "😀"},
		{"voice", doc(&tg.DocumentAttributeAudio{Voice: true}), MediaVoice, ""},
		{"music", doc(&tg.DocumentAttributeAudio{Title: "Song"}), MediaAudio, "Song"},
		{"video", doc(&tg.DocumentAttributeVideo{}), MediaVideo, ""},
		{"video note", doc(&tg.DocumentAttributeVideo{RoundMessage: true}), MediaVideoNote, ""},
		{"gif", doc(&tg.DocumentAttributeAnimated{}), MediaGIF, ""},
		{"file", doc(&tg.DocumentAttributeFilename{FileName: "report.pdf"}), MediaFile, "report.pdf"},
		{"geo", &tg.MessageMediaGeo{}, MediaGeo, ""},
		{"poll", &tg.MessageMediaPoll{Poll: tg.Poll{Question: tg.TextWithEntities{Text: "Which?"}}}, MediaPoll, "Which?"},
		// A link preview is not an attachment: the link is in the text, and
		// marking it would put a media tag on every message carrying a URL.
		{"link preview", &tg.MessageMediaWebPage{}, "", ""},
	} {
		kind, name, _ := mediaOf(tc.media)
		if kind != tc.kind || name != tc.label {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, kind, name, tc.kind, tc.label)
		}
	}
	// A file keeps its size, so the user can tell a note from a 4 GB video.
	if _, _, size := mediaOf(doc(&tg.DocumentAttributeFilename{FileName: "a.bin"})); size != 4096 {
		t.Errorf("file size = %d, want 4096", size)
	}
}

// A bot's inline keyboard is listed so the message still makes sense; only the
// buttons that lead somewhere carry a URL.
func TestInlineButtonsAreListed(t *testing.T) {
	// Telegram moved the button's kind into its own field: the label is on
	// the button, what it does is in Type.
	markup := &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{
		{Buttons: []tg.KeyboardInlineButton{
			{Text: "Open", Type: &tg.InlineButtonTypeURL{URL: "https://example.org"}},
			{Text: "Next", Type: &tg.InlineButtonTypeCallback{}},
		}},
		{Buttons: []tg.KeyboardInlineButton{{Text: "Plain", Type: &tg.InlineButtonTypeDisabled{}}}},
	}}
	got := buttonsOf(markup)
	if len(got) != 3 {
		t.Fatalf("got %d buttons, want 3: %+v", len(got), got)
	}
	if got[0].Text != "Open" || got[0].URL == "" || got[0].Row != 0 {
		t.Errorf("url button wrong: %+v", got[0])
	}
	if got[1].Text != "Next" || got[1].URL != "" {
		t.Errorf("callback button should have no url: %+v", got[1])
	}
	if got[2].Row != 1 {
		t.Errorf("second row not recorded: %+v", got[2])
	}
	// A reply keyboard is a suggestion for the text box, not part of the
	// message, and must not be drawn as one.
	if buttonsOf(&tg.ReplyKeyboardMarkup{}) != nil {
		t.Error("reply keyboard should not be listed")
	}
}

// gotd writes a ping straight to its connection without checking that one
// exists, so a keepalive landing during a reconnect panics inside the library.
// A background keepalive must surface that as an error, never take the process
// down.
func TestSafeCallConvertsAPanicIntoAnError(t *testing.T) {
	err := safeCall(func() error { panic("nil pointer somewhere in the library") })
	if err == nil {
		t.Fatal("a panic came back as success")
	}
	if !strings.Contains(err.Error(), "connection unusable") {
		t.Errorf("unhelpful error: %v", err)
	}
	// An ordinary error still passes through untouched.
	want := errors.New("plain failure")
	if got := safeCall(func() error { return want }); !errors.Is(got, want) {
		t.Errorf("got %v, want the original error", got)
	}
	if got := safeCall(func() error { return nil }); got != nil {
		t.Errorf("success turned into %v", got)
	}
}

// Telegram refuses a message in a channel the user cannot post to, so the
// composer must not be offered there.
func TestPeerReadOnly(t *testing.T) {
	admin := func(c *tg.Channel, rights tg.ChatAdminRights) *tg.Channel {
		c.SetAdminRights(rights)
		return c
	}
	banned := func(c *tg.Channel, rights tg.ChatBannedRights) *tg.Channel {
		c.SetDefaultBannedRights(rights)
		return c
	}
	for _, tc := range []struct {
		name     string
		channel  *tg.Channel
		readOnly bool
	}{
		{"broadcast, plain reader", &tg.Channel{ID: 1, Broadcast: true}, true},
		{"broadcast, creator", &tg.Channel{ID: 1, Broadcast: true, Creator: true}, false},
		{"broadcast, admin who may post",
			admin(&tg.Channel{ID: 1, Broadcast: true}, tg.ChatAdminRights{PostMessages: true}), false},
		{"broadcast, admin who may not post",
			admin(&tg.Channel{ID: 1, Broadcast: true}, tg.ChatAdminRights{EditMessages: true}), true},
		{"supergroup, ordinary member", &tg.Channel{ID: 2, Megagroup: true}, false},
		{"supergroup, sending banned by default",
			banned(&tg.Channel{ID: 2, Megagroup: true}, tg.ChatBannedRights{SendMessages: true}), true},
		{"supergroup, admin despite the default ban",
			admin(banned(&tg.Channel{ID: 2, Megagroup: true}, tg.ChatBannedRights{SendMessages: true}),
				tg.ChatAdminRights{DeleteMessages: true}), false},
		{"left channel", &tg.Channel{ID: 3, Broadcast: true, Creator: true, Left: true}, true},
	} {
		if got := peerFromChannel(tc.channel).ReadOnly; got != tc.readOnly {
			t.Errorf("%s: readOnly = %v, want %v", tc.name, got, tc.readOnly)
		}
	}

	// A person or a bot is always writable.
	if peerFromUser(&tg.User{ID: 9, FirstName: "Ada"}).ReadOnly {
		t.Error("a private conversation reported read-only")
	}
	// A legacy group the user has left takes no messages either.
	if !peerFromChat(&tg.Chat{ID: 4, Title: "Old", Left: true}).ReadOnly {
		t.Error("a group the user left reported writable")
	}
}

// A reply costs twelve bytes more on the wire -- one DNS query -- and the
// information needed to SHOW one already rides along with every message, so
// neither direction is worth withholding on a slow link.
func TestReplyIsCarriedBothWays(t *testing.T) {
	msg := &tg.Message{ID: 5, Date: 100, Message: "answer"}
	hdr := &tg.MessageReplyHeader{}
	hdr.SetReplyToMsgID(3)
	hdr.SetQuoteText("the bit being answered")
	msg.SetReplyTo(hdr)

	got, ok := messageFrom(msg, nil)
	if !ok {
		t.Fatal("message not mapped")
	}
	if got.ReplyTo != 3 {
		t.Errorf("replyTo = %d, want 3", got.ReplyTo)
	}
	if got.ReplyQuote != "the bit being answered" {
		t.Errorf("quote = %q", got.ReplyQuote)
	}
	// An ordinary message carries neither.
	plain, _ := messageFrom(&tg.Message{ID: 6, Date: 101, Message: "hi"}, nil)
	if plain.ReplyTo != 0 || plain.ReplyQuote != "" {
		t.Errorf("a plain message reported a reply: %+v", plain)
	}
}

// The read mark is where the "new messages" line goes, and Telegram is the only
// honest source for it: it is the account's own mark, the same on every device.
// Deriving it here from the unread count would put the line in a different
// place on each one.
func TestDialogsFromCarriesReadMarks(t *testing.T) {
	res := &tg.MessagesDialogs{
		Dialogs: []tg.DialogClass{&tg.Dialog{
			Peer: &tg.PeerUser{UserID: 1}, TopMessage: 40,
			UnreadCount: 3, ReadInboxMaxID: 37, ReadOutboxMaxID: 39,
		}},
		Users: []tg.UserClass{&tg.User{ID: 1, FirstName: "Ada"}},
	}
	got := dialogsFrom(res, NotifyDefaults{})
	if len(got) != 1 {
		t.Fatalf("got %d dialogs, want 1", len(got))
	}
	if got[0].ReadInbox != 37 {
		t.Errorf("readInbox = %d, want 37 — the new-messages line has nowhere to go", got[0].ReadInbox)
	}
	if got[0].ReadOutbox != 39 {
		t.Errorf("readOutbox = %d, want 39", got[0].ReadOutbox)
	}
}

// The conversation's picture rides along with the list and must be taken.
//
// Telegram puts a stripped thumbnail inside the user and chat objects that
// come with messages.getDialogs, and that request has no flag to leave it out
// -- so these bytes cross the link whether or not anything reads them. They
// were read and thrown away. The real photo is a separate download from a
// media data centre, tens of round trips per avatar on this transport, which
// is why only this one is taken.
func TestDialogsCarryTheStrippedThumbnail(t *testing.T) {
	// A valid stripped payload: marker byte, then width and height.
	raw := append([]byte{0x01, 40, 40}, bytes.Repeat([]byte{0x20}, 64)...)

	photo := &tg.UserProfilePhoto{}
	photo.SetStrippedThumb(raw)
	u := &tg.User{ID: 1, FirstName: "Ada"}
	u.SetPhoto(photo)

	res := &tg.MessagesDialogs{
		Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerUser{UserID: 1}, TopMessage: 5}},
		Users:   []tg.UserClass{u},
	}
	got := dialogsFrom(res, NotifyDefaults{})
	if len(got) != 1 {
		t.Fatalf("got %d dialogs", len(got))
	}
	thumb := got[0].Peer.Thumb
	if len(thumb) == 0 {
		t.Fatal("the picture that came with the list was dropped")
	}
	// Expanded, not the raw stripped bytes: a browser cannot read those.
	if thumb[0] != 0xFF || thumb[1] != 0xD8 {
		t.Errorf("thumb does not start with a JPEG marker: % x", thumb[:4])
	}
	if len(thumb) <= len(raw) {
		t.Errorf("thumb is %d bytes, not expanded from %d", len(thumb), len(raw))
	}

	// A conversation with no picture keeps none, and must not invent one.
	plain := dialogsFrom(&tg.MessagesDialogs{
		Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerUser{UserID: 2}, TopMessage: 6}},
		Users:   []tg.UserClass{&tg.User{ID: 2, FirstName: "Bo"}},
	}, NotifyDefaults{})
	if len(plain) != 1 || plain[0].Peer.Thumb != nil {
		t.Errorf("a conversation with no picture got one: %+v", plain)
	}
}

// A conversation with no settings of its own follows its TYPE's default.
//
// Telegram keeps notification settings in two layers: a per-conversation
// override, and one default each for users, groups and channels. Muting a
// whole type is one tap, so most people never set a per-channel override --
// and a client that reads only the override reports every one of those
// channels as able to interrupt, which is the opposite of what the user asked
// for.
func TestMutedFollowsTheAccountDefault(t *testing.T) {
	none := tg.PeerNotifySettings{} // no override: both fields unset

	if mutedNow(none, true) != true {
		t.Error("a conversation with no override ignored a muted default; " +
			"this is the whole list reading as audible")
	}
	if mutedNow(none, false) != false {
		t.Error("a conversation with no override invented a mute")
	}

	// An override always wins over the default, in BOTH directions -- that is
	// what makes it an override.
	var loud tg.PeerNotifySettings
	loud.SetMuteUntil(0)
	if mutedNow(loud, true) {
		t.Error("a conversation deliberately left audible was silenced by the default")
	}

	var forever tg.PeerNotifySettings
	forever.SetMuteUntil(muteForever)
	if !mutedNow(forever, false) {
		t.Error("an explicit mute was lost")
	}

	// The other way Telegram says "no sound".
	var silent tg.PeerNotifySettings
	silent.SetSilent(true)
	if !mutedNow(silent, false) {
		t.Error("silent is still a mute")
	}

	// An expired mute is not a mute, and must not fall through to the default
	// either: it is an override that says "audible from now on".
	var expired tg.PeerNotifySettings
	expired.SetMuteUntil(int(time.Now().Add(-time.Hour).Unix()))
	if mutedNow(expired, true) {
		t.Error("an expired mute was treated as muted")
	}
}

// Each conversation type takes its own default.
func TestNotifyDefaultsPerType(t *testing.T) {
	def := NotifyDefaults{Users: false, Chats: true, Broadcasts: true}
	for _, tc := range []struct {
		kind PeerKind
		want bool
	}{
		{PeerUser, false},
		{PeerGroup, true},
		{PeerForum, true}, // a forum is a group with topics
		{PeerChannel, true},
	} {
		if got := def.For(tc.kind); got != tc.want {
			t.Errorf("%s took %v, want %v", tc.kind, got, tc.want)
		}
	}
}

// A read that did not answer is not an answer of "nothing is muted".
//
// The defaults are fetched once per connection and reused. Caching a FAILED
// read would turn one timeout into a whole connection's worth of silenced
// channels reported as audible -- the bug these exist to fix, except with
// nothing on screen or in the log to show for it.
func TestNotifyDefaultsAreNotCachedWhenTheReadFails(t *testing.T) {
	b := NewBridge()

	// A read where a call did not answer. Whatever it managed to collect is
	// not an answer, and must not become one.
	now := time.Now()
	if got := b.rememberNotifyDefaults(NotifyDefaults{Broadcasts: true}, true, now); got != (NotifyDefaults{}) {
		t.Fatalf("a failed read returned %+v, want the zero value", got)
	}
	b.mu.Lock()
	cached, at := b.notifyOK, b.notifyAt
	b.mu.Unlock()
	if cached {
		t.Fatal("a failed read was cached as authoritative; every conversation " +
			"would read as audible for the rest of the connection")
	}
	if at.IsZero() {
		t.Fatal("the failure was not recorded, so the next poll spends three more calls")
	}
	// ...and while that failure is fresh, it does not try again.
	if got := b.notifyDefaults(context.Background()); got != (NotifyDefaults{}) {
		t.Fatalf("retried immediately after a failure: %+v", got)
	}

	// A read that ANSWERED is cached, and is what later calls get back.
	if got := b.rememberNotifyDefaults(NotifyDefaults{Broadcasts: true}, false, now); !got.Broadcasts {
		t.Fatalf("a successful read returned %+v", got)
	}
	if got := b.notifyDefaults(context.Background()); !got.Broadcasts {
		t.Fatal("a cached answer was not reused")
	}

	// And a fresh connection re-reads: settings change while disconnected.
	b.mu.Lock()
	b.notifyOK, b.notifyAt = false, time.Time{}
	b.mu.Unlock()
	b.mu.Lock()
	stale := b.notifyOK
	b.mu.Unlock()
	if stale {
		t.Fatal("the defaults survived a reconnect")
	}
}

// Formatting arrives with the message, so carrying it costs nothing -- and
// without it a post written with bold, quotes or spoilers reads as flat text.
func TestEntitiesOfMapsWhatTheUICanRender(t *testing.T) {
	in := []tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 0, Length: 4},
		&tg.MessageEntityItalic{Offset: 5, Length: 3},
		&tg.MessageEntitySpoiler{Offset: 9, Length: 6},
		&tg.MessageEntityBlockquote{Offset: 16, Length: 5, Collapsed: true},
		&tg.MessageEntityPre{Offset: 22, Length: 4, Language: "go"},
		&tg.MessageEntityTextURL{Offset: 27, Length: 4, URL: "https://example.com"},
	}
	got := entitiesOf(in)
	if len(got) != len(in) {
		t.Fatalf("mapped %d of %d entities", len(got), len(in))
	}
	if got[3].Type != "blockquote" || !got[3].Collapsed {
		t.Errorf("the expandable blockquote lost its flag: %+v", got[3])
	}
	if got[4].URL != "go" {
		t.Errorf("a pre block lost its language: %+v", got[4])
	}

	// A link the renderer would turn into an href must have a scheme that can
	// only navigate.
	bad := entitiesOf([]tg.MessageEntityClass{
		&tg.MessageEntityTextURL{Offset: 0, Length: 4, URL: "javascript:alert(1)"},
		&tg.MessageEntityTextURL{Offset: 0, Length: 4, URL: "data:text/html,<script>"},
	})
	if len(bad) != 0 {
		t.Errorf("a script-scheme link was passed to the renderer: %+v", bad)
	}

	// An entity we do not understand must not change how the text reads.
	if n := len(entitiesOf([]tg.MessageEntityClass{&tg.MessageEntityBankCard{Offset: 0, Length: 4}})); n != 0 {
		t.Errorf("an unknown entity was mapped anyway (%d)", n)
	}
	// Nonsense ranges are dropped rather than handed on.
	if n := len(entitiesOf([]tg.MessageEntityClass{&tg.MessageEntityBold{Offset: -1, Length: 0}})); n != 0 {
		t.Errorf("a zero-length entity survived (%d)", n)
	}
	if entitiesOf(nil) != nil {
		t.Error("no entities should map to nil, not an empty slice")
	}
}
