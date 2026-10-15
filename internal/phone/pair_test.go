package phone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/session"
)

func TestPairWritesSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	ff := &fakeFactory{prepare: func(_ int, c *fakeClient) { c.emoji = "🦊" }}
	var shown string
	err := Pair(context.Background(), ff.newClient, testCookies(), path, zerolog.Nop(), func(e string) { shown = e })
	if err != nil {
		t.Fatal(err)
	}
	if shown != "🦊" {
		t.Errorf("emoji shown = %q", shown)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("session file mode %v", fi.Mode().Perm())
	}
	auth, err := session.Load(path)
	if err != nil {
		t.Fatalf("the written session should load: %v", err)
	}
	if !auth.IsGoogleAccount() || auth.Cookies["SAPISID"] != "zzSAPISIDval666" {
		t.Error("session lost the Google account cookies")
	}
	ff.client(0).get(func(c *fakeClient) {
		if !c.disconnected {
			t.Error("the pairing client should be disconnected")
		}
	})
}

func TestPairFailures(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(int, *fakeClient)
		want    string
	}{
		{"wrong emoji", func(_ int, c *fakeClient) { c.finishErr = libgm.ErrIncorrectEmoji }, "wrong emoji"},
		{"phone silent", func(_ int, c *fakeClient) { c.startErr = libgm.ErrPairingInitTimeout }, "did not respond"},
		{"no phone", func(_ int, c *fakeClient) { c.startErr = libgm.ErrNoDevicesFound }, "no phone found"},
		{"config fails", func(_ int, c *fakeClient) { c.fetchErr = errors.New("http 401 while fetching config") }, "config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.json")
			ff := &fakeFactory{prepare: tc.prepare}
			err := Pair(context.Background(), ff.newClient, testCookies(), path, zerolog.Nop(), func(string) {})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
			assertNoSecrets(t, "error", err.Error())
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Error("no session file should be written when pairing fails")
			}
		})
	}
}

func TestPairRefusesExistingSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ff := &fakeFactory{}
	err := Pair(context.Background(), ff.newClient, testCookies(), path, zerolog.Nop(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("got %v", err)
	}
	if ff.count() != 0 {
		t.Error("no client should be created when a session already exists")
	}
}
