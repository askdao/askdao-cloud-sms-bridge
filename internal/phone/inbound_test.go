package phone

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// Test data mirrors what the real phone sends: short local integer ids for conversations,
// messages and participants; numbers usually "+1..." but not always.
const (
	groupID = "24"
	dmID    = "31"
	textA   = "/askdao zzBodyA what is up"
)

func participant(pid, number string) *gmproto.Participant {
	return &gmproto.Participant{ID: &gmproto.SmallInfo{ParticipantID: pid, Number: number}, IsVisible: true}
}

func groupConv(id string, typ gmproto.ConversationType) *gmproto.Conversation {
	return &gmproto.Conversation{
		ConversationID: id, Name: "Test group " + id, IsGroupChat: true, Type: typ, DefaultOutgoingID: "1",
		Participants: []*gmproto.Participant{
			{ID: &gmproto.SmallInfo{ParticipantID: "1", Number: "+15550000000"}, IsMe: true},
			participant("5", "+15550000001"),
			participant("6", "5550000002"), // no country code
		},
	}
}

func dmConv(id string) *gmproto.Conversation {
	return &gmproto.Conversation{
		ConversationID: id, Name: "Someone", Type: gmproto.ConversationType_RCS, DefaultOutgoingID: "1",
		Participants: []*gmproto.Participant{participant("8", "+15550000008")},
	}
}

// msg builds a message event as libgm delivers it.
func msg(id, conv, pid, text string, st gmproto.MessageStatusType) *libgm.WrappedMessage {
	m := &gmproto.Message{
		MessageID: id, ConversationID: conv, ParticipantID: pid, Timestamp: time.Now().UnixMicro(),
		MessageStatus: &gmproto.MessageStatus{Status: st},
	}
	if text != "" {
		m.MessageInfo = []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: text}}}}
	}
	return &libgm.WrappedMessage{Message: m}
}

func withMedia(w *libgm.WrappedMessage) *libgm.WrappedMessage {
	w.MessageInfo = append(w.MessageInfo, &gmproto.MessageInfo{Data: &gmproto.MessageInfo_MediaContent{
		MediaContent: &gmproto.MediaContent{MimeType: "image/jpeg", MediaID: "media-1"},
	}})
	return w
}

// copyOf re-sends a message the way the phone does after a status or reaction change.
func copyOf(w *libgm.WrappedMessage, st gmproto.MessageStatusType) *libgm.WrappedMessage {
	m := proto.Clone(w.Message).(*gmproto.Message)
	m.MessageStatus = &gmproto.MessageStatus{Status: st}
	return &libgm.WrappedMessage{Message: m}
}

type inboundSink struct {
	mu  sync.Mutex
	got []Inbound
}

func (s *inboundSink) add(in Inbound) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, in)
}

func (s *inboundSink) all() []Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Inbound(nil), s.got...)
}

func (s *inboundSink) ids() []string {
	var ids []string
	for _, in := range s.all() {
		ids = append(ids, in.ID)
	}
	return ids
}

// readyManager is a connected manager with a sink for inbound messages and the group
// and direct conversations known.
func readyManager(t *testing.T) (*harness, *fakeClient, *inboundSink) {
	t.Helper()
	sink := &inboundSink{}
	h := startManager(t, nil)
	h.mgr.OnInbound(sink.add)
	c := h.connected(0)
	c.emit(&events.ListenRecovered{})
	c.emit(&gmproto.UserAlertEvent{AlertType: gmproto.AlertType_BROWSER_ACTIVE})
	c.emit(&gmproto.Settings{SIMCards: []*gmproto.SIMCard{{
		SIMParticipant: &gmproto.SIMParticipant{ID: "1"},
		SIMData:        &gmproto.SIMData{SIMPayload: &gmproto.SIMPayload{Two: 2, SIMNumber: 1}},
	}}})
	c.emit(groupConv(groupID, gmproto.ConversationType_RCS))
	c.emit(dmConv(dmID))
	return h, c, sink
}

var markerSeq int

