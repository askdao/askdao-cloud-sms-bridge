// [INPUT]: go.mau.fi/mautrix-gmessages/pkg/libgm Client, go.mau.fi/util/exhttp
// [OUTPUT]: Client (the libgm subset the bridge uses), NewClientFunc, NewLibgmClient
// [POS]: internal/phone's seam to libgm; manager.go and pair.go only talk to Client, tests supply a fake
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

// Package phone drives Google Messages through libgm on behalf of the paired Android phone:
// pairing, the connection and its status.
package phone

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

// Client is the subset of *libgm.Client the bridge uses. libgm calls the event handler
// synchronously; the handler must not make requests or block.
type Client interface {
	SetEventHandler(h libgm.EventHandler)
	FetchConfig(ctx context.Context) error
	// AccountEmail is the Google account in the config fetched by FetchConfig, "" when signed out.
	AccountEmail() string
	Connect(ctx context.Context) error
	Disconnect()
	SetActiveSession(ctx context.Context) error
	StartGaiaPairing(ctx, bgCtx context.Context) (string, *libgm.PairingSession, error)
	FinishGaiaPairing(ctx context.Context, ps *libgm.PairingSession) (string, error)
}

// NewClientFunc builds a Client around auth.
type NewClientFunc func(auth *libgm.AuthData, log zerolog.Logger) Client

// pingInterval is upstream's default: how often libgm checks that the phone responds.
const pingInterval = 20 * time.Minute

// NewLibgmClient is the production NewClientFunc.
func NewLibgmClient(auth *libgm.AuthData, log zerolog.Logger) Client {
	cli := libgm.NewClient(auth, nil, log, exhttp.SensibleClientSettings)
	cli.SetPingInterval(pingInterval)
	return libgmClient{cli}
}

type libgmClient struct{ *libgm.Client }

func (c libgmClient) AccountEmail() string { return c.Config.GetDeviceInfo().GetEmail() }
