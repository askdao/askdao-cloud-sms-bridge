package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// chrome returns a Renderer for the Chrome at BRIDGE_CHROME_PATH (or the default path), with its
// profile directories in a fresh directory, and skips the test when there is no Chrome.
func chrome(t *testing.T) (*Renderer, string) {
	t.Helper()
	path := os.Getenv("BRIDGE_CHROME_PATH")
	if path == "" {
		path = DefaultChromePath
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no Chrome at %s", path)
	}
	r := New(path)
	r.tempRoot = t.TempDir()
	return r, r.tempRoot
}

// assertCleanedUp checks that the profile directory is gone and no Chrome process uses it.
func assertCleanedUp(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("profile directory left behind: %v", entries)
	}
	out, _ := exec.Command("pgrep", "-f", root).Output()
	if len(bytes.TrimSpace(out)) != 0 {
		t.Errorf("Chrome processes still running with the profile: %s", out)
	}
}

func decode(t *testing.T, b []byte) (w, h int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	return img.Bounds().Dx(), img.Bounds().Dy()
}

// A tiny red PNG as a data: URL: data: resources must load.
const redDot = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="

// The check is async and grows the page before it resolves: the screenshot must come after it.
const okPage = `<!doctype html>
<html><head><meta charset="utf-8"><style>
body { margin: 0; background: #101418; color: #e8e8e8; font: 24px sans-serif; }
.tall { height: 1500px; }
</style></head><body>
<div class="tall">Daily report <img id="dot" src="` + redDot + `" width="10" height="10"></div>
<script>
window.__renderCheck = async () => {
  await new Promise((r) => setTimeout(r, 100));
  const extra = document.createElement("div");
  extra.style.height = "300px";
  document.body.appendChild(extra);
  const dot = document.getElementById("dot");
  return dot.complete && dot.naturalWidth === 1 ? [] : ["data: image did not load"];
};
</script></body></html>`

func TestRenderFullPageAtWidth(t *testing.T) {
	r, root := chrome(t)
	start := time.Now()
	b, err := r.Render(context.Background(), okPage, 400)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rendered in %s, %d bytes", time.Since(start), len(b))
	w, h := decode(t, b)
	if w != 400 {
		t.Errorf("width %d, want 400", w)
	}
	if h != 1800 {
		t.Errorf("height %d, want 1800 (the whole page, including what the check added)", h)
	}
	assertCleanedUp(t, root)
}

func TestRenderCheckProblemsFail(t *testing.T) {
	r, root := chrome(t)
	page := `<!doctype html><body>x<script>
window.__renderCheck = () => Promise.resolve(["font missing: Example Sans", "` + strings.Repeat("y", 300) + `"]);
</script></body>`
	_, err := r.Render(context.Background(), page, 320)
	var ce *CheckError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want a CheckError", err)
	}
	if len(ce.Problems) != 2 || ce.Problems[0] != "font missing: Example Sans" || len(ce.Problems[1]) > maxProblemLen+len("…") {
		t.Errorf("problems: %q", ce.Problems)
	}
	assertCleanedUp(t, root)
}

func TestRenderCheckThatThrowsFails(t *testing.T) {
	r, _ := chrome(t)
	page := `<!doctype html><body>x<script>window.__renderCheck = async () => { throw new Error("boom"); };</script></body>`
	if _, err := r.Render(context.Background(), page, 320); err == nil {
		t.Fatal("a check that throws must fail the render")
	}
}

// A page without __renderCheck renders; width 0 means the default width.
func TestRenderWithoutCheckAtDefaultWidth(t *testing.T) {
	r, _ := chrome(t)
	b, err := r.Render(context.Background(), `<!doctype html><body style="margin:0"><div style="height:50px">x</div></body>`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := decode(t, b); w != DefaultWidth {
		t.Errorf("width %d, want %d", w, DefaultWidth)
	}
}

// Content wider than the viewport does not make the PNG wider.
func TestRenderClipsToWidth(t *testing.T) {
	r, _ := chrome(t)
	b, err := r.Render(context.Background(), `<!doctype html><body style="margin:0"><div style="width:2000px;height:100px">wide</div></body>`, 500)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decode(t, b); w != 500 || h < 100 {
		t.Errorf("size %dx%d, want 500 wide", w, h)
	}
}

// Nothing reaches the network: subresources, fetch(), beacons and WebSockets are all blocked.
func TestRenderBlocksNetwork(t *testing.T) {
	r, root := chrome(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	u := srv.URL
	ws := "ws" + strings.TrimPrefix(u, "http")
	page := fmt.Sprintf(`<!doctype html><html><head>
<link rel="stylesheet" href="%[1]s/style.css">
<style>@font-face { font-family: Remote; src: url(%[1]s/font.woff2); } body { font-family: Remote, sans-serif; }</style>
</head><body>
<img src="%[1]s/image.png"><iframe src="%[1]s/frame.html"></iframe>
<script src="%[1]s/script.js"></script>
<script>
window.__renderCheck = async () => {
  const failed = [];
  try { await fetch("%[1]s/fetch"); failed.push("fetch went through"); } catch (e) {}
  navigator.sendBeacon("%[1]s/beacon", "x");
  await new Promise((resolve) => {
    const s = new WebSocket("%[2]s/socket");
    s.onopen = () => { failed.push("websocket opened"); resolve(); };
    s.onerror = () => resolve();
    setTimeout(resolve, 3000);
  });
  return failed;
};
</script></body></html>`, u, ws)
	if _, err := r.Render(context.Background(), page, 320); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // a request racing the end of the render would show up by now
	if n := hits.Load(); n != 0 {
		t.Errorf("the page reached the network %d times", n)
	}
	assertCleanedUp(t, root)
}

func TestRenderTimeoutStopsChrome(t *testing.T) {
	r, root := chrome(t)
	r.timeout = 3 * time.Second
	start := time.Now()
	_, err := r.Render(context.Background(), `<!doctype html><body><script>window.__renderCheck = () => new Promise(() => {});</script></body>`, 320)
	if err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("got %v, want a timeout", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s", d)
	}
	assertCleanedUp(t, root)
}

// Each render has its own profile directory under the root; there is never more than one.
func TestRenderOneAtATime(t *testing.T) {
	r, root := chrome(t)
	page := `<!doctype html><body><script>window.__renderCheck = () => new Promise((r) => setTimeout(() => r([]), 300));</script></body>`
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := r.Render(context.Background(), page, 320)
			done <- err
		}()
	}
	most := 0
	deadline := time.After(30 * time.Second)
	for finished := 0; finished < 2; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			finished++
		case <-deadline:
			t.Fatal("renders did not finish")
		case <-time.After(5 * time.Millisecond):
			entries, _ := os.ReadDir(root)
			most = max(most, len(entries))
		}
	}
	if most != 1 {
		t.Errorf("%d profile directories at once, want 1", most)
	}
}

func TestRenderWithoutChromeFails(t *testing.T) {
	r := New("/nonexistent/Google Chrome")
	if _, err := r.Render(context.Background(), "<p>x</p>", 320); err == nil || !strings.Contains(err.Error(), "no Chrome") {
		t.Errorf("got %v", err)
	}
}
