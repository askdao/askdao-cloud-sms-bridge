package phone

import (
	"bytes"
	"context"
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
	c := &fakeClient{auth: auth, email: "bot@example.com"}
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