// settle emits a valid marker message and waits for it: the worker is first-in first-out,
// so anything admitted before the marker has been delivered by then.
func settle(t *testing.T, c *fakeClient, sink *inboundSink) {
	t.Helper()
	markerSeq++
	id := fmt.Sprintf("8%04d", markerSeq)
	c.emit(msg(id, groupID, "5", "marker", gmproto.MessageStatusType_INCOMING_COMPLETE))
	eventually(t, "marker "+id, func() bool {
		ids := sink.ids()
		return len(ids) > 0 && ids[len(ids)-1] == id
	})
}

func nonMarkers(sink *inboundSink) []Inbound {
	var out []Inbound
	for _, in := range sink.all() {
		if in.Content != "marker" {
			out = append(out, in)
		}
	}
	return out
}

func TestInboundForwardsNewMessageFromSomeoneElse(t *testing.T) {
	h, c, sink := readyManager(t)
	w := msg("101", groupID, "5", textA, gmproto.MessageStatusType_INCOMING_COMPLETE)
	c.emit(w)
	settle(t, c, sink)
	got := nonMarkers(sink)
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	want := Inbound{
		ID: "101", ChatID: groupID, Sender: "+15550000001", Content: textA,
		Timestamp: time.UnixMicro(w.GetTimestamp()).Unix(), IsGroup: true, ChatTitle: "Test group " + groupID,
	}
	if got[0] != want {
		t.Errorf("got %+v\nwant %+v", got[0], want)
	}
	assertNoText(t, h)
}

func assertNoText(t *testing.T, h *harness) {
	t.Helper()
	if strings.Contains(h.logs.String(), "zzBody") {
		t.Error("logs contain message text")
	}
	assertNoSecrets(t, "logs", h.logs.String())
}

// Each case is dropped for its own reason; the marker that follows proves the pipeline kept working.
func TestInboundDropsWhatIsNotANewMessageFromSomeoneElse(t *testing.T) {
	h, c, sink := readyManager(t)
	h.mgr.inbox.rememberSent("tmp-ours")

	old := msg("201", groupID, "5", textA, gmproto.MessageStatusType_INCOMING_COMPLETE)
	old.IsOld = true
	// The tmp id alone marks it as ours, whatever its status says.
	echoByTmp := msg("203", groupID, "5", textA, gmproto.MessageStatusType_INCOMING_COMPLETE)
	echoByTmp.TmpID = "tmp-ours"
	stale := msg("208", groupID, "5", textA, gmproto.MessageStatusType_INCOMING_COMPLETE)
	stale.Timestamp = time.Now().Add(-time.Hour).UnixMicro()

	cases := []struct {
		name   string
		evt    *libgm.WrappedMessage
		reason string
	}{
		{"replayed after reconnect (IsOld)", old, dropOld},
		{"own echo (outgoing status)", msg("202", groupID, "1", textA, gmproto.MessageStatusType_OUTGOING_COMPLETE), dropOwn},
		{"own echo (our tmp id)", echoByTmp, dropOwn},
		{"system: RCS group created", msg("204", groupID, "", "", gmproto.MessageStatusType_TOMBSTONE_RCS_GROUP_CREATED), dropSystem},
		{"system: switched to text", msg("205", groupID, "", "", gmproto.MessageStatusType_TOMBSTONE_PROTOCOL_SWITCH_TO_TEXT), dropSystem},
		{"system: participant joined", msg("206", groupID, "6", "", gmproto.MessageStatusType_TOMBSTONE_PARTICIPANT_JOINED), dropSystem},
		{"deleted", msg("207", groupID, "5", "", gmproto.MessageStatusType_MESSAGE_DELETED), dropDeleted},
		{"stale: sent before the bridge connected", stale, dropStale},
		{"still downloading", msg("209", groupID, "5", textA, gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING), dropIncomplete},
		{"attachment only", withMedia(msg("210", groupID, "5", "", gmproto.MessageStatusType_INCOMING_COMPLETE)), dropNoText},
	}
	for _, tc := range cases {
		c.emit(tc.evt)
		settle(t, c, sink)
		if got := nonMarkers(sink); len(got) != 0 {
			t.Fatalf("%s: forwarded %+v", tc.name, got)
		}
		if !droppedWith(h.logs.String(), tc.evt.GetMessageID(), tc.reason) {
			t.Errorf("%s: no log line dropping message %s with reason %q", tc.name, tc.evt.GetMessageID(), tc.reason)
		}
	}
	if !strings.Contains(h.logs.String(), "attachment-only message not forwarded") {
		t.Error("the dropped attachment should be logged")
	}
	assertNoText(t, h)
}

