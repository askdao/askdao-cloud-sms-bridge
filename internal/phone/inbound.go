// [INPUT]: libgm WrappedMessage, gmproto message statuses and conversations, number.go NormalizeNumber
// [OUTPUT]: Inbound; inbox (the inbound rules, run inside libgm's callback), idSet, messageText
//           (strips the iPhone "Replied to a message:" line), senderNumber
// [POS]: internal/phone's inbound side: manager.go calls inbox.admit in the callback, then its worker
//        looks up the conversation and hands an Inbound to the consumer (the WebSocket server)
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package phone

import (
	"strings"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// Inbound is a new message from someone else, ready for conductor.
type Inbound struct {
	ID        string // phone-local message id
	ChatID    string // phone-local conversation id
	Sender    string // see NormalizeNumber
	Content   string
	Timestamp int64 // unix seconds
	IsGroup   bool
	ChatTitle string // the conversation name for groups, "" otherwise
	// ReplyFallback: Content had a leading "Replied to a message:" line, which was removed.
	ReplyFallback bool
}

// Drop reasons, logged with the message id.
const (
	dropDuplicate  = "duplicate"      // already handled: a re-send after a status or reaction change
	dropOld        = "old"            // libgm's IsOld: replayed after (re)connecting
	dropOwn        = "own"            // the bridge's own message echoed back
	dropSystem     = "system"         // a tombstone: group created, protocol switched, member joined...
	dropDeleted    = "deleted"        // MESSAGE_DELETED
	dropUnknown    = "unknown_status" // a status outside the known ranges
	dropStale      = "stale"          // sent before the bridge first connected, though not marked old
	dropIncomplete = "incomplete"     // still downloading or failed; may arrive again complete
	dropNoText     = "no_text"        // no text: attachments only (not forwarded yet), or only the reply line
)

// staleMargin allows for the phone's clock running slightly behind this machine's.
const staleMargin = 10 * time.Second

// inbox decides which message events are new messages from someone else. It runs inside
// libgm's synchronous callback, so it only touches memory.
type inbox struct {
	mu     sync.Mutex
	seen   *idSet // message ids already decided for good
	sent   *idSet // tmp ids of messages this bridge sent
	cutoff time.Time
}

func newInbox() *inbox {
	return &inbox{seen: newIDSet(10000), sent: newIDSet(1000)}
}

// startCutoff sets the stale cutoff from the first successful connection; later calls keep it.
func (b *inbox) startCutoff(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cutoff.IsZero() {
		b.cutoff = now.Add(-staleMargin)
	}
}

func (b *inbox) rememberSent(tmpID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent.add(tmpID)
}

// admit returns "" when m should be forwarded, otherwise the reason to drop it. Every verdict
// except "incomplete" and "unknown_status" is final: the id is remembered, so later re-sends of
// the same message (libgm re-sends the whole message on every status or reaction change) drop
// as duplicates. libgm's own de-duplication only covers its last 8 updates.
func (b *inbox) admit(m *libgm.WrappedMessage) string {
	id := m.GetMessageID()
	st := m.GetMessageStatus().GetStatus()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen.has(id) {
		return dropDuplicate
	}
	final := func(reason string) string {
		b.seen.add(id)
		return reason
	}
	switch {
	case m.IsOld:
		return final(dropOld)
	case st >= 1 && st < 100: // outgoing statuses
		return final(dropOwn)
	case m.GetTmpID() != "" && b.sent.has(m.GetTmpID()):
		return final(dropOwn)
	case st >= 200 && st < 300:
		return final(dropSystem)
	case st == gmproto.MessageStatusType_MESSAGE_DELETED:
		return final(dropDeleted)
	case st < 100 || st >= 200:
		return dropUnknown
	case !b.cutoff.IsZero() && time.UnixMicro(m.GetTimestamp()).Before(b.cutoff):
		return final(dropStale)
	case !receivedComplete(st):
		return dropIncomplete
	case noText(m.Message):
		return final(dropNoText)
	}
	b.seen.add(id)
	return ""
}

// receivedComplete: the incoming statuses of a fully received message.
func receivedComplete(st gmproto.MessageStatusType) bool {
	switch st {
	case gmproto.MessageStatusType_INCOMING_COMPLETE,
		gmproto.MessageStatusType_INCOMING_DELIVERED,
		gmproto.MessageStatusType_INCOMING_DISPLAYED:
		return true
	}
	return false
}

// replyFallbackPrefix is the line an iPhone puts in front of a reply sent into an RCS group:
// the Android side gets plain text, with no reply reference. Only this exact English form,
// as seen on a real phone, is recognised.
const replyFallbackPrefix = "Replied to a message:\n"

// messageText joins the text parts of m and removes a leading replyFallbackPrefix, once;
// stripped reports whether it did. A message that was nothing but that line ends up empty.
func messageText(m *gmproto.Message) (text string, stripped bool) {
	var parts []string
	for _, p := range m.GetMessageInfo() {
		if c, ok := p.GetData().(*gmproto.MessageInfo_MessageContent); ok && c.MessageContent.GetContent() != "" {
			parts = append(parts, c.MessageContent.GetContent())
		}
	}
	return stripReplyFallback(strings.Join(parts, "\n"))
}

func stripReplyFallback(text string) (string, bool) {
	if rest, ok := strings.CutPrefix(text, replyFallbackPrefix); ok {
		return rest, true
	}
	return text, false
}

// noText: no text left once the reply line is removed (attachments only, or only that line).
func noText(m *gmproto.Message) bool {
	text, _ := messageText(m)
	return text == ""
}

func hasMedia(m *gmproto.Message) bool {
	for _, p := range m.GetMessageInfo() {
		if _, ok := p.GetData().(*gmproto.MessageInfo_MediaContent); ok {
			return true
		}
	}
	return false
}

// senderNumber is the sender's number from the message, else from the conversation's participants.
func senderNumber(m *gmproto.Message, conv *gmproto.Conversation) string {
	if n := m.GetSenderParticipant().GetID().GetNumber(); n != "" {
		return n
	}
	for _, p := range conv.GetParticipants() {
		if p.GetID().GetParticipantID() == m.GetParticipantID() && p.GetID().GetNumber() != "" {
			return p.GetID().GetNumber()
		}
	}
	return ""
}

// idSet is a set of strings that forgets the oldest entry beyond its capacity.
type idSet struct {
	max   int
	order []string
	items map[string]struct{}
}

func newIDSet(max int) *idSet {
	return &idSet{max: max, items: make(map[string]struct{}, max)}
}

func (s *idSet) has(id string) bool {
	_, ok := s.items[id]
	return ok
}

func (s *idSet) add(id string) {
	if s.has(id) {
		return
	}
	s.items[id] = struct{}{}
	s.order = append(s.order, id)
	if len(s.order) > s.max {
		delete(s.items, s.order[0])
		s.order = s.order[1:]
	}
}
