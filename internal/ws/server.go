// [INPUT]: github.com/coder/websocket, frames.go, internal/phone Inbound and Status, a Backend (phone.Manager),
//          internal/delivery Ledger (seq_ack bookkeeping)
// [OUTPUT]: Server (New, ServeHTTP, PublishStatus, Deliver, Connected), Backend
// [POS]: internal/ws, the WebSocket server conductor connects to: shared-token auth, one conductor
//        connection at a time (a newcomer is refused while the current one answers a ping), read-only
//        observers on a second token, status / message / ack frames out, send / react / send_image /
//        hello / received frames in
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

// Package ws serves the bridge's WebSocket protocol to conductor.
package ws

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/delivery"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/phone"
)

// Backend receives conductor's commands. Every call must return without waiting for the phone.
// After runs fn once everything queued for to so far has been handled.
type Backend interface {
	Send(to, text string)
	React(to, messageID, reaction string)
	SendImage(to string, img phone.Image)
	After(to string, fn func())
}

// Close codes. Anything other than 1000/1001 makes conductor's client wait 5 seconds before
// reconnecting instead of retrying at once.
const (
	closeReplaced websocket.StatusCode = 4000 // the connection stopped answering pings and a newer one took over
	closeSlow     websocket.StatusCode = 4001 // the outgoing frame buffer filled up
	closeBusy     websocket.StatusCode = 4009 // another conductor connection is live
)

const (
	readLimit    = 8 << 20 // a send_image frame carries a whole HTML page, possibly with data: images
	outBuffer    = 256
	writeTimeout = 10 * time.Second
	maxObservers = 4
)

// Server is the WebSocket endpoint. It keeps one conductor connection: a newcomer with the
// token is refused (4009) while the current one answers a ping, and takes over (the old one
// closed with 4000) when it does not. Observers authenticate with the observer token, get
// every frame the conductor connection gets and may not send commands.
type Server struct {
	token         string
	observerToken string
	backend       Backend
	ledger        *delivery.Ledger
	log           zerolog.Logger
	authTimeout   time.Duration
	pingTimeout   time.Duration

	attachMu  sync.Mutex // one newcomer at a time checks the current connection
	mu        sync.Mutex
	cur       *conn
	observers map[*conn]struct{}
	status    phone.Status
}

type role int

const (
	refused role = iota
	primary
	observer
)

type conn struct {
	ws   *websocket.Conn
	out  chan []byte
	done chan struct{}
}

// New returns a Server. token must not be empty; an empty observerToken allows no observers.
// ledger holds the seq_ack bookkeeping; nil keeps it in memory with a 30-minute replay window.
func New(token, observerToken string, backend Backend, ledger *delivery.Ledger, log zerolog.Logger) *Server {
	if ledger == nil {
		ledger = delivery.New("", 30*time.Minute, log)
	}
	return &Server{
		token: token, observerToken: observerToken, backend: backend, ledger: ledger, log: log,
		authTimeout: 10 * time.Second, pingTimeout: 5 * time.Second, observers: map[*conn]struct{}{},
	}
}

// Connected reports whether a conductor connection is attached.
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil
}

// broadcast sends frame to the conductor connection and every observer. Callers hold s.mu.
func (s *Server) broadcast(frame []byte) {
	if s.cur != nil {
		s.push(s.cur, frame)
	}
	for o := range s.observers {
		s.push(o, frame)
	}
}

// PublishStatus records st and sends it to the connected conductor. It is a Tracker
// subscriber: it runs under the tracker's lock and never blocks.
func (s *Server) PublishStatus(st phone.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
	s.broadcast(encodeStatus(st))
}

// Deliver holds an inbound message until conductor confirms it (received), and sends it to the
// connected conductor. With none connected it stays held, for a conductor that says hello.
func (s *Server) Deliver(in phone.Inbound) {
	frame := encodeMessage(in)
	s.ledger.Hold(in.ID, frame)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		s.log.Warn().Str("message_id", in.ID).Str("conversation_id", in.ChatID).Msg("inbound held: no conductor connected")
	}
	s.broadcast(frame)
}

// sendAck sends an ack frame to the connected conductor, if any.
func (s *Server) sendAck(to string, upTo int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcast(encodeAck(to, upTo))
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
	who := s.authenticate(ctx, ws)
	if who == refused {
		return
	}
	c := &conn{ws: ws, out: make(chan []byte, outBuffer), done: make(chan struct{})}
	defer close(c.done)
	if who == observer {
		s.serveObserver(ctx, c)
		return
	}
	defer ws.CloseNow()
	// Frames are read on their own goroutine from the start, so pings can be answered and nothing
	// conductor sends right after auth is lost while attach decides.
	frames := make(chan []byte, outBuffer)
	go func() {
		defer close(frames)
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				s.log.Info().Int("close_code", int(websocket.CloseStatus(err))).Msg("conductor connection closed")
				return
			}
			select {
			case frames <- data:
			case <-c.done:
				return
			}
		}
	}()
	if !s.attach(ctx, c) {
		return
	}
	go s.writeLoop(c)
	defer s.detach(c)
	for data := range frames {
		if !s.isCurrent(c) {
			return // replaced: nothing more from this connection is acted on
		}
		s.handle(c, data)
	}
}

