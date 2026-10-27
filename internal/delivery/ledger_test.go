// [INPUT]: internal/delivery Ledger, a temp directory for the state file
// [OUTPUT]: tests: per-conversation (seq, part) de-duplication, cumulative acks, progress and held
//           messages surviving a restart, the replay window and the cap, an unreadable state file
// [POS]: internal/delivery's unit tests; no network, no phone
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

package delivery

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestAcceptDoneAndAck(t *testing.T) {
	l := New("", time.Minute, zerolog.Nop())
	if !l.Accept("A", Mark{Seq: 1, Part: 0}) || l.Accept("A", Mark{Seq: 1, Part: 0}) {
		t.Fatal("first frame should be new, its replay (still queued) not")
	}
	if !l.Accept("B", Mark{Seq: 1, Part: 0, Last: true}) {
		t.Fatal("conversations are independent")
	}
	if _, ok := l.Done("A", Mark{Seq: 1, Part: 0}); ok {
		t.Fatal("only a letter's last frame is acknowledged")
	}
	if !l.Accept("A", Mark{Seq: 1, Part: 1, Last: true}) {
		t.Fatal("next part should be new")
	}
	if n, ok := l.Done("A", Mark{Seq: 1, Part: 1, Last: true}); !ok || n != 1 {
		t.Fatalf("ack = %d %v, want 1", n, ok)
	}
	if l.Accept("A", Mark{Seq: 1, Part: 1, Last: true}) {
		t.Fatal("a handled frame is a replay")
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "delivery.json")
	a := New(path, time.Minute, zerolog.Nop())
	a.Accept("A", Mark{Seq: 4, Last: true})
	a.Done("A", Mark{Seq: 4, Last: true})
	a.Hold("M1", []byte(`{"id":"M1"}`))
	a.Hold("M2", []byte(`{"id":"M2"}`))
	a.Release("M1")
	b := New(path, time.Minute, zerolog.Nop())
	if b.Accept("A", Mark{Seq: 4, Last: true}) {
		t.Error("restart forgot the progress")
	}
	if n, ok := b.AckedUpTo("A"); !ok || n != 4 {
		t.Errorf("acked = %d %v", n, ok)
	}
	if p := b.Pending(); len(p) != 1 || string(p[0]) != `{"id":"M2"}` {
		t.Errorf("pending = %q", p)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode: %v %v", fi, err)
	}
}

func TestReplayWindowAndCap(t *testing.T) {
	now := time.Unix(0, 0)
	l := New("", 100*time.Second, zerolog.Nop())
	l.now = func() time.Time { return now }
	l.maxHeld = 2
	l.Hold("M1", []byte("1"))
	now = now.Add(50 * time.Second)
	l.Hold("M2", []byte("2"))
	l.Hold("M3", []byte("3"))
	if p := l.Pending(); len(p) != 2 || string(p[0]) != "2" {
		t.Errorf("cap: %q", p)
	}
	now = now.Add(200 * time.Second)
	if p := l.Pending(); len(p) != 0 {
		t.Errorf("window: %q", p)
	}
}

func TestUnreadableStateFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivery.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(path, time.Minute, zerolog.Nop())
	if _, ok := l.AckedUpTo("A"); ok || len(l.Pending()) != 0 {
		t.Error("should start empty")
	}
}
