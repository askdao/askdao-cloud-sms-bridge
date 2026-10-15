// [INPUT]: client.go Client/NewClientFunc, state.go Tracker, internal/session Save, libgm events and gmproto alerts
// [OUTPUT]: Manager (NewManager, Run, Tracker), ManagerConfig
// [POS]: internal/phone's connection owner: connects with the saved session, turns libgm events into Tracker
//        changes, saves the session, reconnects after non-credential failures, never takes the phone back
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package phone

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/session"
)

// ManagerConfig holds what NewManager needs.
type ManagerConfig struct {
	Auth        *libgm.AuthData
	SessionPath string
	NewClient   NewClientFunc
	Log         zerolog.Logger // the bridge's own logger
	LibraryLog  zerolog.Logger // handed to libgm
}

// Manager owns the libgm client for one paired phone.
type Manager struct {
	auth        *libgm.AuthData
	sessionPath string
	newClient   NewClientFunc
	log         zerolog.Logger
	libLog      zerolog.Logger
	state       *Tracker

	// test hooks
	backoff       func(attempt int) time.Duration
	activateAfter time.Duration

	mu  sync.Mutex
	cli Client

	saveMu      sync.Mutex
	stop        chan struct{} // logged out: stop using the client
	reconnect   chan struct{} // a non-credential fatal error: start a new client
	stopOnce    sync.Once
	activateTry sync.Once
}

// NewManager prepares a Manager; Run starts it.
func NewManager(cfg ManagerConfig) *Manager {
	return &Manager{
		auth:          cfg.Auth,
		sessionPath:   cfg.SessionPath,
		newClient:     cfg.NewClient,
		log:           cfg.Log,
		libLog:        cfg.LibraryLog,
		state:         NewTracker(cfg.Log),
		backoff:       reconnectBackoff,
		activateAfter: 7 * time.Second,
		stop:          make(chan struct{}),
		reconnect:     make(chan struct{}, 1),
	}
}

// Tracker exposes the status for subscribers.
func (m *Manager) Tracker() *Tracker { return m.state }

// errLoggedOut is returned by connectOnce when the session can no longer be used.
var errLoggedOut = errors.New("logged out")

// Run connects and keeps the connection until ctx ends, then saves the session and disconnects.
// After logged_out it stays idle (the process keeps reporting the state) until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	defer m.shutdown()
	attempt := 0
	for {
		err := m.connectOnce(ctx)
		switch {
		case errors.Is(err, errLoggedOut):
			<-ctx.Done()
			return
		case err == nil:
			attempt = 0
			select {
			case <-ctx.Done():
				return
			case <-m.stop:
				m.dropClient()
				<-ctx.Done()
				return
			case <-m.reconnect:
			}
		default:
			m.log.Warn().Err(err).Msg("connect failed")
		}
		m.dropClient()
		if m.state.TakenOver() { // reconnecting would make the bridge the active client again
			m.log.Warn().Msg("not reconnecting while another web client is active; restart the service to take over")
			select {
			case <-ctx.Done():
			case <-m.stop:
			}
			return
		}
		m.state.ResetConnection()
		delay := m.backoff(attempt)
		attempt++
		m.log.Info().Dur("in", delay).Msg("reconnecting")
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			<-ctx.Done()
			return
		case <-time.After(delay):
		}
	}
}

// reconnectBackoff: 5s, 10s, 20s, ... capped at 320s.
func reconnectBackoff(attempt int) time.Duration {
	return 5 * time.Second << min(attempt, 6)
}

func (m *Manager) connectOnce(ctx context.Context) error {
	cli := m.newClient(m.auth, m.libLog)
	cli.SetEventHandler(func(evt any) { m.handleEvent(cli, evt) })
	m.mu.Lock()
	m.cli = cli
	m.mu.Unlock()

	if m.auth.IsGoogleAccount() && !m.auth.HasCookies() {
		m.loggedOut(DetailBadCredentials, "session has no Google account cookies")
		return errLoggedOut
	}
	lctx := m.libLog.WithContext(ctx)
	if err := cli.FetchConfig(lctx); err != nil {
		m.log.Warn().Err(err).Msg("fetching config failed, connecting anyway") // as upstream does
	} else if m.auth.IsGoogleAccount() && cli.AccountEmail() == "" {
		m.loggedOut(DetailGoogleLoggedOut, "config has no Google account")
		return errLoggedOut
	}
	if err := cli.Connect(lctx); err != nil {
		if detail := credentialFailure(err); detail != "" {
			m.loggedOut(detail, "Google rejected the session: "+err.Error())
			return errLoggedOut
		}
		return err
	}
	m.log.Info().Msg("connected, waiting for the phone")
	return nil
}

