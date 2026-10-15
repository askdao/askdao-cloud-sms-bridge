// [INPUT]: client.go Client (SendMessage, SendReaction, GetOrCreateConversation), manager.go conversation cache and SIMs,
//          inbound.go inbox (to recognise echoes), number.go NormalizeNumber
// [OUTPUT]: Manager.Send, Manager.React; outbox (per-target serial queue)
// [POS]: internal/phone's outbound side, called by the WebSocket server for send / react frames
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package phone

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// errNotConnected: there is no client to send with (not yet connected, reconnecting, or logged out).
var errNotConnected = errors.New("not connected to the phone")

// likeEmoji is what conductor's "like" reaction becomes, in RCS and SMS/MMS conversations alike.
const likeEmoji = "👍"

// Send queues text for to: a phone-local conversation id (digits) or a phone number starting
// with "+". It returns at once. Everything queued for the same to goes out one at a time in
// queue order; a message counts as done when the phone has echoed it back (the echo carries
// the same tmp id) or after echoWait, or when sending failed.
func (m *Manager) Send(to, text string) {
	m.out.enqueue(to, func() { m.sendText(m.ctx, to, text) })
}

// React queues a reaction on messageID in the same queue as Send. Only "like" is supported,
// sent as 👍; anything else is logged and ignored.
func (m *Manager) React(to, messageID, reaction string) {
	if reaction != "like" {
		m.log.Warn().Str("to", to).Str("message_id", messageID).Str("reaction", reaction).Msg("react ignored: only like is supported")
		return
	}
	m.out.enqueue(to, func() { m.sendLike(m.ctx, to, messageID) })
}

func (m *Manager) sendText(ctx context.Context, to, text string) {
	log := m.log.With().Str("to", to).Int("chars", len([]rune(text))).Logger()
	cli := m.client()
	if cli == nil {
		log.Warn().Err(errNotConnected).Msg("send failed")
		return
	}
	convID, err := m.resolveTarget(ctx, cli, to)
	if err != nil {
		log.Warn().Err(err).Msg("send failed")
		return
	}
	conv, err := m.conversation(ctx, convID)
	if err != nil {
		log.Warn().Err(err).Str("conversation_id", convID).Msg("send failed")
		return
	}
	tmp := uuid.NewString()
	log = log.With().Str("conversation_id", convID).Str("tmp_id", tmp).Logger()
	echo := m.expectEcho(tmp)
	defer m.forgetEcho(tmp)
	resp, err := cli.SendMessage(ctx, &gmproto.SendMessageRequest{
		ConversationID: convID,
		MessagePayload: &gmproto.MessagePayload{
			TmpID:          tmp,
			TmpID2:         tmp,
			ConversationID: convID,
			ParticipantID:  conv.GetDefaultOutgoingID(),
			MessageInfo: []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: text},
			}}},
		},
		SIMPayload: m.simPayload(conv),
		TmpID:      tmp,
	})
	if err != nil {
		log.Warn().Err(err).Msg("send failed")
		return
	}
	if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		log.Warn().Str("status", resp.GetStatus().String()).Msg("send failed: the phone refused it")
		return
	}
	select {
	case <-echo:
		log.Info().Msg("sent")
	case <-time.After(m.echoWait):
		log.Warn().Dur("waited", m.echoWait).Msg("sent, but the phone has not echoed it yet; sending the next one")
	case <-ctx.Done():
	}
}

func (m *Manager) sendLike(ctx context.Context, to, messageID string) {
	log := m.log.With().Str("to", to).Str("message_id", messageID).Logger()
	cli := m.client()
	if cli == nil {
		log.Warn().Err(errNotConnected).Msg("react failed")
		return
	}
	var conv *gmproto.Conversation // only used to pick the SIM; without it a single SIM is used
	if convID, err := m.resolveTarget(ctx, cli, to); err == nil {
		conv, _ = m.conversation(ctx, convID)
	}
	resp, err := cli.SendReaction(ctx, &gmproto.SendReactionRequest{
		MessageID:    messageID,
		ReactionData: gmproto.MakeReactionData(likeEmoji),
		Action:       gmproto.SendReactionRequest_ADD,
		SIMPayload:   m.simPayload(conv),
	})
	switch {
	case err != nil:
		log.Warn().Err(err).Msg("react failed")
	case !resp.GetSuccess():
		log.Warn().Msg("react failed: the phone refused it")
	default:
		log.Info().Msg("reacted")
	}
}

