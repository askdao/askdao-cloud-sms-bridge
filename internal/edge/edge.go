// [INPUT]: net/http (an injectable *http.Client), encoding/json, crypto/rand, zerolog
// [OUTPUT]: Identity, ReadIdentity, Enroll, Health, Heartbeat (NewHeartbeat, Run, BeatOnce)
// [POS]: internal/edge, the bridge's half of conductor's edge nodes: a one-time enrollment code is traded
//        for a node token (kept in a 0600 identity file), then a heartbeat every minute declares the
//        capabilities and the health (ok / degraded). Conductor alerts when the heartbeats stop.
//        The token never reaches a log line
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

// Package edge registers the bridge as a conductor edge node and sends its heartbeats.
package edge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	defaultInterval = 60 * time.Second
	minInterval     = 10 * time.Second
	requestTimeout  = 10 * time.Second
)

// Identity is what enrollment returns and the identity file holds.
type Identity struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Token  string `json:"token"`
}

// ErrNoIdentity means the identity file does not exist: the bridge was never enrolled.
var ErrNoIdentity = errors.New("no identity file")

// ReadIdentity reads the identity file. A missing file gives ErrNoIdentity.
func ReadIdentity(path string) (Identity, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, ErrNoIdentity
	} else if err != nil {
		return Identity{}, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil || id.Token == "" || id.Name == "" {
		return Identity{}, errors.New("identity file is not a valid node identity")
	}
	return id, nil
}

func writeIdentity(path string, id Identity) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(id, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func endpoint(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + path
}

// Enroll trades a one-time code for a node token and writes the identity file.
func Enroll(ctx context.Context, client *http.Client, baseURL, code, path string) (Identity, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(baseURL, "/api/v1/edge/enroll"), bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("enroll request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("enroll failed: HTTP %d", resp.StatusCode)
	}
	var id Identity
	if err := json.NewDecoder(resp.Body).Decode(&id); err != nil || id.Token == "" {
		return Identity{}, errors.New("enroll: unexpected response body")
	}
	if err := writeIdentity(path, id); err != nil {
		return Identity{}, fmt.Errorf("writing the identity file: %w", err)
	}
	return id, nil
}

// Health is what one heartbeat reports: Status is "ok" or "degraded".
type Health struct {
	Status string
	Detail string
}

// Heartbeat sends the node's heartbeats.
type Heartbeat struct {
	client       *http.Client
	baseURL      string
	id           Identity
	capabilities []string
	health       func() Health
	version      string
	host         string
	log          zerolog.Logger
	instanceID   string

	mu       sync.Mutex
	interval time.Duration
	failures int
}

// NewHeartbeat returns a Heartbeat; health is called once per beat.
func NewHeartbeat(client *http.Client, baseURL string, id Identity, capabilities []string,
	health func() Health, version, host string, log zerolog.Logger) *Heartbeat {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return &Heartbeat{
		client: client, baseURL: baseURL, id: id, capabilities: capabilities, health: health,
		version: version, host: host, log: log, instanceID: "inst-" + hex.EncodeToString(b[:]),
		interval: defaultInterval,
	}
}

// Run beats until ctx ends or conductor no longer accepts the token (401).
func (h *Heartbeat) Run(ctx context.Context) {
	h.log.Info().Str("node", h.id.Name).Msg("edge heartbeat started")
	for {
		if !h.BeatOnce(ctx) {
			return
		}
		h.mu.Lock()
		wait := h.interval
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// BeatOnce sends one heartbeat. It returns false when the token was rejected (401): stop beating.
func (h *Heartbeat) BeatOnce(ctx context.Context) bool {
	health := h.health()
	payload := map[string]any{
		"instance_id":  h.instanceID,
		"capabilities": h.capabilities,
		"status":       health.Status,
	}
	if health.Detail != "" {
		payload["detail"] = health.Detail
	}
	if h.version != "" {
		payload["version"] = h.version
	}
	if h.host != "" {
		payload["host"] = h.host
	}
	body, _ := json.Marshal(payload)
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint(h.baseURL, "/api/v1/edge/heartbeat"), bytes.NewReader(body))
	if err != nil {
		h.warn("heartbeat request could not be built", 0)
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.id.Token)
	resp, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			h.warn("edge heartbeat failed: request error", 0)
		}
		return true
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		h.log.Error().Str("node", h.id.Name).Msg("edge node token rejected (401): heartbeat stopped; enroll this node again")
		return false
	case resp.StatusCode == http.StatusConflict:
		h.warn("another instance of this edge node is live (409); will retry", resp.StatusCode)
		return true
	case resp.StatusCode != http.StatusOK:
		h.warn("edge heartbeat failed", resp.StatusCode)
		return true
	}
	var out struct {
		HeartbeatIntervalS int `json:"heartbeat_interval_s"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) == nil {
		if d := time.Duration(out.HeartbeatIntervalS) * time.Second; d >= minInterval {
			h.mu.Lock()
			h.interval = d
			h.mu.Unlock()
		}
	}
	h.mu.Lock()
	recovered := h.failures > 0
	h.failures = 0
	h.mu.Unlock()
	if recovered {
		h.log.Info().Msg("edge heartbeat recovered")
	}
	return true
}

// warn logs the first failure and every tenth after it, so an outage does not flood the log.
func (h *Heartbeat) warn(msg string, status int) {
	h.mu.Lock()
	n := h.failures
	h.failures++
	h.mu.Unlock()
	if n%10 != 0 {
		return
	}
	ev := h.log.Warn()
	if status != 0 {
		ev = ev.Int("http_status", status)
	}
	ev.Msg(msg)
}
