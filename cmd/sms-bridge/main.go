// [INPUT]: env.go config, internal/logx loggers, internal/delivery Ledger, internal/session (cookies, session file), internal/phone (Pair, Manager),
//          internal/render Renderer, internal/ws Server
// [OUTPUT]: the sms-bridge binary: `sms-bridge pair -cookies <file>` and `sms-bridge serve`
// [POS]: cmd/sms-bridge, the entry point launchd runs (serve: phone Manager + renderer + delivery ledger + WebSocket server) and the operator runs once (pair)
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
	"syscall"
	"time"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/delivery"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/logx"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/phone"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/render"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/session"
	"github.com/askdao/askdao-cloud-sms-bridge/internal/ws"
)

const usage = `usage:
  sms-bridge pair -cookies <file>   pair with the phone through Google account cookies, write the session file
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
	srv := ws.New(cfg.Token, mgr, ledger, log)
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
