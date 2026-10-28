// [INPUT]: os environment, an optional .env file in the working directory
// [OUTPUT]: loadDotEnv, config, loadConfig, config.validateServe
// [POS]: cmd/sms-bridge's configuration reader; main.go calls loadDotEnv then loadConfig
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/askdao/askdao-cloud-sms-bridge/internal/render"
)

// loadDotEnv sets KEY=VALUE lines from path into the environment without overriding variables
// that are already set. Blank lines and lines starting with # are skipped; one pair of matching
// surrounding quotes is removed from a value. A missing file is not an error.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if !ok || key == "" {
			return fmt.Errorf("%s line %d: expected KEY=VALUE", path, n) // the line itself may hold a secret
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, val); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

type config struct {
	SessionFile string
	Port        string // WebSocket port conductor connects to
	Token       string // shared token conductor sends in its auth frame
	// read-only token for tests and watching; empty allows no observers
	ObserverToken string
	EdgeURL       string // conductor's API address for edge enrollment and heartbeats
	EdgeIdentity  string // the edge node identity file written by `sms-bridge enroll`
	ChromePath    string // the Google Chrome binary that renders send_image frames
	StateFile     string // the seq_ack delivery state (outbound progress, held inbound messages)
	// how long held inbound messages are sent again to a reconnecting conductor
	ReplayWindow time.Duration
}

func loadConfig() config {
	return config{
		SessionFile:   envOr("BRIDGE_SESSION_FILE", "session.json"),
		Port:          envOr("BRIDGE_PORT", "3003"),
		Token:         os.Getenv("BRIDGE_TOKEN"),
		ObserverToken: os.Getenv("BRIDGE_OBSERVER_TOKEN"),
		EdgeURL:       os.Getenv("EDGE_CONDUCTOR_URL"),
		EdgeIdentity:  envOr("EDGE_IDENTITY_FILE", "edge-node.json"),
		ChromePath:    envOr("BRIDGE_CHROME_PATH", render.DefaultChromePath),
		StateFile:     envOr("BRIDGE_STATE_FILE", "delivery.json"),
		// a value that is not a whole number of minutes falls back to 30 (validateServe reports it)
		ReplayWindow: time.Duration(envInt("BRIDGE_INBOUND_REPLAY_MINUTES", 30)) * time.Minute,
	}
}

// envInt reads a non-negative whole number; unset gives def, anything else gives -1.
func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// validateServe checks the settings serve needs; pair needs none of them.
func (c config) validateServe() error {
	if c.Token == "" {
		return errors.New("BRIDGE_TOKEN is empty; set it in .env (conductor sends the same token)")
	}
	if c.ObserverToken != "" && c.ObserverToken == c.Token {
		return errors.New("BRIDGE_OBSERVER_TOKEN must differ from BRIDGE_TOKEN")
	}
	if c.ReplayWindow < 0 {
		return errors.New("BRIDGE_INBOUND_REPLAY_MINUTES must be a whole number of minutes")
	}
	if p, err := strconv.Atoi(c.Port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("BRIDGE_PORT must be a port number, got %q", c.Port)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