// resolveTarget returns the conversation id for to: digits are a conversation id already;
// "+<number>" is looked up, or created, as the one-to-one conversation with that number.
func (m *Manager) resolveTarget(ctx context.Context, cli Client, to string) (string, error) {
	switch {
	case strings.HasPrefix(to, "+"):
		number := NormalizeNumber(to)
		resp, err := cli.GetOrCreateConversation(ctx, &gmproto.GetOrCreateConversationRequest{
			Numbers: []*gmproto.ContactNumber{{MysteriousInt: 2, Number: number, Number2: number}},
		})
		if err != nil {
			return "", fmt.Errorf("finding the conversation for the number: %w", err)
		}
		conv := resp.GetConversation()
		if conv.GetConversationID() == "" {
			return "", fmt.Errorf("no conversation for the number (status %s)", resp.GetStatus())
		}
		m.cacheConversation(conv)
		return conv.GetConversationID(), nil
	case isDigits(to):
		return to, nil
	default:
		return "", errors.New("target is neither a conversation id nor a +number")
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// expectEcho registers tmpID before sending: the echo marks the message as sent, and inbox
// drops it as the bridge's own message.
func (m *Manager) expectEcho(tmpID string) <-chan struct{} {
	ch := make(chan struct{})
	m.echoMu.Lock()
	m.echoes[tmpID] = ch
	m.echoMu.Unlock()
	m.inbox.rememberSent(tmpID)
	return ch
}

// echoed is called from libgm's callback for every message event carrying a tmp id.
func (m *Manager) echoed(tmpID string) {
	m.echoMu.Lock()
	defer m.echoMu.Unlock()
	if ch, ok := m.echoes[tmpID]; ok {
		delete(m.echoes, tmpID)
		close(ch)
	}
}

func (m *Manager) forgetEcho(tmpID string) {
	m.echoMu.Lock()
	defer m.echoMu.Unlock()
	delete(m.echoes, tmpID)
}

// simPayload picks the SIM for conv like upstream: the SIM whose participant is the
// conversation's default outgoing participant, else the conversation's SIM, else the only SIM.
func (m *Manager) simPayload(conv *gmproto.Conversation) *gmproto.SIMPayload {
	m.convMu.Lock()
	defer m.convMu.Unlock()
	if conv != nil {
		for _, s := range m.sims {
			if s.GetSIMParticipant().GetID() == conv.GetDefaultOutgoingID() {
				return s.GetSIMData().GetSIMPayload()
			}
		}
		if p := conv.GetSimCard().GetSIMData().GetSIMPayload(); p != nil {
			return p
		}
	}
	if len(m.sims) == 1 {
		return m.sims[0].GetSIMData().GetSIMPayload()
	}
	return nil
}

// outbox runs queued jobs one at a time per key, in queue order; different keys do not wait
// for each other.
type outbox struct {
	mu    sync.Mutex
	tails map[string]chan struct{}
}

func newOutbox() *outbox { return &outbox{tails: map[string]chan struct{}{}} }

func (o *outbox) enqueue(key string, job func()) {
	o.mu.Lock()
	prev := o.tails[key]
	done := make(chan struct{})
	o.tails[key] = done
	o.mu.Unlock()
	go func() {
		defer func() {
			close(done)
			o.mu.Lock()
			if o.tails[key] == done {
				delete(o.tails, key)
			}
			o.mu.Unlock()
		}()
		if prev != nil {
			<-prev
		}
		job()
	}()
}
