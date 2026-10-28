// [INPUT]: env.go config, internal/logx loggers, internal/delivery Ledger, internal/session (cookies, session file), internal/phone (Pair, Manager),
//          internal/render Renderer, internal/ws Server, internal/edge (Enroll, Heartbeat)
// [OUTPUT]: the sms-bridge binary: `sms-bridge pair -cookies <file>`, `sms-bridge enroll <code>` and `sms-bridge serve`
// [POS]: cmd/sms-bridge, the entry point launchd runs (serve: phone Manager + renderer + delivery ledger + WebSocket server
//        + edge heartbeat once enrolled) and the operator runs once (pair, enroll)
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

// Command sms-bridge relays an Android phone's Google Messages (SMS/RCS) to the AskDAO conductor.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/delivery"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/edge"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/logx"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/phone"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/render"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/session"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/ws"
)

const usage = `usage:
  sms-bridge pair -cookies <file>   pair with the phone through Google account cookies, write the session file
  sms-bridge enroll <code>          register with conductor as an edge node (one-time code), write the identity file
  sms-bridge serve                  run the bridge with the saved session

Settings come from the environment and from .env in the working directory (see .env.example).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := loadDotEnv(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "sms-bridge: reading .env:", err)
		os.Exit(1)
	}
	cfg := loadConfig()
	var err error
	switch os.Args[1] {
	case "pair":
		err = runPair(cfg, os.Args[2:])
	case "enroll":
		err = runEnroll(cfg, os.Args[2:])
	case "serve":
		err = runServe(cfg)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sms-bridge:", err)
		os.Exit(1)
	}
}

func runPair(cfg config, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	cookiesPath := fs.String("cookies", "", "file holding the Google account cookies (a browser \"Copy as cURL\" text, or JSON)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cookiesPath == "" {
		return errors.New("pair needs -cookies <file>")
	}
	raw, err := os.ReadFile(*cookiesPath)
	if err != nil {
		return fmt.Errorf("reading the cookie file: %w", err)
	}
	cookies, err := session.ParseCookies(string(raw))
	if err != nil {
		return fmt.Errorf("cookie file %s: %w", *cookiesPath, err)
	}
	log := logx.New(os.Stdout, os.Stderr)
	log.Info().Strs("cookie_names", session.CookieNames(cookies)).Msg("starting Google account pairing; keep Google Messages open on the phone")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = phone.Pair(ctx, phone.NewLibgmClient, cookies, cfg.SessionFile, logx.Library(os.Stdout, os.Stderr), func(emoji string) {
		fmt.Printf("\n    Tap this emoji on the phone:  %s\n\n", emoji)
	})
	if err != nil {
		return err
	}
	fmt.Printf("Paired. Session saved to %s. Delete the cookie file now: rm -f %s\n", cfg.SessionFile, *cookiesPath)
	return nil
}

func runEnroll(cfg config, args []string) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("enroll needs the one-time code: sms-bridge enroll <code>")
	}
	if cfg.EdgeURL == "" {
		return errors.New("EDGE_CONDUCTOR_URL is empty; set conductor's API address in .env")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := edge.Enroll(ctx, &http.Client{}, cfg.EdgeURL, args[0], cfg.EdgeIdentity)
	if err != nil {
		return err
	}
	fmt.Printf("Enrolled as edge node %s (%s). Identity saved to %s. Restart the service to start the heartbeat.\n",
		id.Name, id.Kind, cfg.EdgeIdentity)
	return nil
}

// capabilities is what the heartbeat declares.
var capabilities = []string{"channel:sms", "feature:send_image", "feature:seq_ack"}

// buildVersion is the commit the binary was built from (go build embeds it in a git checkout).
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, st := range info.Settings {
		switch st.Key {
		case "vcs.revision":
			rev = st.Value
		case "vcs.modified":
			dirty = st.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && dirty {
		rev += "+dirty"
	}
	return rev
}

// health is degraded while the phone is not ok or conductor is not connected.
func health(tracker *phone.Tracker, srv *ws.Server) edge.Health {
	st := tracker.Status()
	switch {
	case st.State != phone.StateOK:
		return edge.Health{Status: "degraded", Detail: "phone " + string(st.State) + "/" + st.Detail}
	case !srv.Connected():
		return edge.Health{Status: "degraded", Detail: "conductor not connected"}
	}
	return edge.Health{Status: "ok"}
}

func runServe(cfg config) error {
	if err := cfg.validateServe(); err != nil {
		return err
	}
	log := logx.New(os.Stdout, os.Stderr)
	auth, err := session.Load(cfg.SessionFile)
	if err != nil {
		return fmt.Errorf("%s: %w", cfg.SessionFile, err)
	}
	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listening for conductor: %w", err)
	}
	if _, err := os.Stat(cfg.ChromePath); err != nil {
		log.Warn().Str("chrome_path", cfg.ChromePath).Msg("Google Chrome not found: send_image frames will go out as their fallback texts; install Chrome or set BRIDGE_CHROME_PATH")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mgr := phone.NewManager(phone.ManagerConfig{
		Auth:        auth,
		SessionPath: cfg.SessionFile,
		NewClient:   phone.NewLibgmClient,
		Log:         log,
		LibraryLog:  logx.Library(os.Stdout, os.Stderr),
		Render:      render.New(cfg.ChromePath).Render,
	})
	ledger := delivery.New(cfg.StateFile, cfg.ReplayWindow, log)
	srv := ws.New(cfg.Token, cfg.ObserverToken, mgr, ledger, log)
	mgr.OnInbound(srv.Deliver)
	mgr.Tracker().Subscribe(srv.PublishStatus)
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			stop() // exit with an error so launchd restarts the service
		}
	}()
	switch id, err := edge.ReadIdentity(cfg.EdgeIdentity); {
	case errors.Is(err, edge.ErrNoIdentity):
		log.Info().Msg("not enrolled as an edge node: no heartbeat (run `sms-bridge enroll <code>` once)")
	case err != nil:
		log.Error().Err(err).Str("file", cfg.EdgeIdentity).Msg("edge identity file unreadable: no heartbeat")
	case cfg.EdgeURL == "":
		log.Error().Msg("EDGE_CONDUCTOR_URL is empty: no heartbeat")
	default:
		host, _ := os.Hostname()
		hb := edge.NewHeartbeat(&http.Client{}, cfg.EdgeURL, id, capabilities,
			func() edge.Health { return health(mgr.Tracker(), srv) }, buildVersion(), host, log)
		go hb.Run(ctx)
	}
	log.Info().Str("port", cfg.Port).Msg("starting; conductor connects over WebSocket")
	mgr.Run(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	select {
	case err := <-serveErr:
		return fmt.Errorf("WebSocket server stopped: %w", err)
	default:
		return nil
	}
}
