// [INPUT]: internal/phone Inbound and Status
// [OUTPUT]: messageFrame, statusFrame (with features), errorFrame, command (the JSON shapes on the wire, send_image included)
// [POS]: internal/ws's wire format, the iMessage bridge's frames plus the status frame and send_image; server.go encodes and decodes these
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package ws

import (
	"encoding/json"

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

// features is what every status frame advertises.
var features = []string{"send_image"}

type errorFrame struct {
	Type  string `json:"type"` // "error"
	Error string `json:"error"`
}

// command is any frame conductor sends: auth, send, react or send_image.
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