// credentialFailure classifies errors that retrying cannot fix; "" for anything else.
func credentialFailure(err error) string {
	switch {
	case errors.Is(err, events.ErrRequestedEntityNotFound):
		return DetailUnpaired
	case errors.Is(err, events.ErrInvalidCredentials):
		return DetailBadCredentials
	}
	var he events.HTTPError
	if errors.As(err, &he) && he.Resp != nil {
		switch he.Resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return DetailBadCredentials
		}
	}
	return ""
}

// handleEvent is libgm's synchronous callback: no requests, no blocking.
func (m *Manager) handleEvent(cli Client, raw any) {
	if !m.isCurrent(cli) {
		return // a late event from a client already replaced
	}
	switch evt := raw.(type) {
	case *events.ListenRecovered:
		m.state.SetListening(true)
	case *events.ListenTemporaryError:
		m.log.Warn().Err(evt.Error).Msg("connection to Google failed, libgm retries")
		m.state.SetListening(false)
	case *events.ListenFatalError:
		if detail := credentialFailure(evt.Error); detail != "" {
			m.loggedOut(detail, "Google rejected the session: "+evt.Error.Error())
			return
		}
		m.log.Error().Err(evt.Error).Msg("connection to Google stopped")
		m.state.SetListening(false)
		select {
		case m.reconnect <- struct{}{}:
		default:
		}
	case *events.PhoneNotResponding:
		m.state.SetPhoneResponding(false)
	case *events.PhoneRespondingAgain:
		m.state.SetPhoneResponding(true)
	case *events.PingFailed:
		if errors.Is(evt.Error, events.ErrRequestedEntityNotFound) {
			m.loggedOut(DetailUnpaired, "Google no longer knows this pairing")
			return
		}
		m.log.Warn().Err(evt.Error).Int("error_count", evt.ErrorCount).Msg("ping to the phone failed")
	case *gmproto.RevokePairData:
		m.loggedOut(DetailPairRevoked, "the phone unpaired this bridge")
	case *events.GaiaLoggedOut:
		m.loggedOut(DetailGoogleLoggedOut, "the Google account session ended")
	case *events.AuthTokenRefreshed:
		go m.saveSession()
	case *gmproto.UserAlertEvent:
		m.handleAlert(evt.GetAlertType())
	case *events.HackySetActiveMayFail:
		m.activateTry.Do(func() { go m.retryActivation(cli) })
	case *events.NoDataReceived:
		m.log.Info().Msg("no data from the phone for a while, libgm asked for updates")
	}
}

func (m *Manager) handleAlert(t gmproto.AlertType) {
	switch t {
	case gmproto.AlertType_BROWSER_ACTIVE:
		m.state.SetActive()
	case gmproto.AlertType_BROWSER_INACTIVE:
		// Another web client is active now. Deliberately no SetActiveSession: an operator
		// decides, by closing that client and restarting the service.
		m.log.Warn().Msg("another Google Messages web client took over; restart the service after closing it")
		m.state.SetInactive(DetailAnotherBrowser)
	case gmproto.AlertType_BROWSER_INACTIVE_FROM_TIMEOUT:
		m.state.SetInactive(DetailTimeout)
	case gmproto.AlertType_BROWSER_INACTIVE_FROM_INACTIVITY:
		m.state.SetInactive(DetailInactivity)
	}
}

// retryActivation follows upstream: when libgm warns that becoming the active client may have
// failed, check once a few seconds later and ask again, unless an inactive alert arrived meanwhile.
func (m *Manager) retryActivation(cli Client) {
	time.Sleep(m.activateAfter)
	if !m.isCurrent(cli) || !m.state.AwaitingActivation() {
		return
	}
	m.log.Warn().Msg("phone has not made the bridge active yet, asking again")
	if err := cli.SetActiveSession(m.libLog.WithContext(context.Background())); err != nil {
		m.log.Warn().Err(err).Msg("asking to become active failed")
	}
}

func (m *Manager) loggedOut(detail, why string) {
	m.state.SetLoggedOut(detail)
	m.log.Error().Str("detail", detail).Msg(why + "; pair again: stop the service, delete the session file, run the pair command, start the service")
	m.stopOnce.Do(func() { close(m.stop) })
}

func (m *Manager) isCurrent(cli Client) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cli == cli
}

func (m *Manager) dropClient() {
	m.mu.Lock()
	cli := m.cli
	m.cli = nil
	m.mu.Unlock()
	if cli != nil {
		cli.Disconnect()
	}
}

// saveSession writes the session file unless the session is already known to be dead.
func (m *Manager) saveSession() {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if m.state.Status().State == StateLoggedOut {
		return
	}
	if err := session.Save(m.sessionPath, m.auth); err != nil {
		m.log.Error().Err(err).Msg("saving the session failed")
		return
	}
	m.log.Info().Msg("session saved")
}

func (m *Manager) shutdown() {
	m.saveSession()
	m.dropClient()
	m.log.Info().Msg("stopped")
}