func droppedWith(logs, id, reason string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"message_id":"`+id+`"`) && strings.Contains(line, `"reason":"`+reason+`"`) &&
			strings.Contains(line, "inbound dropped") {
			return true
		}
	}
	return false
}

// The phone re-sends the whole message on every status or reaction change.
func TestInboundResendsAreForwardedOnce(t *testing.T) {
	_, c, sink := readyManager(t)
	w := msg("301", groupID, "5", textA, gmproto.MessageStatusType_INCOMING_COMPLETE)
	c.emit(w)
	c.emit(copyOf(w, gmproto.MessageStatusType_INCOMING_DISPLAYED))
	reacted := copyOf(w, gmproto.MessageStatusType_INCOMING_DISPLAYED)
	reacted.Reactions = []*gmproto.ReactionEntry{{Data: gmproto.MakeReactionData("❤️"), ParticipantIDs: []string{"6"}}}
	c.emit(reacted)
	settle(t, c, sink)
	if got := nonMarkers(sink); len(got) != 1 {
		t.Fatalf("forwarded %d times, want once", len(got))
	}
}

// libgm only de-duplicates its last 8 updates; the bridge keeps its own record.
func TestInboundDedupBeyondLibraryWindow(t *testing.T) {
	_, c, sink := readyManager(t)
	var all []*libgm.WrappedMessage
	for i := 0; i < 20; i++ {
		w := msg(fmt.Sprintf("4%02d", i), groupID, "5", textA, gmproto.MessageStatusType_INCOMING_COMPLETE)
		all = append(all, w)
		c.emit(w)
	}
	for _, w := range all {
		c.emit(copyOf(w, gmproto.MessageStatusType_INCOMING_DISPLAYED))
	}
	settle(t, c, sink)
	if got := nonMarkers(sink); len(got) != 20 {
		t.Fatalf("got %d, want 20", len(got))
	}
}

// A message replayed as old, then re-sent as new because someone reacted to it, stays dropped.
func TestInboundOldMessageStaysDroppedAfterReaction(t *testing.T) {
	_, c, sink := readyManager(t)
	w := msg("501", groupID, "5", textA, gmproto.MessageStatusType_INCOMING_COMPLETE)
	w.IsOld = true
	c.emit(w)
	c.emit(copyOf(w, gmproto.MessageStatusType_INCOMING_DISPLAYED)) // IsOld false now
	settle(t, c, sink)
	if got := nonMarkers(sink); len(got) != 0 {
		t.Fatalf("forwarded %+v", got)
	}
}

// Not yet complete is not final: the complete version is forwarded, once.
func TestInboundIncompleteThenComplete(t *testing.T) {
	_, c, sink := readyManager(t)
	w := withMedia(msg("601", groupID, "5", textA, gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING))
	c.emit(w)
	settle(t, c, sink)
	if got := nonMarkers(sink); len(got) != 0 {
		t.Fatalf("forwarded while downloading: %+v", got)
	}
	c.emit(copyOf(w, gmproto.MessageStatusType_INCOMING_COMPLETE))
	c.emit(copyOf(w, gmproto.MessageStatusType_INCOMING_DISPLAYED))
	settle(t, c, sink)
	got := nonMarkers(sink)
	if len(got) != 1 || got[0].Content != textA {
		t.Fatalf("got %+v", got)
	}
}

// libgm calls the handler synchronously: it must hand off and return, even while a
// conversation lookup from the phone is still pending.
func TestInboundCallbackDoesNotWaitForRequests(t *testing.T) {
	_, c, sink := readyManager(t)
	gate := make(chan struct{})
	c.get(func(f *fakeClient) {
		f.getConvGate = gate
		f.convs["77"] = groupConv("77", gmproto.ConversationType_SMS)
	})
	returned := make(chan struct{})
	go func() {
		c.emit(msg("701", "77", "5", textA, gmproto.MessageStatusType_INCOMING_COMPLETE))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("the event handler blocked on a request")
	}
	eventually(t, "lookup started", func() bool {
		n := 0
		c.get(func(f *fakeClient) { n = f.getConvCalls })
		return n == 1
	})
	if len(sink.all()) != 0 {
		t.Fatal("delivered before the conversation was known")
	}
	close(gate)
	eventually(t, "delivery after lookup", func() bool { return len(sink.all()) == 1 })
	if in := sink.all()[0]; !in.IsGroup || in.ChatID != "77" {
		t.Errorf("got %+v", in)
	}
}

func TestInboundDetails(t *testing.T) {
	h, c, sink := readyManager(t)
	// Sender without "+": normalized. Number only in the conversation's participant list.
	c.emit(msg("801", groupID, "6", "hello", gmproto.MessageStatusType_INCOMING_COMPLETE))
	// iPhone replies arrive as plain text with a leading reply line and no reply reference.
	c.emit(msg("802", groupID, "5", "Replied to a message:\n/askdao zzBodyB and more", gmproto.MessageStatusType_INCOMING_COMPLETE))
	// Text with an attachment: the text goes, the attachment does not.
	c.emit(withMedia(msg("803", groupID, "5", "look at this", gmproto.MessageStatusType_INCOMING_COMPLETE)))
	// Direct conversation: not a group, no title.
	c.emit(msg("804", dmID, "8", "start bind_x", gmproto.MessageStatusType_INCOMING_COMPLETE))
	// Sender number given on the message itself.
	own := msg("805", groupID, "9", "hi", gmproto.MessageStatusType_INCOMING_COMPLETE)
	own.SenderParticipant = participant("9", "+1 555 000 0009")
	c.emit(own)
	settle(t, c, sink)
	got := nonMarkers(sink)
	if len(got) != 5 {
		t.Fatalf("got %d: %+v", len(got), got)
	}
	if got[0].Sender != "+15550000002" {
		t.Errorf("sender = %q, want +15550000002", got[0].Sender)
	}
	if got[1].Content != "/askdao zzBodyB and more" || !got[1].ReplyFallback {
		t.Errorf("reply: %q, reply_fallback=%v", got[1].Content, got[1].ReplyFallback)
	}
	for _, i := range []int{0, 2, 3, 4} {
		if got[i].ReplyFallback {
			t.Errorf("message %s: reply_fallback set without a reply line", got[i].ID)
		}
	}
	if got[2].Content != "look at this" {
		t.Errorf("content = %q", got[2].Content)
	}
	if got[3].IsGroup || got[3].ChatTitle != "" || got[3].ChatID != dmID || got[3].Sender != "+15550000008" {
		t.Errorf("direct message: %+v", got[3])
	}
	if got[4].Sender != "+15550000009" {
		t.Errorf("sender = %q", got[4].Sender)
	}
	if !strings.Contains(h.logs.String(), "attachments not forwarded") {
		t.Error("the dropped attachment should be logged")
	}
	assertNoText(t, h)
}

// An iPhone reply in an RCS group reaches Android as "Replied to a message:\n<text>".
// Only that exact leading line is removed, once; anything else is forwarded untouched.
func TestInboundReplyFallback(t *testing.T) {
	h, c, sink := readyManager(t)
	cases := []struct {
		name, text, want string
		stripped         bool
	}{
		{"leading reply line", "Replied to a message:\n/askdao zzBodyR1", "/askdao zzBodyR1", true},
		{"only once", "Replied to a message:\nReplied to a message:\n/askdao zzBodyR2", "Replied to a message:\n/askdao zzBodyR2", true},
		{"in the middle", "/askdao zzBodyR3\nReplied to a message:\nmore", "/askdao zzBodyR3\nReplied to a message:\nmore", false},
		{"lower case", "replied to a message:\n/askdao zzBodyR4", "replied to a message:\n/askdao zzBodyR4", false},
		{"upper case", "REPLIED TO A MESSAGE:\n/askdao zzBodyR5", "REPLIED TO A MESSAGE:\n/askdao zzBodyR5", false},
		{"no colon", "Replied to a message\n/askdao zzBodyR6", "Replied to a message\n/askdao zzBodyR6", false},
		{"period instead of colon", "Replied to a message.\n/askdao zzBodyR7", "Replied to a message.\n/askdao zzBodyR7", false},
		{"space instead of newline", "Replied to a message: /askdao zzBodyR8", "Replied to a message: /askdao zzBodyR8", false},
		{"CRLF", "Replied to a message:\r\n/askdao zzBodyR9", "Replied to a message:\r\n/askdao zzBodyR9", false},
		{"leading space", " Replied to a message:\n/askdao zzBodyRa", " Replied to a message:\n/askdao zzBodyRa", false},
	}
	for i, tc := range cases {
		id := fmt.Sprintf("11%02d", i)
		c.emit(msg(id, groupID, "5", tc.text, gmproto.MessageStatusType_INCOMING_COMPLETE))
		settle(t, c, sink)
		got := nonMarkers(sink)
		if len(got) != i+1 || got[i].ID != id {
			t.Fatalf("%s: not forwarded (%d messages)", tc.name, len(got))
		}
		if got[i].Content != tc.want || got[i].ReplyFallback != tc.stripped {
			t.Errorf("%s: content %q reply_fallback=%v, want %q %v", tc.name, got[i].Content, got[i].ReplyFallback, tc.want, tc.stripped)
		}
	}

	// Nothing left after the reply line: no text, dropped like an attachment-only message.
	before := len(nonMarkers(sink))
	c.emit(msg("1199", groupID, "5", "Replied to a message:\n", gmproto.MessageStatusType_INCOMING_COMPLETE))
	settle(t, c, sink)
	if got := nonMarkers(sink); len(got) != before {
		t.Fatalf("an empty reply was forwarded: %+v", got[len(got)-1])
	}
	if !droppedWith(h.logs.String(), "1199", dropNoText) {
		t.Error("the empty reply should be logged as dropped with reason no_text")
	}
	assertNoText(t, h)
}

// Without the conversation the bridge cannot tell a group from a direct message: drop it.
func TestInboundUnknownConversationIsDropped(t *testing.T) {
	h, c, sink := readyManager(t)
	w := msg("901", "99", "5", textA, gmproto.MessageStatusType_INCOMING_COMPLETE) // GetConversation fails
	w.SenderParticipant = participant("5", "+15550000001")                         // only the sender is known
	c.emit(w)
	settle(t, c, sink)
	if got := nonMarkers(sink); len(got) != 0 {
		t.Fatalf("forwarded %+v", got)
	}
	if !strings.Contains(h.logs.String(), "conversation unknown") {
		t.Error("the drop should be logged")
	}
}

func TestNormalizeNumber(t *testing.T) {
	cases := map[string]string{
		"+15550000000":        "+15550000000",
		"15550000000":         "+15550000000",
		"5550000000":          "+15550000000",
		"(555) 000-0000":      "+15550000000",
		"+1 555-000-0000":     "+15550000000",
		"+44 20 7946 0000":    "+442079460000",
		"442079460000":        "+442079460000",
		"12345":               "12345",
		" Member@Example.COM": "member@example.com",
		"":                    "",
	}
	for in, want := range cases {
		if got := NormalizeNumber(in); got != want {
			t.Errorf("NormalizeNumber(%q) = %q, want %q", in, got, want)
		}
	}
}
