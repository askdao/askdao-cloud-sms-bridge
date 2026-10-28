package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

const nodeToken = "edt_zzNodeTokenForTests"

func TestEnrollWritesIdentity(t *testing.T) {
	var got map[string]string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/edge/enroll" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"node_id":"edn_1","name":"macmini-sms","kind":"sms-bridge","token":"` + nodeToken + `","heartbeat_interval_s":60}`))
	}))
	defer hs.Close()
	path := filepath.Join(t.TempDir(), "state", "edge-node.json")
	id, err := Enroll(context.Background(), hs.Client(), hs.URL+"/", "edc_zzCode", path)
	if err != nil {
		t.Fatal(err)
	}
	if got["code"] != "edc_zzCode" || id.Name != "macmini-sms" || id.Token != nodeToken {
		t.Fatalf("request %v, identity %+v", got, id)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode: %v %v", info, err)
	}
	back, err := ReadIdentity(path)
	if err != nil || back != id {
		t.Fatalf("read back %+v %v", back, err)
	}
}

func TestEnrollRefused(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer hs.Close()
	path := filepath.Join(t.TempDir(), "edge-node.json")
	_, err := Enroll(context.Background(), hs.Client(), hs.URL, "edc_zzCode", path)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a refused enrollment must not write the identity file")
	}
}

func TestReadIdentity(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadIdentity(filepath.Join(dir, "absent.json")); err != ErrNoIdentity {
		t.Errorf("missing file: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"name":"x"}`), 0o600)
	if _, err := ReadIdentity(bad); err == nil || err == ErrNoIdentity {
		t.Errorf("file without a token: %v", err)
	}
}

type beatServer struct {
	mu     sync.Mutex
	status int
	bodies []map[string]any
	auths  []string
}

func (b *beatServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	b.bodies = append(b.bodies, m)
	b.auths = append(b.auths, r.Header.Get("Authorization"))
	if b.status != 0 && b.status != http.StatusOK {
		w.WriteHeader(b.status)
		return
	}
	_, _ = w.Write([]byte(`{"node_id":"edn_1","name":"macmini-sms","heartbeat_interval_s":30}`))
}

func newBeat(t *testing.T, bs *beatServer, health Health, logs *bytes.Buffer) *Heartbeat {
	hs := httptest.NewServer(bs)
	t.Cleanup(hs.Close)
	id := Identity{NodeID: "edn_1", Name: "macmini-sms", Kind: "sms-bridge", Token: nodeToken}
	return NewHeartbeat(hs.Client(), hs.URL, id, []string{"channel:sms"},
		func() Health { return health }, "abc123", "mini", zerolog.New(logs))
}

func TestBeatOnce(t *testing.T) {
	bs := &beatServer{}
	var logs bytes.Buffer
	hb := newBeat(t, bs, Health{Status: "degraded", Detail: "conductor not connected"}, &logs)
	if !hb.BeatOnce(context.Background()) {
		t.Fatal("a successful beat keeps going")
	}
	body := bs.bodies[0]
	if bs.auths[0] != "Bearer "+nodeToken || body["status"] != "degraded" || body["detail"] != "conductor not connected" ||
		body["version"] != "abc123" || body["host"] != "mini" || !strings.HasPrefix(body["instance_id"].(string), "inst-") {
		t.Fatalf("beat %v auth %q", body, bs.auths[0])
	}
	if hb.interval.Seconds() != 30 {
		t.Errorf("interval from conductor not taken: %v", hb.interval)
	}
	hb.BeatOnce(context.Background())
	if bs.bodies[1]["instance_id"] != body["instance_id"] {
		t.Error("the instance id must stay the same for the process")
	}
	if strings.Contains(logs.String(), nodeToken) {
		t.Error("logs contain the node token")
	}
}

func TestBeatOnceStatuses(t *testing.T) {
	for status, keepGoing := range map[int]bool{401: false, 409: true, 500: true} {
		bs := &beatServer{status: status}
		var logs bytes.Buffer
		hb := newBeat(t, bs, Health{Status: "ok"}, &logs)
		if got := hb.BeatOnce(context.Background()); got != keepGoing {
			t.Errorf("HTTP %d: keep going %v, want %v", status, got, keepGoing)
		}
		if strings.Contains(logs.String(), nodeToken) {
			t.Errorf("HTTP %d: logs contain the node token", status)
		}
	}
}

func TestFailuresLogOnceInTen(t *testing.T) {
	bs := &beatServer{status: 500}
	var logs bytes.Buffer
	hb := newBeat(t, bs, Health{Status: "ok"}, &logs)
	for range 11 {
		hb.BeatOnce(context.Background())
	}
	if n := strings.Count(logs.String(), "edge heartbeat failed"); n != 2 {
		t.Errorf("%d failure lines for 11 failures, want 2", n)
	}
	bs.mu.Lock()
	bs.status = 200
	bs.mu.Unlock()
	hb.BeatOnce(context.Background())
	if !strings.Contains(logs.String(), "edge heartbeat recovered") {
		t.Error("recovery not logged")
	}
}

func TestRunStopsOn401(t *testing.T) {
	bs := &beatServer{status: 401}
	var logs bytes.Buffer
	hb := newBeat(t, bs, Health{Status: "ok"}, &logs)
	hb.Run(context.Background()) // returns at once instead of beating forever
	if len(bs.bodies) != 1 {
		t.Errorf("%d beats after a 401", len(bs.bodies))
	}
}
