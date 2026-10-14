package session

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// googleAuth is a fake Google-account session: enough fields for Load to accept it.
func googleAuth() *libgm.AuthData {
	auth := libgm.NewAuthData()
	auth.SetCookies(map[string]string{"SID": sentinel["SID"], "SAPISID": sentinel["SAPISID"]})
	auth.DestRegID = uuid.New()
	auth.Browser = &gmproto.Device{SourceID: "browser"}
	auth.TachyonAuthToken = []byte("zzTokenval888")
	return auth
}

func TestSaveLoadRoundTripAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil { // an older, too-open file
		t.Fatal(err)
	}
	auth := googleAuth()
	if err := Save(path, auth); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("session file mode = %v, want 0600", fi.Mode().Perm())
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !back.IsGoogleAccount() || back.Cookies["SAPISID"] != sentinel["SAPISID"] || string(back.TachyonAuthToken) != "zzTokenval888" {
		t.Error("round trip lost data")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary file left behind: %d entries", len(entries))
	}
}

func TestLoadMissingAndBroken(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "absent.json")); !errors.Is(err, ErrNoSession) {
		t.Errorf("missing file: got %v, want ErrNoSession", err)
	}
	cases := map[string]string{
		"not json":       `{"cookies": {"SID": "zzSIDval111"`,
		"wrong shape":    `{"cookies": "zzSIDval111"}`,
		"not paired":     `{"cookies": {"SID": "zzSIDval111"}}`,
		"google no jar":  `{"browser": {"sourceID": "b"}, "tachyon_token": "AQ==", "dest_reg_id": "` + uuid.NewString() + `"}`,
		"syntax in body": `{"tachyon_token": zzTokenval888}`,
	}
	for name, body := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		assertNoSentinel(t, name, err.Error())
		if strings.Contains(err.Error(), "zzTokenval888") {
			t.Errorf("%s: error shows the token", name)
		}
	}
}

// Save must hold the cookie lock: libgm rewrites the cookie map after every HTTP response.
// Under -race this test fails if Save reads auth.Cookies without the read lock.
func TestSaveHoldsCookieLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	auth := googleAuth()
	resp := &http.Response{Header: http.Header{"Set-Cookie": {"SID=zzSIDval111; Path=/", "NEW=v; Path=/"}}}
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				auth.UpdateCookiesFromResponse(resp)
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if err := Save(path, auth); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	wg.Wait()
}
