// [INPUT]: encoding/json, os (the state file, written to a temp file and renamed into place), sync, time
// [OUTPUT]: Ledger (New, Accept, Done, AckedUpTo, Hold, Release, Pending), Mark
// [POS]: internal/delivery, the bookkeeping behind the seq_ack feature; internal/ws calls it for every
//        marked outbound frame and every inbound message
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

// Package delivery keeps the seq_ack bookkeeping: which outbound frames each conversation has
// already handled (so frames conductor replays after a reconnect are not sent twice), and the
// inbound messages conductor has not yet confirmed (so they can be sent again when it reconnects).
// Both survive a restart in one JSON state file. Inbound messages older than the replay window
// are dropped: a reply that late would make no sense any more.
package delivery

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Mark is the delivery mark conductor puts on a frame: the letter's sequence number in its
// conversation, which frame of the letter this is (from 0), and whether it is the letter's last frame.
type Mark struct {
	Seq  int64 `json:"seq"`
	Part int64 `json:"part"`
	Last bool  `json:"last"`
}

func (m Mark) after(p *Mark) bool {
	return p == nil || m.Seq > p.Seq || (m.Seq == p.Seq && m.Part > p.Part)
}

type held struct {
	At    time.Time       `json:"at"`
	ID    string          `json:"id"`
	Frame json.RawMessage `json:"frame"`
}

type stateFile struct {
	Version  int              `json:"version"`
	Outbound map[string]*Mark `json:"outbound"`
	Inbound  []held           `json:"inbound"`
}

// Ledger is safe for concurrent use.
type Ledger struct {
	path     string // empty: memory only
	replay   time.Duration
	maxHeld  int
	now      func() time.Time
	log      zerolog.Logger
	mu       sync.Mutex
	done     map[string]*Mark // handled, saved
	accepted map[string]*Mark // seen (possibly still in the phone queue), memory only
	inbound  []held
}

// New loads path (a missing or unreadable file starts empty) and returns the ledger.
// An empty path keeps everything in memory.
func New(path string, replay time.Duration, log zerolog.Logger) *Ledger {
	l := &Ledger{
		path: path, replay: replay, maxHeld: 500, now: time.Now, log: log,
		done: map[string]*Mark{}, accepted: map[string]*Mark{},
	}
	if path == "" {
		return l
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l
	} else if err != nil {
		log.Error().Err(err).Msg("delivery state file unreadable; starting empty")
		return l
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		log.Error().Msg("delivery state file is not valid JSON; starting empty")
		return l
	}
	if st.Outbound != nil {
		l.done = st.Outbound
	}
	l.inbound = st.Inbound
	return l
}

// save writes the state file; callers hold l.mu.
func (l *Ledger) save() {
	if l.path == "" {
		return
	}
	b, _ := json.Marshal(stateFile{Version: 1, Outbound: l.done, Inbound: l.inbound})
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		l.log.Error().Err(err).Msg("delivery state file not written")
		return
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		l.log.Error().Err(err).Msg("delivery state file not written")
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		l.log.Error().Err(err).Msg("delivery state file not written")
	}
}

// Accept reports whether a marked frame for to is new (act on it) or a replay (skip it).
func (l *Ledger) Accept(to string, m Mark) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !m.after(l.accepted[to]) || !m.after(l.done[to]) {
		return false
	}
	mc := m
	l.accepted[to] = &mc
	return true
}

// Done records that a frame was handled (sent, or given up on). For a letter's last frame it
// returns the sequence number to acknowledge.
func (l *Ledger) Done(to string, m Mark) (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m.after(l.done[to]) {
		mc := m
		l.done[to] = &mc
		l.save()
	}
	if !m.Last {
		return 0, false
	}
	return l.ackedUpTo(to)
}

// AckedUpTo is the highest letter of to whose frames are all handled.
func (l *Ledger) AckedUpTo(to string) (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ackedUpTo(to)
}

func (l *Ledger) ackedUpTo(to string) (int64, bool) {
	p := l.done[to]
	if p == nil {
		return 0, false
	}
	n := p.Seq
	if !p.Last {
		n--
	}
	return n, n >= 0
}

// Hold keeps an inbound message frame until conductor confirms it.
func (l *Ledger) Hold(id string, frame []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inbound = append(l.inbound, held{At: l.now().UTC(), ID: id, Frame: append(json.RawMessage(nil), frame...)})
	if over := len(l.inbound) - l.maxHeld; over > 0 {
		l.inbound = append([]held(nil), l.inbound[over:]...)
	}
	l.save()
}

// Release forgets the held message id (conductor confirmed it).
func (l *Ledger) Release(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.inbound[:0]
	for _, h := range l.inbound {
		if h.ID != id {
			kept = append(kept, h)
		}
	}
	if len(kept) != len(l.inbound) {
		l.inbound = kept
		l.save()
	} else {
		l.inbound = kept
	}
}

// Pending returns the held frames still inside the replay window, oldest first, and drops the rest.
func (l *Ledger) Pending() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-l.replay)
	var kept []held
	var out [][]byte
	for _, h := range l.inbound {
		if !h.At.Before(cutoff) {
			kept = append(kept, h)
			out = append(out, h.Frame)
		}
	}
	if dropped := len(l.inbound) - len(kept); dropped > 0 {
		l.log.Info().Int("dropped", dropped).Msg("held inbound messages past the replay window dropped")
		l.inbound = kept
		l.save()
	}
	return out
}
