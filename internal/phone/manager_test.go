package phone

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

type harness struct {
	t       *testing.T
	mgr     *Manager
	ff      *fakeFactory
	logs    *syncBuffer
	path    string
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	history []Status
}

func startManager(t *testing.T, prepare func(n int, c *fakeClient)) *harness {
	t.Helper()
	h := &harness{t: t, ff: &fakeFactory{prepare: prepare}, logs: &syncBuffer{}, done: make(chan struct{})}
	h.path = filepath.Join(t.TempDir(), "session.json")
	log := zerolog.New(h.logs)
	h.mgr = NewManager(ManagerConfig{Auth: pairedAuth(), SessionPath: h.path, NewClient: h.ff.newClient, Log: log, LibraryLog: log})
	h.mgr.backoff = func(int) time.Duration { return time.Millisecond }
	h.mgr.activateAfter = time.Millisecond
	h.mgr.Tracker().Subscribe(func(s Status) {
		h.mu.Lock()
		h.history = append(h.history, s)
		h.mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.mgr.Run(ctx)
		close(h.done)
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		h.t.Fatal("Run did not return after cancel")
	}
}

// connected waits for client i to have been connected and returns it.
func (h *harness) connected(i int) *fakeClient {
	h.t.Helper()
	eventually(h.t, fmt.Sprintf("client %d connected", i), func() bool {
		if h.ff.count() <= i {
			return false
		}
		n := 0
		h.ff.client(i).get(func(c *fakeClient) { n = c.connects })
		return n > 0
	})
	return h.ff.client(i)
}

func (h *harness) waitStatus(want Status) {
	h.t.Helper()
	eventually(h.t, fmt.Sprintf("status %v", want), func() bool { return h.mgr.Tracker().Status() == want })
}

func alert(t gmproto.AlertType) *gmproto.UserAlertEvent { return &gmproto.UserAlertEvent{AlertType: t} }

func TestConnectReachesOKAndSavesOnStop(t *testing.T) {
	h := startManager(t, nil)
	c := h.connected(0)
	h.waitStatus(Status{StatePhoneOffline, DetailConnecting})
	c.emit(&events.ListenRecovered{})
	h.waitStatus(Status{StatePhoneOffline, DetailConnecting})
	c.emit(alert(gmproto.AlertType_BROWSER_ACTIVE))
	h.waitStatus(Status{StateOK, ""})

	h.stop()
	fi, err := os.Stat(h.path)
	if err != nil {
		t.Fatalf("session should be saved on stop: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("session file mode %v", fi.Mode().Perm())
	}
	c.get(func(c *fakeClient) {
		if !c.disconnected {
			t.Error("client should be disconnected on stop")
		}
	})
	assertNoSecrets(t, "logs", h.logs.String())
}

func TestEventsDriveStatusAndNoTakeBack(t *testing.T) {
	h := startManager(t, nil)
	c := h.connected(0)
	c.emit(&events.ListenRecovered{})
	c.emit(alert(gmproto.AlertType_BROWSER_ACTIVE))
	h.waitStatus(Status{StateOK, ""})

	c.emit(&events.PhoneNotResponding{})
	h.waitStatus(Status{StatePhoneOffline, DetailPhoneNotResponding})
	c.emit(&events.PhoneRespondingAgain{})
	h.waitStatus(Status{StateOK, ""})
	c.emit(&events.ListenTemporaryError{Error: errors.New("http 502 while polling")})
	h.waitStatus(Status{StatePhoneOffline, DetailConnectionLost})
	c.emit(&events.ListenRecovered{})
	h.waitStatus(Status{StateOK, ""})
	c.emit(alert(gmproto.AlertType_BROWSER_INACTIVE_FROM_INACTIVITY))
	h.waitStatus(Status{StateBrowserInactive, DetailInactivity})
	c.emit(alert(gmproto.AlertType_BROWSER_ACTIVE))
	h.waitStatus(Status{StateOK, ""})
	c.emit(alert(gmproto.AlertType_BROWSER_INACTIVE))
	h.waitStatus(Status{StateBrowserInactive, DetailAnotherBrowser})

	time.Sleep(20 * time.Millisecond)
	c.get(func(c *fakeClient) {
		if c.setActiveCalls != 0 {
			t.Errorf("bridge took the phone back %d times; it must not", c.setActiveCalls)
		}
	})
	if h.ff.count() != 1 {
		t.Errorf("no reconnect expected, got %d clients", h.ff.count())
	}
}

func TestLoggedOutPaths(t *testing.T) {
	cases := []struct {
		name   string
		evt    any
		detail string
	}{
		{"pair revoked", &gmproto.RevokePairData{}, DetailPairRevoked},
		{"gaia logged out", &events.GaiaLoggedOut{}, DetailGoogleLoggedOut},
		{"invalid credentials", &events.ListenFatalError{Error: fmt.Errorf("failed to refresh auth token: %w", events.ErrInvalidCredentials)}, DetailBadCredentials},
		{"http 401 polling", &events.ListenFatalError{Error: events.HTTPError{Action: "polling", Resp: &http.Response{StatusCode: 401}}}, DetailBadCredentials},
		{"unpaired ping", &events.PingFailed{Error: fmt.Errorf("x: %w", events.ErrRequestedEntityNotFound), ErrorCount: 1}, DetailUnpaired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := startManager(t, nil)
			c := h.connected(0)
			c.emit(&events.ListenRecovered{})
			c.emit(alert(gmproto.AlertType_BROWSER_ACTIVE))
			h.waitStatus(Status{StateOK, ""})
			c.emit(tc.evt)
			h.waitStatus(Status{StateLoggedOut, tc.detail})
			eventually(t, "client disconnected", func() bool {
				d := false
				c.get(func(c *fakeClient) { d = c.disconnected })
				return d
			})
			c.emit(alert(gmproto.AlertType_BROWSER_ACTIVE)) // late events change nothing
			time.Sleep(20 * time.Millisecond)
			if got := h.mgr.Tracker().Status(); got != (Status{StateLoggedOut, tc.detail}) {
				t.Errorf("logged_out should be final, got %v", got)
			}
			if h.ff.count() != 1 {
				t.Errorf("must not reconnect after logged_out, got %d clients", h.ff.count())
			}
			h.stop()
			if _, err := os.Stat(h.path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a dead session must not be written on stop (err=%v)", err)
			}
			if !strings.Contains(h.logs.String(), "pair again") {
				t.Error("logs should say how to pair again")
			}
			assertNoSecrets(t, "logs", h.logs.String())
		})
	}
}

func TestConnectFailures(t *testing.T) {
	t.Run("credentials rejected on connect", func(t *testing.T) {
		h := startManager(t, func(_ int, c *fakeClient) {
			c.connectErr = fmt.Errorf("failed to refresh auth token: %w", events.ErrInvalidCredentials)
		})
		h.connected(0)
		h.waitStatus(Status{StateLoggedOut, DetailBadCredentials})
		time.Sleep(20 * time.Millisecond)
		if h.ff.count() != 1 {
			t.Errorf("must not retry, got %d clients", h.ff.count())
		}
	})
	t.Run("signed-out account in config", func(t *testing.T) {
		h := startManager(t, func(_ int, c *fakeClient) { c.email = "" })
		eventually(t, "client created", func() bool { return h.ff.count() == 1 })
		h.waitStatus(Status{StateLoggedOut, DetailGoogleLoggedOut})
		h.ff.client(0).get(func(c *fakeClient) {
			if c.connects != 0 {
				t.Error("should not connect with a signed-out account")
			}
		})
	})
	t.Run("config fetch fails, connects anyway", func(t *testing.T) {
		h := startManager(t, func(_ int, c *fakeClient) { c.fetchErr = errors.New("dial tcp: no route to host") })
		h.connected(0)
	})
	t.Run("other connect error retries with a new client", func(t *testing.T) {
		h := startManager(t, func(n int, c *fakeClient) {
			if n == 0 {
				c.connectErr = errors.New("something transient")
			}
		})
		h.connected(1)
		h.ff.client(0).get(func(c *fakeClient) {
			if !c.disconnected {
				t.Error("the failed client should be disconnected")
			}
		})
	})
}

func TestNonCredentialFatalErrorReconnects(t *testing.T) {
	h := startManager(t, nil)
	c0 := h.connected(0)
	c0.emit(&events.ListenRecovered{})
	c0.emit(alert(gmproto.AlertType_BROWSER_ACTIVE))
	h.waitStatus(Status{StateOK, ""})
	c0.emit(&events.ListenFatalError{Error: errors.New("failed to reconnect: something odd")})
	c1 := h.connected(1)
	h.waitStatus(Status{StatePhoneOffline, DetailConnecting})
	c0.emit(alert(gmproto.AlertType_BROWSER_ACTIVE)) // the old client is ignored
	time.Sleep(20 * time.Millisecond)
	if got := h.mgr.Tracker().Status(); got.State == StateOK {
		t.Error("an event from the replaced client changed the status")
	}
	c1.emit(&events.ListenRecovered{})
	c1.emit(alert(gmproto.AlertType_BROWSER_ACTIVE))
	h.waitStatus(Status{StateOK, ""})
}

func TestNoReconnectWhileTakenOver(t *testing.T) {
	h := startManager(t, nil)
	c := h.connected(0)
	c.emit(&events.ListenRecovered{})
	c.emit(alert(gmproto.AlertType_BROWSER_INACTIVE))
	h.waitStatus(Status{StateBrowserInactive, DetailAnotherBrowser})
	c.emit(&events.ListenFatalError{Error: errors.New("failed to reconnect: something odd")})
	time.Sleep(30 * time.Millisecond)
	if h.ff.count() != 1 {
		t.Errorf("reconnecting would take the phone back; got %d clients", h.ff.count())
	}
}

func TestTokenRefreshSavesSession(t *testing.T) {
	h := startManager(t, nil)
	c := h.connected(0)
	c.emit(&events.AuthTokenRefreshed{})
	eventually(t, "session file written", func() bool {
		_, err := os.Stat(h.path)
		return err == nil
	})
	b, _ := os.ReadFile(h.path)
	if !strings.Contains(string(b), "zzSAPISIDval666") {
		t.Error("the session file must keep the cookies, the connection needs them")
	}
}

func TestActivationRetry(t *testing.T) {
	t.Run("asks again when still not active", func(t *testing.T) {
		h := startManager(t, nil)
		c := h.connected(0)
		c.emit(&events.ListenRecovered{})
		c.emit(&events.HackySetActiveMayFail{})
		c.emit(&events.HackySetActiveMayFail{}) // at most once
		eventually(t, "SetActiveSession", func() bool {
			n := 0
			c.get(func(c *fakeClient) { n = c.setActiveCalls })
			return n == 1
		})
		time.Sleep(20 * time.Millisecond)
		c.get(func(c *fakeClient) {
			if c.setActiveCalls != 1 {
				t.Errorf("SetActiveSession called %d times", c.setActiveCalls)
			}
		})
	})
	t.Run("not after an inactive alert", func(t *testing.T) {
		h := startManager(t, nil)
		h.mgr.activateAfter = 30 * time.Millisecond
		c := h.connected(0)
		c.emit(&events.ListenRecovered{})
		c.emit(&events.HackySetActiveMayFail{})
		c.emit(alert(gmproto.AlertType_BROWSER_INACTIVE))
		time.Sleep(80 * time.Millisecond)
		c.get(func(c *fakeClient) {
			if c.setActiveCalls != 0 {
				t.Error("must not take the phone back from another web client")
			}
		})
	})
}
