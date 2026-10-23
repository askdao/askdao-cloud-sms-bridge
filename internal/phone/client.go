// [INPUT]: go.mau.fi/mautrix-gmessages/pkg/libgm Client and gmproto, go.mau.fi/util/exhttp
// [OUTPUT]: Client (the libgm subset the bridge uses), NewClientFunc, NewLibgmClient
// [POS]: internal/phone's seam to libgm; manager.go, pair.go and outbound.go (texts, reactions, media uploads)
//        only talk to Client, tests supply a fake
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

// Package phone drives Google Messages through libgm on behalf of the paired Android phone:
// pairing, the connection and its status, inbound messages and outbound sends.
package phone

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
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
	GetConversation(ctx context.Context, conversationID string) (*gmproto.Conversation, error)
	GetOrCreateConversation(ctx context.Context, req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error)
	// SendMessage returns once the phone accepted the message; the real message id only
	// arrives later, in the echoed message event carrying the same tmp id.
	SendMessage(ctx context.Context, req *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)
	SendReaction(ctx context.Context, req *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error)
	// UploadMedia encrypts data and uploads it to Google (not to the phone); the returned
	// MediaContent goes into a SendMessage part. It takes no context: libgm's HTTP client
	// timeouts bound it.
	UploadMedia(data []byte, fileName, mime string) (*gmproto.MediaContent, error)
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
