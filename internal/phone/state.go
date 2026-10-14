// [INPUT]: github.com/rs/zerolog for the one-line change log
// [OUTPUT]: State and its four values, Status, Tracker (Status, Subscribe and the setters manager.go calls)
// [POS]: internal/phone's status model; manager.go feeds it libgm events, subscribers (the status frame) read it
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package phone

import (
	"sync"

	"github.com/rs/zerolog"
)

// State is the bridge's connection state as reported to conductor.
type State string

const (
	// StateOK: connected, the phone responds and the bridge is its active web client.
	StateOK State = "ok"
	// StatePhoneOffline: still connecting, the connection to Google dropped, or the phone does not respond.
	StatePhoneOffline State = "phone_offline"
	// StateBrowserInactive: the phone made another client active; the bridge does not take it back.
	StateBrowserInactive State = "browser_inactive"
	// StateLoggedOut: pairing revoked or the Google account signed out; needs pairing again.
	StateLoggedOut State = "logged_out"
)

// Detail values that go with each state.
const (
	DetailConnecting         = "connecting"           // phone_offline: not yet listening, or not yet made active
	DetailConnectionLost     = "connection_lost"      // phone_offline: the long-poll connection to Google failed
	DetailPhoneNotResponding = "phone_not_responding" // phone_offline: libgm's pings to the phone time out
	DetailAnotherBrowser     = "another_browser"      // browser_inactive: BROWSER_INACTIVE
	DetailTimeout            = "timeout"              // browser_inactive: BROWSER_INACTIVE_FROM_TIMEOUT
	DetailInactivity         = "inactivity"           // browser_inactive: BROWSER_INACTIVE_FROM_INACTIVITY
	DetailPairRevoked        = "pair_revoked"         // logged_out: the phone unpaired this device
	DetailGoogleLoggedOut    = "google_logged_out"    // logged_out: the Google account session ended
	DetailUnpaired           = "unpaired"             // logged_out: Google no longer knows this pairing
	DetailBadCredentials     = "credentials_rejected" // logged_out: Google rejected the saved credentials
)

// Status is a state with its detail.
type Status struct {
	State  State
	Detail string
}

// Tracker derives the Status from what libgm has reported. Priority:
// logged_out > browser_inactive > phone_offline > ok. logged_out is final for the process.
type Tracker struct {
	mu              sync.Mutex
	log             zerolog.Logger
	listening       bool
	everListened    bool
	phoneResponding bool
	active          bool
	inactive        string
	loggedOut       string
	cur             Status
	subs            []func(Status)
}

// NewTracker starts at phone_offline/connecting.
func NewTracker(log zerolog.Logger) *Tracker {
	t := &Tracker{log: log, phoneResponding: true}
	t.cur = t.derive()
	return t
}

// Status returns the current status.
func (t *Tracker) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}

// Subscribe calls fn with the current status now and again after every change, in order.
// fn runs with the tracker locked: it must return quickly and must not call the tracker.
func (t *Tracker) Subscribe(fn func(Status)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subs = append(t.subs, fn)
	fn(t.cur)
}

// SetListening records whether libgm's long-poll connection to Google is open.
func (t *Tracker) SetListening(v bool) {
	t.update(func() {
		t.listening = v
		t.everListened = t.everListened || v
	})
}

// SetPhoneResponding records libgm's PhoneNotResponding / PhoneRespondingAgain.
func (t *Tracker) SetPhoneResponding(v bool) { t.update(func() { t.phoneResponding = v }) }

// SetActive records BROWSER_ACTIVE: the phone made this bridge its active client.
func (t *Tracker) SetActive() { t.update(func() { t.active, t.inactive = true, "" }) }

// SetInactive records one of the BROWSER_INACTIVE alerts with its detail.
func (t *Tracker) SetInactive(detail string) {
	t.update(func() { t.active, t.inactive = false, detail })
}

// SetLoggedOut records that the session can no longer be used. The first reason wins.
func (t *Tracker) SetLoggedOut(detail string) {
	t.update(func() {
		if t.loggedOut == "" {
			t.loggedOut = detail
		}
	})
}

// ResetConnection forgets connection facts before a new client connects. logged_out stays.
func (t *Tracker) ResetConnection() {
	t.update(func() {
		t.listening, t.everListened, t.phoneResponding, t.active, t.inactive = false, false, true, false, ""
	})
}

// TakenOver reports whether another web client made the phone drop this bridge.
func (t *Tracker) TakenOver() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inactive == DetailAnotherBrowser
}

// AwaitingActivation reports whether the bridge is connected and the phone responds,
// but the phone has not yet made the bridge its active client, with no inactive alert either.
func (t *Tracker) AwaitingActivation() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.loggedOut == "" && !t.active && t.inactive == "" && t.phoneResponding
}

func (t *Tracker) update(change func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	change()
	next := t.derive()
	if next == t.cur {
		return
	}
	prev := t.cur
	t.cur = next
	evt := t.log.Info()
	if next.State != StateOK {
		evt = t.log.Warn()
	}
	evt.Str("from", string(prev.State)+"/"+prev.Detail).
		Str("to", string(next.State)+"/"+next.Detail).
		Msg("status changed")
	for _, fn := range t.subs {
		fn(next)
	}
}

func (t *Tracker) derive() Status {
	switch {
	case t.loggedOut != "":
		return Status{StateLoggedOut, t.loggedOut}
	case t.inactive != "":
		return Status{StateBrowserInactive, t.inactive}
	case !t.listening && t.everListened:
		return Status{StatePhoneOffline, DetailConnectionLost}
	case !t.listening:
		return Status{StatePhoneOffline, DetailConnecting}
	case !t.phoneResponding:
		return Status{StatePhoneOffline, DetailPhoneNotResponding}
	case !t.active:
		return Status{StatePhoneOffline, DetailConnecting}
	default:
		return Status{StateOK, ""}
	}
}