// serveObserver runs a read-only connection: it gets the current status and every frame after
// it; any command is answered with a read-only error.
func (s *Server) serveObserver(ctx context.Context, c *conn) {
	s.mu.Lock()
	if len(s.observers) >= maxObservers {
		s.mu.Unlock()
		s.refuse(ctx, c.ws, "Too many observers", closeBusy)
		return
	}
	s.observers[c] = struct{}{}
	s.push(c, encodeStatus(s.status))
	s.mu.Unlock()
	go s.writeLoop(c)
	s.log.Info().Msg("observer connected")
	defer func() {
		s.mu.Lock()
		delete(s.observers, c)
		s.mu.Unlock()
		s.log.Info().Msg("observer disconnected")
	}()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			return
		}
		s.mu.Lock()
		s.push(c, encodeError("read-only"))
		s.mu.Unlock()
	}
}

// authenticate requires {"type":"auth","token":...} as the first frame within authTimeout.
// The conductor token gives primary, the observer token (when set) gives observer.
func (s *Server) authenticate(ctx context.Context, ws *websocket.Conn) role {
	actx, cancel := context.WithTimeout(ctx, s.authTimeout)
	defer cancel()
	_, data, err := ws.Read(actx)
	if err != nil {
		s.log.Warn().Msg("connection closed: no auth frame in time")
		ws.Close(websocket.StatusPolicyViolation, "authentication timeout")
		return refused
	}
	var cmd command
	if json.Unmarshal(data, &cmd) != nil || cmd.Type != "auth" {
		s.refuse(ctx, ws, "Not authenticated", websocket.StatusPolicyViolation)
		return refused
	}
	if subtle.ConstantTimeCompare([]byte(cmd.Token), []byte(s.token)) == 1 {
		return primary
	}
	if s.observerToken != "" && subtle.ConstantTimeCompare([]byte(cmd.Token), []byte(s.observerToken)) == 1 {
		return observer
	}
	s.refuse(ctx, ws, "Invalid token", websocket.StatusPolicyViolation)
	return refused
}

// refuse sends an error frame and closes with code.
func (s *Server) refuse(ctx context.Context, ws *websocket.Conn, reason string, code websocket.StatusCode) {
	s.log.Warn().Str("reason", reason).Msg("connection refused")
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, encodeError(reason))
	ws.Close(code, reason)
}

// attach makes c the conductor connection and sends it the current status first. While the
// current connection answers a ping within pingTimeout, c is refused instead (4009): during a
// conductor redeploy the new instance waits until the old one has gone, and the letters it sent
// meanwhile are replayed by conductor because they were never acknowledged.
func (s *Server) attach(ctx context.Context, c *conn) bool {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	s.mu.Lock()
	old := s.cur
	s.mu.Unlock()
	if old != nil {
		pctx, cancel := context.WithTimeout(ctx, s.pingTimeout)
		err := old.ws.Ping(pctx)
		cancel()
		if err == nil {
			s.refuse(ctx, c.ws, "Another client is connected", closeBusy)
			return false
		}
		s.log.Warn().Msg("the conductor connection stopped answering pings; a newer one takes over")
		go old.ws.Close(closeReplaced, "stopped answering pings")
	}
	s.mu.Lock()
	s.cur = c
	s.push(c, encodeStatus(s.status))
	s.mu.Unlock()
	s.log.Info().Msg("conductor authenticated")
	return true
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
// Backend calls return at once, so a slow phone never stalls this loop. A frame with a delivery
// mark that this conversation has already seen is skipped (re-acknowledged when it is a letter's
// last frame); otherwise, once the phone queue has handled it, its progress is saved and the
// letter's last frame is acknowledged.
func (s *Server) handle(c *conn, data []byte) {
	var cmd command
	if err := json.Unmarshal(data, &cmd); err != nil {
		s.log.Warn().Msg("ignored a frame that is not JSON")
		return
	}
	switch cmd.Type {
	case "hello":
		if slices.Contains(cmd.Features, "received") {
			held := s.ledger.Pending()
			if len(held) > 0 {
				s.log.Info().Int("messages", len(held)).Msg("replaying held inbound messages")
			}
			// Blocking sends on a goroutine: the replay may be longer than the frame buffer.
			go func() {
				for _, f := range held {
					select {
					case c.out <- f:
					case <-c.done:
						return
					}
				}
			}()
		}
		return
	case "received":
		if cmd.ID != "" {
			s.ledger.Release(cmd.ID)
		}
		return
	}
	mark, marked := cmd.mark()
	marked = marked && cmd.To != "" && (cmd.Type == "send" || cmd.Type == "react" || cmd.Type == "send_image")
	if marked && !s.ledger.Accept(cmd.To, mark) {
		s.log.Info().Str("to", cmd.To).Int64("seq", mark.Seq).Int64("part", mark.Part).Msg("skipped a replayed frame")
		if upTo, ok := s.ledger.AckedUpTo(cmd.To); ok && mark.Last {
			s.sendAck(cmd.To, upTo)
		}
		return
	}
	s.dispatch(cmd)
	if marked {
		to := cmd.To
		s.backend.After(to, func() {
			if upTo, ok := s.ledger.Done(to, mark); ok {
				s.sendAck(to, upTo)
			}
		})
	}
}

func (s *Server) dispatch(cmd command) {
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
