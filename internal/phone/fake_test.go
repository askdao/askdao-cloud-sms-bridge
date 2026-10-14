package phone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// fakeClient stands in for *libgm.Client. It never touches the network: every method only
// records the call and returns what the test configured.
type fakeClient struct {
	auth *libgm.AuthData

	mu             sync.Mutex
	handler        libgm.EventHandler
	fetchErr       error
	email          string
	connectErr     error
	connects       int
	disconnected   bool
	setActiveCalls int
	emoji          string
	startErr       error
	finishErr      error

	convs        map[string]*gmproto.Conversation // what GetConversation returns
	getConvGate  chan struct{}                    // when set, GetConversation waits for it
	getConvCalls int
	byNumber     map[string]*gmproto.Conversation // what GetOrCreateConversation returns
	createReqs   []*gmproto.GetOrCreateConversationRequest
	sendStatus   gmproto.SendMessageResponse_Status
	sendErr      error
	autoEcho     bool          // echo each sent message back, like the phone does
	sendGate     chan struct{} // when set, SendMessage waits for it
	sent         []*gmproto.SendMessageRequest
	reactOK      bool
	reactions    []*gmproto.SendReactionRequest
	echoSeq      int
}

func (f *fakeClient) SetEventHandler(h libgm.EventHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
}

func (f *fakeClient) FetchConfig(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetchErr
}

func (f *fakeClient) AccountEmail() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.email
}

func (f *fakeClient) Connect(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	return f.connectErr
}

func (f *fakeClient) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected = true
}

func (f *fakeClient) SetActiveSession(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setActiveCalls++
	return nil
}

func (f *fakeClient) StartGaiaPairing(context.Context, context.Context) (string, *libgm.PairingSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", nil, f.startErr
	}
	// What libgm's signInGaiaGetToken fills in before the emoji is known.
	f.auth.Browser = &gmproto.Device{SourceID: "browser"}
	f.auth.Mobile = &gmproto.Device{SourceID: "mobile"}
	f.auth.TachyonAuthToken = []byte("zzTokenval888")
	f.auth.DestRegID = uuid.New()
	return f.emoji, &libgm.PairingSession{}, nil
}

func (f *fakeClient) FinishGaiaPairing(context.Context, *libgm.PairingSession) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "mobile/1", f.finishErr
}

func (f *fakeClient) GetConversation(ctx context.Context, id string) (*gmproto.Conversation, error) {
	f.mu.Lock()
	f.getConvCalls++
	gate := f.getConvGate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.convs[id]; c != nil {
		return c, nil
	}
	return nil, errors.New("conversation not found")
}

func (f *fakeClient) GetOrCreateConversation(_ context.Context, req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createReqs = append(f.createReqs, req)
	conv := f.byNumber[req.GetNumbers()[0].GetNumber()]
	if conv == nil {
		return &gmproto.GetOrCreateConversationResponse{}, nil
	}
	return &gmproto.GetOrCreateConversationResponse{Conversation: conv, Status: gmproto.GetOrCreateConversationResponse_SUCCESS}, nil
}

func (f *fakeClient) SendMessage(ctx context.Context, req *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	f.mu.Lock()
	gate := f.sendGate
	f.sent = append(f.sent, req)
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	status, err, echo := f.sendStatus, f.sendErr, f.autoEcho
	f.echoSeq++
	id := fmt.Sprintf("9%03d", f.echoSeq)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if echo && status == gmproto.SendMessageResponse_SUCCESS {
		go f.emit(&libgm.WrappedMessage{Message: &gmproto.Message{
			MessageID: id, ConversationID: req.GetConversationID(), TmpID: req.GetTmpID(),
			ParticipantID: "1", Timestamp: time.Now().UnixMicro(),
			MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_OUTGOING_SENDING},
			MessageInfo:   req.GetMessagePayload().GetMessageInfo(),
		}})
	}
	return &gmproto.SendMessageResponse{Status: status}, nil
}

func (f *fakeClient) SendReaction(_ context.Context, req *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions = append(f.reactions, req)
	return &gmproto.SendReactionResponse{Success: f.reactOK}, nil
}

func (f *fakeClient) emit(evt any) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	h(evt)
}

func (f *fakeClient) get(fn func(*fakeClient)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// fakeFactory hands out fake clients, configured by prepare, and remembers them.
type fakeFactory struct {
	mu      sync.Mutex
	clients []*fakeClient
	prepare func(n int, c *fakeClient) // n counts from 0
}

func (ff *fakeFactory) newClient(auth *libgm.AuthData, _ zerolog.Logger) Client {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	c := &fakeClient{auth: auth, email: "bot@example.com", sendStatus: gmproto.SendMessageResponse_SUCCESS,
		reactOK: true, convs: map[string]*gmproto.Conversation{}, byNumber: map[string]*gmproto.Conversation{}}
	if ff.prepare != nil {
		ff.prepare(len(ff.clients), c)
	}
	ff.clients = append(ff.clients, c)
	return c
}

func (ff *fakeFactory) count() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return len(ff.clients)
}

func (ff *fakeFactory) client(i int) *fakeClient {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.clients[i]
}

// Distinctive fake credential values; tests assert they never reach a log line or an error.
var sentinels = []string{"zzSIDval111", "zzSAPISIDval666", "zzTokenval888"}

func testCookies() map[string]string {
	return map[string]string{"SID": "zzSIDval111", "HSID": "h", "SSID": "s", "OSID": "o", "APISID": "a", "SAPISID": "zzSAPISIDval666"}
}

// pairedAuth is a fake Google-account session as session.Load would return it.
func pairedAuth() *libgm.AuthData {
	auth := libgm.NewAuthData()
	auth.SetCookies(testCookies())
	auth.DestRegID = uuid.New()
	auth.Browser = &gmproto.Device{SourceID: "browser"}
	auth.TachyonAuthToken = []byte("zzTokenval888")
	return auth
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func assertNoSecrets(t *testing.T, where, s string) {
	t.Helper()
	for _, v := range sentinels {
		if strings.Contains(s, v) {
			t.Errorf("%s contains a credential value", where)
		}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
