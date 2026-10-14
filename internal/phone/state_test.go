package phone

import (
	"testing"

	"github.com/rs/zerolog"
)

func TestTrackerTransitions(t *testing.T) {
	tr := NewTracker(zerolog.Nop())
	var seen []Status
	tr.Subscribe(func(s Status) { seen = append(seen, s) })

	steps := []struct {
		name string
		do   func()
		want Status
	}{
		{"start", func() {}, Status{StatePhoneOffline, DetailConnecting}},
		{"long poll open", func() { tr.SetListening(true) }, Status{StatePhoneOffline, DetailConnecting}},
		{"phone made us active", tr.SetActive, Status{StateOK, ""}},
		{"phone stops responding", func() { tr.SetPhoneResponding(false) }, Status{StatePhoneOffline, DetailPhoneNotResponding}},
		{"phone back", func() { tr.SetPhoneResponding(true) }, Status{StateOK, ""}},
		{"long poll fails", func() { tr.SetListening(false) }, Status{StatePhoneOffline, DetailConnectionLost}},
		{"long poll back", func() { tr.SetListening(true) }, Status{StateOK, ""}},
		{"another browser", func() { tr.SetInactive(DetailAnotherBrowser) }, Status{StateBrowserInactive, DetailAnotherBrowser}},
		{"inactive beats offline", func() { tr.SetPhoneResponding(false) }, Status{StateBrowserInactive, DetailAnotherBrowser}},
		{"active again, phone still silent", tr.SetActive, Status{StatePhoneOffline, DetailPhoneNotResponding}},
		{"phone back again", func() { tr.SetPhoneResponding(true) }, Status{StateOK, ""}},
		{"inactive from timeout", func() { tr.SetInactive(DetailTimeout) }, Status{StateBrowserInactive, DetailTimeout}},
		{"logged out beats inactive", func() { tr.SetLoggedOut(DetailPairRevoked) }, Status{StateLoggedOut, DetailPairRevoked}},
		{"logged out is final", func() { tr.SetActive(); tr.SetLoggedOut(DetailUnpaired); tr.ResetConnection() }, Status{StateLoggedOut, DetailPairRevoked}},
	}
	for _, s := range steps {
		s.do()
		if got := tr.Status(); got != s.want {
			t.Fatalf("%s: got %v, want %v", s.name, got, s.want)
		}
	}
	// One notification per change: the initial status plus every step that changed it.
	want := []Status{
		{StatePhoneOffline, DetailConnecting},
		{StateOK, ""},
		{StatePhoneOffline, DetailPhoneNotResponding},
		{StateOK, ""},
		{StatePhoneOffline, DetailConnectionLost},
		{StateOK, ""},
		{StateBrowserInactive, DetailAnotherBrowser},
		{StatePhoneOffline, DetailPhoneNotResponding},
		{StateOK, ""},
		{StateBrowserInactive, DetailTimeout},
		{StateLoggedOut, DetailPairRevoked},
	}
	if len(seen) != len(want) {
		t.Fatalf("notifications: got %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("notification %d: got %v, want %v", i, seen[i], want[i])
		}
	}
}

func TestResetConnectionStartsOver(t *testing.T) {
	tr := NewTracker(zerolog.Nop())
	tr.SetListening(true)
	tr.SetActive()
	tr.SetInactive(DetailAnotherBrowser)
	if !tr.TakenOver() {
		t.Fatal("TakenOver should be true after BROWSER_INACTIVE")
	}
	tr.ResetConnection()
	if got := tr.Status(); got != (Status{StatePhoneOffline, DetailConnecting}) {
		t.Errorf("after reset: got %v", got)
	}
	if tr.TakenOver() {
		t.Error("reset should clear the inactive alert")
	}
}
