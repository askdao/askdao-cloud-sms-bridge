// [INPUT]: libgm AuthData (with its CookiesLock), encoding/json, os
// [OUTPUT]: Load, Save, ErrNoSession
// [POS]: internal/session's session-file store; the pair command creates the file, phone.Manager rewrites it
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

// ErrNoSession means the session file does not exist: the bridge has not been paired yet.
var ErrNoSession = errors.New("no session file; run the pair command first")

// Load reads a session written by Save. It returns ErrNoSession when the file does not exist.
// Errors never quote the file's content, which is a live login.
func Load(path string) (*libgm.AuthData, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSession
	} else if err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}
	auth := &libgm.AuthData{}
	if err = json.Unmarshal(b, auth); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return nil, fmt.Errorf("session file is not valid JSON (near byte %d)", se.Offset)
		}
		return nil, errors.New("session file does not have the expected structure")
	}
	if auth.Browser == nil || auth.TachyonAuthToken == nil {
		return nil, errors.New("session file has no paired device or token")
	}
	if auth.IsGoogleAccount() && !auth.HasCookies() {
		return nil, errors.New("session file is a Google account session without cookies")
	}
	return auth, nil
}

// Save writes auth to path with mode 0600, replacing the file atomically.
// libgm rewrites auth.Cookies in place under CookiesLock after every HTTP response,
// so the read lock is held while encoding.
func Save(path string, auth *libgm.AuthData) error {
	auth.CookiesLock.RLock()
	b, err := json.Marshal(auth)
	auth.CookiesLock.RUnlock()
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*") // created with mode 0600
	if err != nil {
		return fmt.Errorf("create temporary session file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write temporary session file: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace session file: %w", err)
	}
	return nil
}
