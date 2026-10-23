// [INPUT]: github.com/coder/websocket, frames.go, internal/phone Inbound and Status, a Backend (phone.Manager)
// [OUTPUT]: Server (New, ServeHTTP, PublishStatus, Deliver), Backend
// [POS]: internal/ws, the WebSocket server conductor connects to: shared-token auth, one authenticated
//        connection at a time (the newest wins), status and message frames out, send / react / send_image frames in
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

// Package ws serves the bridge's WebSocket protocol to conductor.
package ws

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/phone"
)

// Backend receives conductor's commands. Every call must return without waiting for the phone.
type Backend interface {
	Send(to, text string)
	React(to, messageID, reaction string)
	SendImage(to string, img phone.Image)
}

// Close codes. Anything other than 1000/1001 makes conductor's client wait 5 seconds before
// reconnecting instead of retrying at once.
const (
	closeReplaced websocket.StatusCode = 4000 // a newer authenticated connection took over
	closeSlow     websocket.StatusCode = 4001 // the outgoing frame buffer filled up
)

const (
	readLimit    = 8 << 20 // a send_image frame carries a whole HTML page, possibly with data: images
	outBuffer    = 256
	writeTimeout = 10 * time.Second
)

// Server is the WebSocket endpoint. It keeps one authenticated connection; a newly
// authenticated one replaces the previous (closed with 4000), so during a conductor
// redeploy the old and new instances may take turns until the old one stops.
type Server struct {
	token       string
	backend     Backend
	log         zerolog.Logger
	authTimeout time.Duration

	mu     sync.Mutex
	cur    *conn
	status phone.Status
}

type conn struct {
	ws   *websocket.Conn
	out  chan []byte
	done chan struct{}
}

// New returns a Server. token must not be empty.
func New(token string, backend Backend, log zerolog.Logger) *Server {
	return &Server{token: token, backend: backend, log: log, authTimeout: 10 * time.Second}
}

// PublishStatus records st and sends it to the connected conductor. It is a Tracker
// subscriber: it runs under the tracker's lock and never blocks.
func (s *Server) PublishStatus(st phone.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
	if s.cur != nil {
		s.push(s.cur, encodeStatus(st))
	}
}

// Deliver sends an inbound message to the connected conductor, or drops it (logged) when
// none is connected.
func (s *Server) Deliver(in phone.Inbound) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		s.log.Warn().Str("message_id", in.ID).Str("conversation_id", in.ChatID).Msg("inbound dropped: no conductor connected")
		return
	}
	s.push(s.cur, encodeMessage(in))
}

// push queues a frame without blocking; a connection that stops reading is closed.
// Callers hold s.mu.
func (s *Server) push(c *conn, frame []byte) {
	select {
	case c.out <- frame:
	default:
		s.log.Error().Msg("conductor is not reading; closing its connection")
		go c.ws.Close(closeSlow, "too many unsent frames")
	}
}

// ServeHTTP upgrades the request and runs the connection until it closes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has written the HTTP error
	}
	ws.SetReadLimit(readLimit)
	ctx := r.Context()
	if !s.authenticate(ctx, ws) {
		return
	}
	c := &conn{ws: ws, out: make(chan []byte, outBuffer), done: make(chan struct{})}
	defer close(c.done)
	go s.writeLoop(c)
	s.attach(c)
	defer s.detach(c)
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			s.log.Info().Int("close_code", int(websocket.CloseStatus(err))).Msg("conductor disconnected")
			return
		}
		if !s.isCurrent(c) {
			return // replaced: nothing more from this connection is acted on
		}
		s.handle(data)
	}
}

// authenticate requires {"type":"auth","token":...} as the first frame within authTimeout.
func (s *Server) authenticate(ctx context.Context, ws *websocket.Conn) bool {
	actx, cancel := context.WithTimeout(ctx, s.authTimeout)
	defer cancel()
	_, data, err := ws.Read(actx)
	if err != nil {
		s.log.Warn().Msg("connection closed: no auth frame in time")
		ws.Close(websocket.StatusPolicyViolation, "authentication timeout")
		return false
	}
	var cmd command
	reason := ""
	switch {
	case json.Unmarshal(data, &cmd) != nil || cmd.Type != "auth":
		reason = "Not authenticated"
	case subtle.ConstantTimeCompare([]byte(cmd.Token), []byte(s.token)) != 1:
		reason = "Invalid token"
	}
	if reason == "" {
		return true
	}
	s.log.Warn().Str("reason", reason).Msg("connection refused")
	wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
	defer wcancel()
	_ = ws.Write(wctx, websocket.MessageText, encodeError(reason))
	ws.Close(websocket.StatusPolicyViolation, reason)
	return false
}

// attach makes c the connection and sends it the current status first.
func (s *Server) attach(c *conn) {
	s.mu.Lock()
	old := s.cur
	s.cur = c
	s.push(c, encodeStatus(s.status))
	s.mu.Unlock()
	if old != nil {
		s.log.Warn().Msg("a newer conductor connection replaced the previous one")
		go old.ws.Close(closeReplaced, "replaced by a newer connection")
	}
	s.log.Info().Msg("conductor authenticated")
}

func (s *Server) detach(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == c {
		s.cur = nil
	}
}

func (s *Server) isCurrent(c *conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur == c
}

func (s *Server) writeLoop(c *conn) {
	for {
		select {
		case <-c.done:
			return
		case frame := <-c.out:
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				c.ws.CloseNow()
				return
			}
		}
	}
}

// handle acts on one frame from the authenticated conductor. Frames are acted on in order;
// Backend calls return at once, so a slow phone never stalls this loop.
func (s *Server) handle(data []byte) {
	var cmd command
	if err := json.Unmarshal(data, &cmd); err != nil {
		s.log.Warn().Msg("ignored a frame that is not JSON")
		return
	}
	switch cmd.Type {
	case "send":
		if cmd.To == "" || cmd.Text == "" {
			s.log.Warn().Str("to", cmd.To).Msg("ignored a send frame without to or text")
			return
		}
		s.backend.Send(cmd.To, cmd.Text) // reply_to is ignored: this channel has no quoted replies
	case "react":
		if cmd.To == "" || cmd.MessageID == "" || cmd.Reaction == "" {
			s.log.Warn().Str("to", cmd.To).Msg("ignored a react frame without to, message_id or reaction")
			return
		}
		s.backend.React(cmd.To, cmd.MessageID, cmd.Reaction)
	case "send_image":
		if cmd.To == "" || (cmd.HTML == "" && len(cmd.Fallback) == 0) {
			s.log.Warn().Str("to", cmd.To).Msg("ignored a send_image frame without to, or without both html and fallback")
			return
		}
		// Without html the phone side sends the fallback texts straight away.
		s.backend.SendImage(cmd.To, phone.Image{
			HTML: cmd.HTML, Width: cmd.Width, Filename: cmd.Filename, Caption: cmd.Caption, Fallback: cmd.Fallback,
		})
	case "auth":
		// already authenticated
	default:
		s.log.Warn().Str("type", cmd.Type).Msg("ignored a frame of unknown type")
	}
}
