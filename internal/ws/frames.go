// [INPUT]: internal/phone Inbound and Status
// [OUTPUT]: messageFrame, statusFrame (with features), ackFrame, errorFrame, command (the JSON shapes on the wire, send_image,
//           the seq_ack delivery mark, hello and received included)
// [POS]: internal/ws's wire format, the iMessage bridge's frames plus the status frame and send_image; server.go encodes and decodes these
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package ws

import (
	"encoding/json"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/delivery"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/phone"
)

// messageFrame is an inbound message, same fields as the iMessage bridge sends.
type messageFrame struct {
	Type      string `json:"type"` // "message"
	ID        string `json:"id"`
	Sender    string `json:"sender"`
	ChatID    string `json:"chat_id"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
	IsGroup   bool   `json:"isGroup"`
	ChatTitle string `json:"chat_title"`
	Media     []any  `json:"media"` // always empty: attachments are not forwarded yet
	// ReplyFallback is present (true) only when a leading "Replied to a message:" line was removed.
	ReplyFallback bool `json:"reply_fallback,omitempty"`
}

// statusFrame reports the phone connection. "status" is always "connected": it means this
// WebSocket link is up, as in the iMessage bridge's status frame, which conductor reads that
// way. The phone connection is in "state" and "detail". "features" lists the optional frames
// this bridge accepts; conductor sends send_image only to a bridge that lists it.
type statusFrame struct {
	Type     string   `json:"type"` // "status"
	Status   string   `json:"status"`
	State    string   `json:"state"`
	Detail   string   `json:"detail"`
	Features []string `json:"features"`
}

// features is what every status frame advertises: send_image frames, and the seq_ack delivery
// protocol (marked outbound frames are de-duplicated and acknowledged; inbound messages are held
// until conductor answers received, and sent again after a hello that lists received).
var features = []string{"send_image", "seq_ack"}

// ackFrame acknowledges that every letter of a conversation up to UpToSeq has been handled.
type ackFrame struct {
	Type    string `json:"type"` // "ack"
	To      string `json:"to"`
	UpToSeq int64  `json:"up_to_seq"`
}

type errorFrame struct {
	Type  string `json:"type"` // "error"
	Error string `json:"error"`
}

// command is any frame conductor sends: auth, send, react, send_image, hello or received.
type command struct {
	Type      string `json:"type"`
	Token     string `json:"token"`
	To        string `json:"to"`
	Text      string `json:"text"`
	ReplyTo   string `json:"reply_to"` // accepted and ignored: no quoted replies on this channel
	MessageID string `json:"message_id"`
	Reaction  string `json:"reaction"`
	// send_image
	HTML     string   `json:"html"`
	Width    int      `json:"width"`
	Filename string   `json:"filename"`
	Caption  string   `json:"caption"`
	Fallback []string `json:"fallback"`
	// seq_ack delivery mark on send / react / send_image: all three or none
	Seq  *int64 `json:"seq"`
	Part *int64 `json:"part"`
	Last *bool  `json:"last"`
	// hello: conductor's features ("received" = it answers every inbound message with received)
	Features []string `json:"features"`
	// received: the inbound message id conductor has taken
	ID string `json:"id"`
}

// mark returns the frame's delivery mark, if it carries a complete and valid one.
func (c command) mark() (delivery.Mark, bool) {
	if c.Seq == nil || c.Part == nil || c.Last == nil || *c.Seq < 0 || *c.Part < 0 {
		return delivery.Mark{}, false
	}
	return delivery.Mark{Seq: *c.Seq, Part: *c.Part, Last: *c.Last}, true
}

func encodeMessage(in phone.Inbound) []byte {
	return mustJSON(messageFrame{
		Type: "message", ID: in.ID, Sender: in.Sender, ChatID: in.ChatID, Content: in.Content,
		Timestamp: in.Timestamp, IsGroup: in.IsGroup, ChatTitle: in.ChatTitle, Media: []any{},
		ReplyFallback: in.ReplyFallback,
	})
}

func encodeStatus(st phone.Status) []byte {
	return mustJSON(statusFrame{Type: "status", Status: "connected", State: string(st.State), Detail: st.Detail, Features: features})
}

func encodeAck(to string, upTo int64) []byte {
	return mustJSON(ackFrame{Type: "ack", To: to, UpToSeq: upTo})
}

func encodeError(msg string) []byte {
	return mustJSON(errorFrame{Type: "error", Error: msg})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only fixed struct types of strings, string lists, numbers and bools are encoded
	}
	return b
}
