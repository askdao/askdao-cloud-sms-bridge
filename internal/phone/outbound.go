// [INPUT]: client.go Client (SendMessage, SendReaction, GetOrCreateConversation, UploadMedia), manager.go conversation
//          cache, SIMs and RenderFunc, inbound.go inbox (to recognise echoes), number.go NormalizeNumber
// [OUTPUT]: Manager.Send, Manager.React, Manager.SendImage; Image, RenderFunc; outbox (per-target serial queue)
// [POS]: internal/phone's outbound side, called by the WebSocket server for send / react / send_image frames
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
	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// errNotConnected: there is no client to send with (not yet connected, reconnecting, or logged out).
var errNotConnected = errors.New("not connected to the phone")

// likeEmoji is what conductor's "like" reaction becomes, in RCS and SMS/MMS conversations alike.
const likeEmoji = "👍"

// defaultImageName names the PNG when the frame gives no filename.
const defaultImageName = "image.png"

// RenderFunc draws a self-contained HTML page width CSS pixels wide and returns the PNG.
type RenderFunc func(ctx context.Context, html string, width int) ([]byte, error)

// Image is a send_image frame: HTML to draw and send as a picture, a caption sent after it as a
// separate text, and the texts to send instead when the picture cannot be sent.
type Image struct {
	HTML     string
	Width    int // CSS pixels; the renderer applies its default when 0
	Filename string
	Caption  string
	Fallback []string
}

// Send queues text for to: a phone-local conversation id (digits) or a phone number starting
// with "+". It returns at once. Everything queued for the same to goes out one at a time in
// queue order; a message counts as done when the phone has echoed it back (the echo carries
// the same tmp id) or after echoWait, or when sending failed.
func (m *Manager) Send(to, text string) {
	m.out.enqueue(to, func() { _ = m.sendText(m.ctx, to, text) })
}

// SendImage queues img for to in the same queue as Send, as one job, and returns at once: the
// HTML is rendered to a PNG, uploaded and sent as a media message, then the caption (if any)
// goes as a text. If anything fails before the phone accepted the media message, the fallback
// texts go instead, one after the other. If only the caption fails, that is logged.
func (m *Manager) SendImage(to string, img Image) {
	m.out.enqueue(to, func() { m.sendImage(m.ctx, to, img) })
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

func (m *Manager) sendText(ctx context.Context, to, text string) error {
	log := m.log.With().Str("to", to).Int("chars", len([]rune(text))).Logger()
	return m.sendParts(ctx, to, []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MessageContent{
		MessageContent: &gmproto.MessageContent{Content: text},
	}}}, log)
}

// sendImage is SendImage's job. Its last log line sums up the frame: outcome image or fallback,
// render time and PNG size.
func (m *Manager) sendImage(ctx context.Context, to string, img Image) {
	log := m.log.With().Str("to", to).Logger()
	var renderMS int64
	var size int
	err := func() error {
		if img.HTML == "" {
			return errors.New("the frame has no html")
		}
		cli := m.client()
		if cli == nil {
			return errNotConnected
		}
		start := time.Now()
		png, err := m.render(ctx, img.HTML, img.Width)
		renderMS = time.Since(start).Milliseconds()
		if err != nil {
			return fmt.Errorf("rendering: %w", err)
		}
		size = len(png)
		name := img.Filename
		if name == "" {
			name = defaultImageName
		}
		media, err := cli.UploadMedia(png, name, "image/png")
		if err != nil {
			return fmt.Errorf("uploading: %w", err)
		}
		return m.sendParts(ctx, to, []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MediaContent{MediaContent: media}}},
			log.With().Int("png_bytes", size).Logger())
	}()
	if err != nil {
		sent := 0
		for _, text := range img.Fallback {
			if text != "" && m.sendText(ctx, to, text) == nil {
				sent++
			}
		}
		log.Warn().Err(err).Str("outcome", "fallback").Int64("render_ms", renderMS).Int("png_bytes", size).
			Int("fallback_sent", sent).Int("fallback_texts", len(img.Fallback)).Msg("send_image: image not sent, fallback texts sent instead")
		return
	}
	caption := "none"
	if img.Caption != "" {
		caption = "sent"
		if m.sendText(ctx, to, img.Caption) != nil {
			caption = "failed" // no fallback: it would repeat what the image already says
		}
	}
	log.Info().Str("outcome", "image").Int64("render_ms", renderMS).Int("png_bytes", size).Str("caption", caption).
		Msg("send_image: image sent")
}

// sendParts sends one message made of parts to `to`, then waits for the phone to echo it back
// (at most echoWait) so that the next message cannot overtake it. It returns nil once the phone
// has accepted the message, that is SendMessage answered SUCCESS: that is what "sent" means for
// texts and images alike; a missing echo only delays the next message. Failures are logged here.
func (m *Manager) sendParts(ctx context.Context, to string, parts []*gmproto.MessageInfo, log zerolog.Logger) error {
	cli := m.client()
	if cli == nil {
		log.Warn().Err(errNotConnected).Msg("send failed")
		return errNotConnected
	}
	convID, err := m.resolveTarget(ctx, cli, to)
	if err != nil {
		log.Warn().Err(err).Msg("send failed")
		return err
	}
	conv, err := m.conversation(ctx, convID)
	if err != nil {
		log.Warn().Err(err).Str("conversation_id", convID).Msg("send failed")
		return err
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
			MessageInfo:    parts,
		},
		SIMPayload: m.simPayload(conv),
		TmpID:      tmp,
	})
	if err != nil {
		log.Warn().Err(err).Msg("send failed")
		return err
	}
	if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		log.Warn().Str("status", resp.GetStatus().String()).Msg("send failed: the phone refused it")
		return fmt.Errorf("the phone refused the message (%s)", resp.GetStatus())
	}
	select {
	case <-echo:
		log.Info().Msg("sent")
	case <-time.After(m.echoWait):
		log.Warn().Dur("waited", m.echoWait).Msg("sent, but the phone has not echoed it yet; sending the next one")
	case <-ctx.Done():
	}
	return nil
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
