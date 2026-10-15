// [INPUT]: client.go Client/NewClientFunc, internal/session Save, libgm pairing errors
// [OUTPUT]: Pair
// [POS]: internal/phone's Google-account pairing flow behind the pair command; writes the session Manager later loads
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package phone

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/session"
)

// Pair pairs with the phone through the Google account in cookies (QR pairing no longer exists),
// calls showEmoji with the emoji the operator must tap on the phone, and writes the session file
// (mode 0600) at sessionPath. It refuses to overwrite an existing session file.
func Pair(ctx context.Context, newClient NewClientFunc, cookies map[string]string, sessionPath string,
	libLog zerolog.Logger, showEmoji func(emoji string)) error {
	if _, err := os.Stat(sessionPath); err == nil {
		return fmt.Errorf("session file %s already exists; delete it first to pair again", sessionPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check session file: %w", err)
	}
	auth := libgm.NewAuthData()
	auth.SetCookies(cookies)
	cli := newClient(auth, libLog)
	cli.SetEventHandler(func(any) {}) // nothing to do until paired
	defer cli.Disconnect()

	lctx := libLog.WithContext(ctx) // libgm's pairing code logs through the context logger
	// FetchConfig also sets AuthData.SessionID, which pairing uses as the browser device id.
	if err := cli.FetchConfig(lctx); err != nil {
		return fmt.Errorf("fetching the Google Messages config failed (are the cookies from a signed-in private window?): %w", err)
	}
	bgCtx, cancel := context.WithCancel(lctx)
	defer cancel()
	emoji, ps, err := cli.StartGaiaPairing(lctx, bgCtx)
	if err != nil {
		return pairError("starting pairing", err)
	}
	showEmoji(emoji)
	if _, err = cli.FinishGaiaPairing(lctx, ps); err != nil { // its result names the account: not logged
		return pairError("finishing pairing", err)
	}
	if err = session.Save(sessionPath, auth); err != nil {
		return err
	}
	return nil
}

func pairError(step string, err error) error {
	hint := "pairing failed"
	switch {
	case errors.Is(err, libgm.ErrNoDevicesFound):
		hint = "no phone found: the phone's Google Messages must be signed in to the same Google account, with account pairing allowed"
	case errors.Is(err, libgm.ErrHadMultipleDevices):
		hint = "the account has several phones that look primary, the wrong one may have been picked; try again"
	case errors.Is(err, libgm.ErrPairingInitTimeout):
		hint = "the phone did not respond: keep Google Messages open in the foreground and battery optimization off, then try again"
	case errors.Is(err, events.ErrCallerNoPermission):
		hint = "this Google account may not use Google Messages for web"
	case errors.Is(err, libgm.ErrIncorrectEmoji):
		hint = "the wrong emoji was tapped on the phone"
	case errors.Is(err, libgm.ErrPairingCancelled):
		hint = "pairing was cancelled on the phone"
	case errors.Is(err, libgm.ErrPairingTimeout):
		hint = "pairing timed out: the emoji was not tapped in time"
	}
	return fmt.Errorf("%s: %s: %w", step, hint, err)
}
