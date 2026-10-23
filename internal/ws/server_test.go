package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/phone"
)

const token = "zzTokenForTests"

type fakeBackend struct {
	mu     sync.Mutex
	sends  []string // "to|text"
	reacts []string // "to|message_id|reaction"
	images []string // "to|" + the Image as JSON
}

func (b *fakeBackend) Send(to, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sends = append(b.sends, to+"|"+text)
}

func (b *fakeBackend) React(to, messageID, reaction string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reacts = append(b.reacts, to+"|"+messageID+"|"+reaction)
}

func (b *fakeBackend) SendImage(to string, img phone.Image) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, _ := json.Marshal(img)
	b.images = append(b.images, to+"|"+string(j))
}

func (b *fakeBackend) imageCalls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.images...)
}

func (b *fakeBackend) snapshot() ([]string, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sends...), append([]string(nil), b.reacts...)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type testServer struct {
	srv     *Server
	backend *fakeBackend
	logs    *syncBuffer
	url     string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{backend: &fakeBackend{}, logs: &syncBuffer{}}
	ts.srv = New(token, ts.backend, zerolog.New(ts.logs))
	ts.srv.PublishStatus(phone.Status{State: phone.StatePhoneOffline, Detail: phone.DetailConnecting})
	hs := httptest.NewServer(ts.srv)
	t.Cleanup(hs.Close)
	ts.url = "ws" + strings.TrimPrefix(hs.URL, "http")
	return ts
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func write(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// read returns the next frame, or the close error.
func read(t *testing.T, c *websocket.Conn) (map[string]any, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, "", err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return m, string(b), nil
}

func authed(t *testing.T, ts *testServer) *websocket.Conn {
	t.Helper()
	c := dial(t, ts.url)
	write(t, c, map[string]string{"type": "auth", "token": token})
	m, _, err := read(t, c)
	if err != nil || m["type"] != "status" {
		t.Fatalf("expected the status frame after auth, got %v %v", m, err)
	}
	return c
}

func TestAuthThenStatusFrames(t *testing.T) {
	ts := newTestServer(t)
	c := dial(t, ts.url)
	write(t, c, map[string]string{"type": "auth", "token": token})
	_, raw, err := read(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if raw != `{"type":"status","status":"connected","state":"phone_offline","detail":"connecting","features":["send_image"]}` {
		t.Errorf("first frame: %s", raw)
	}
	ts.srv.PublishStatus(phone.Status{State: phone.StateOK})
	ts.srv.PublishStatus(phone.Status{State: phone.StateLoggedOut, Detail: phone.DetailPairRevoked})
	_, raw, _ = read(t, c)
	if raw != `{"type":"status","status":"connected","state":"ok","detail":"","features":["send_image"]}` {
		t.Errorf("second frame: %s", raw)
	}
	_, raw, _ = read(t, c)
	if raw != `{"type":"status","status":"connected","state":"logged_out","detail":"pair_revoked","features":["send_image"]}` {
		t.Errorf("third frame: %s", raw)
	}
}

func TestRefusedConnections(t *testing.T) {
	cases := []struct {
		name  string
		first any
		want  string
	}{
		{"wrong token", map[string]string{"type": "auth", "token": "nope"}, "Invalid token"},
		{"command before auth", map[string]string{"type": "send", "to": "24", "text": "zzBody"}, "Not authenticated"},
		{"not json", "zzBody", "Not authenticated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			c := dial(t, ts.url)
			if s, ok := tc.first.(string); ok {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = c.Write(ctx, websocket.MessageText, []byte(s))
				cancel()
			} else {
				write(t, c, tc.first)
			}
			m, _, err := read(t, c)
			if err != nil || m["type"] != "error" || m["error"] != tc.want {
				t.Fatalf("got %v %v, want error frame %q", m, err, tc.want)
			}
			_, _, err = read(t, c)
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Errorf("close status = %v, want 1008", websocket.CloseStatus(err))
			}
			if sends, _ := ts.backend.snapshot(); len(sends) != 0 {
				t.Error("a command from an unauthenticated connection was acted on")
			}
			if strings.Contains(ts.logs.String(), token) || strings.Contains(ts.logs.String(), "zzBody") {
				t.Error("logs contain the token or message text")
			}
		})
	}
}

func TestAuthTimeout(t *testing.T) {
	ts := newTestServer(t)
	ts.srv.authTimeout = 50 * time.Millisecond
	c := dial(t, ts.url)
	_, _, err := read(t, c)
	if err == nil {
		t.Fatal("connection should be closed without an auth frame")
	}
}

func TestMessageFrame(t *testing.T) {
	ts := newTestServer(t)
	c := authed(t, ts)
	ts.srv.Deliver(phone.Inbound{
		ID: "101", ChatID: "24", Sender: "+15550000001", Content: "/askdao hi",
		Timestamp: 1767268800, IsGroup: true, ChatTitle: "Test group",
	})
	_, raw, err := read(t, c)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"message","id":"101","sender":"+15550000001","chat_id":"24","content":"/askdao hi","timestamp":1767268800,"isGroup":true,"chat_title":"Test group","media":[]}`
	if raw != want {
		t.Errorf("got  %s\nwant %s", raw, want)
	}
	// A reply whose leading "Replied to a message:" line was removed carries reply_fallback.
	ts.srv.Deliver(phone.Inbound{
		ID: "102", ChatID: "24", Sender: "+15550000001", Content: "/askdao hi",
		Timestamp: 1767268800, IsGroup: true, ChatTitle: "Test group", ReplyFallback: true,
	})
	_, raw, err = read(t, c)
	if err != nil {
		t.Fatal(err)
	}
	want = `{"type":"message","id":"102","sender":"+15550000001","chat_id":"24","content":"/askdao hi","timestamp":1767268800,"isGroup":true,"chat_title":"Test group","media":[],"reply_fallback":true}`
	if raw != want {
		t.Errorf("got  %s\nwant %s", raw, want)
	}
}

func TestCommands(t *testing.T) {
	ts := newTestServer(t)
	c := authed(t, ts)
	write(t, c, map[string]string{"type": "send", "to": "24", "text": "zzBody one", "reply_to": "101"})
	write(t, c, map[string]string{"type": "send", "to": "+15550000001", "text": "zzBody two"})
	write(t, c, map[string]string{"type": "react", "to": "24", "message_id": "101", "reaction": "like"})
	write(t, c, map[string]string{"type": "send", "to": "24"})          // no text: ignored
	write(t, c, map[string]string{"type": "react", "to": "24"})         // incomplete: ignored
	write(t, c, map[string]string{"type": "typing", "to": "24"})        // unknown: ignored
	write(t, c, map[string]string{"type": "auth", "token": "whatever"}) // already authenticated: ignored
	write(t, c, map[string]string{"type": "send", "to": "24", "text": "zzBody three"})
	eventually(t, func() bool { s, _ := ts.backend.snapshot(); return len(s) == 3 })
	sends, reacts := ts.backend.snapshot()
	if strings.Join(sends, ",") != "24|zzBody one,+15550000001|zzBody two,24|zzBody three" {
		t.Errorf("sends: %v", sends)
	}
	if strings.Join(reacts, ",") != "24|101|like" {
		t.Errorf("reacts: %v", reacts)
	}
	if strings.Contains(ts.logs.String(), "zzBody") {
		t.Error("logs contain message text")
	}
}

func TestSendImageFrames(t *testing.T) {
	ts := newTestServer(t)
	c := authed(t, ts)
	write(t, c, map[string]any{"type": "send_image", "to": "24", "html": "<p>zzBodyHTML</p>", "width": 1080,
		"filename": "report.png", "caption": "zzBody caption", "fallback": []string{"zzBody one", "zzBody two"}})
	write(t, c, map[string]any{"type": "send_image", "to": "24", "html": "<p>zzBodyHTML</p>"})                    // nothing optional
	write(t, c, map[string]any{"type": "send_image", "to": "+15550000001", "fallback": []string{"zzBody three"}}) // no html: fallback only
	write(t, c, map[string]any{"type": "send_image", "html": "<p>zzBodyHTML</p>", "fallback": []string{"x"}})     // no to: ignored
	write(t, c, map[string]any{"type": "send_image", "to": "24"})                                                 // nothing to send: ignored
	write(t, c, map[string]any{"type": "send", "to": "24", "text": "zzBody after"})
	eventually(t, func() bool { s, _ := ts.backend.snapshot(); return len(s) == 1 })
	want := []string{
		`24|{"HTML":"\u003cp\u003ezzBodyHTML\u003c/p\u003e","Width":1080,"Filename":"report.png","Caption":"zzBody caption","Fallback":["zzBody one","zzBody two"]}`,
		`24|{"HTML":"\u003cp\u003ezzBodyHTML\u003c/p\u003e","Width":0,"Filename":"","Caption":"","Fallback":null}`,
		`+15550000001|{"HTML":"","Width":0,"Filename":"","Caption":"","Fallback":["zzBody three"]}`,
	}
	if got := ts.backend.imageCalls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := strings.Count(ts.logs.String(), "ignored a send_image frame"); n != 2 {
		t.Errorf("%d ignored frames logged, want 2", n)
	}
	if strings.Contains(ts.logs.String(), "zzBody") {
		t.Error("logs contain message text or html")
	}
}

// A send_image frame larger than the old 1 MiB limit is read, not a reason to drop the connection.
func TestLargeSendImageFrame(t *testing.T) {
	ts := newTestServer(t)
	c := authed(t, ts)
	html := "<p>" + strings.Repeat("x", 3<<20) + "</p>"
	write(t, c, map[string]any{"type": "send_image", "to": "24", "html": html})
	eventually(t, func() bool { return len(ts.backend.imageCalls()) == 1 })
}

// Only one conductor connection: a newly authenticated one replaces the previous.
func TestNewestConnectionWins(t *testing.T) {
	ts := newTestServer(t)
	a := authed(t, ts)
	b := authed(t, ts)
	_, _, err := read(t, a)
	if websocket.CloseStatus(err) != closeReplaced {
		t.Fatalf("first connection: close status %v, want %d", websocket.CloseStatus(err), closeReplaced)
	}
	ts.srv.Deliver(phone.Inbound{ID: "1", ChatID: "24", Sender: "+15550000001", Content: "x"})
	m, _, err := read(t, b)
	if err != nil || m["type"] != "message" {
		t.Fatalf("second connection should get the message: %v %v", m, err)
	}
	write(t, b, map[string]string{"type": "send", "to": "24", "text": "from b"})
	eventually(t, func() bool { s, _ := ts.backend.snapshot(); return len(s) == 1 })
}

func TestDeliverWithoutConductorIsDropped(t *testing.T) {
	ts := newTestServer(t)
	ts.srv.Deliver(phone.Inbound{ID: "1", ChatID: "24", Sender: "+15550000001", Content: "zzBody"})
	if !strings.Contains(ts.logs.String(), "no conductor connected") || strings.Contains(ts.logs.String(), "zzBody") {
		t.Errorf("logs: %s", ts.logs.String())
	}
	c := authed(t, ts) // nothing queued from before the connection
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Error("a message dropped before the connection was delivered later")
	}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
