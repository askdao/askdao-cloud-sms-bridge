// [INPUT]: github.com/chromedp/chromedp (exec allocator, target listener) and cdproto fetch / emulation / page / network,
//          a local Google Chrome binary
// [OUTPUT]: Renderer (New, Render), CheckError, DefaultChromePath, DefaultWidth, MaxWidth
// [POS]: internal/render, draws a send_image frame's self-contained HTML to a full-page PNG with headless Chrome;
//        phone.Manager calls Render inside the conversation's outbound job, main.go builds the one Renderer
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

// Package render draws a self-contained HTML page to a PNG with the local Google Chrome.
package render

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// DefaultChromePath is where Google Chrome lives on a Mac.
const DefaultChromePath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

// Page widths in CSS pixels; one CSS pixel is one PNG pixel (device scale factor 1).
const (
	DefaultWidth = 1080
	MaxWidth     = 4096
)

const (
	renderTimeout = 30 * time.Second // the whole render: Chrome start, page, check, screenshot
	viewHeight    = 800              // the viewport's height; the screenshot covers the whole page anyway
	maxProblems   = 10               // __renderCheck problems kept in the error (they are page-authored)
	maxProblemLen = 200
)

// CheckError means the page's window.__renderCheck returned problems.
type CheckError struct {
	Problems []string
}

func (e *CheckError) Error() string {
	return "the page's check failed: " + strings.Join(e.Problems, "; ")
}

// Renderer renders one page at a time with a fresh Chrome each time.
type Renderer struct {
	chromePath string
	timeout    time.Duration
	tempRoot   string // where profile directories go; "" is the system's temporary directory

	mu sync.Mutex
}

// New returns a Renderer that runs the Chrome binary at chromePath.
func New(chromePath string) *Renderer {
	return &Renderer{chromePath: chromePath, timeout: renderTimeout}
}

// Render loads html in headless Chrome with a viewport width CSS pixels wide (device scale
// factor 1; 0 or less means DefaultWidth, more than MaxWidth means MaxWidth), waits for the
// page's fonts and its window.__renderCheck (when the page defines one), and returns a PNG of the
// whole page, exactly that many pixels wide. Every network request the page makes is blocked;
// data: URLs work. Calls run one at a time; each starts Chrome with a new temporary profile
// directory, which is removed, with Chrome stopped, before Render returns. One call takes at most
// 30 seconds, not counting the wait for an earlier call.
func (r *Renderer) Render(ctx context.Context, html string, width int) ([]byte, error) {
	if width <= 0 {
		width = DefaultWidth
	}
	width = min(width, MaxWidth)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := os.Stat(r.chromePath); err != nil {
		return nil, fmt.Errorf("no Chrome at %s: %w", r.chromePath, err)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	dir, err := os.MkdirTemp(r.tempRoot, "sms-bridge-chrome-")
	if err != nil {
		return nil, fmt.Errorf("creating Chrome's profile directory: %w", err)
	}
	defer os.RemoveAll(dir) // deferred first, so it runs after Chrome has exited

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(r.chromePath),
		chromedp.UserDataDir(dir),
		chromedp.WindowSize(width, viewHeight),
		// Requests the Fetch interception below does not see (WebSockets, Chrome's own traffic)
		// go to a proxy on a closed local port and fail.
		chromedp.ProxyServer("http://127.0.0.1:9"),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
	)
	actx, acancel := chromedp.NewExecAllocator(ctx, opts...)
	defer acancel() // stops Chrome and waits for the process to exit
	bctx, bcancel := chromedp.NewContext(actx)
	defer bcancel()

	chromedp.ListenTarget(bctx, func(ev any) {
		if e, ok := ev.(*fetch.EventRequestPaused); ok {
			go block(bctx, e)
		}
	})
	var png []byte
	// Navigating first lets chromedp track the top-level frame that the HTML replaces.
	err = chromedp.Run(bctx, chromedp.Navigate("about:blank"), chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		png, err = draw(ctx, html, width)
		return err
	}))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("rendering took longer than %s: %w", r.timeout, err)
		}
		return nil, err
	}
	return png, nil
}

// block fails a request the page made; Chrome does not pause data: URLs, but they may pass.
func block(ctx context.Context, e *fetch.EventRequestPaused) {
	var a chromedp.Action = fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient)
	if strings.HasPrefix(e.Request.URL, "data:") {
		a = fetch.ContinueRequest(e.RequestID)
	}
	_ = chromedp.Run(ctx, a) // fails only when the browser is already going away
}

// waitForPage resolves when the page has loaded and its fonts are ready, then with the problems
// window.__renderCheck reports (an empty list when the page defines no check).
const waitForPage = `(async () => {
  if (document.readyState !== "complete") {
    await new Promise((resolve) => window.addEventListener("load", resolve, { once: true }));
  }
  await document.fonts.ready;
  if (typeof window.__renderCheck !== "function") return [];
  const problems = await window.__renderCheck();
  return Array.isArray(problems) ? problems.map(String) : [];
})()`

func draw(ctx context.Context, html string, width int) ([]byte, error) {
	if err := fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*"}}).Do(ctx); err != nil {
		return nil, fmt.Errorf("blocking network requests: %w", err)
	}
	if err := emulation.SetDeviceMetricsOverride(int64(width), viewHeight, 1, false).Do(ctx); err != nil {
		return nil, fmt.Errorf("setting the viewport: %w", err)
	}
	tree, err := page.GetFrameTree().Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("finding the page: %w", err)
	}
	if err := page.SetDocumentContent(tree.Frame.ID, html).Do(ctx); err != nil {
		return nil, fmt.Errorf("loading the HTML: %w", err)
	}
	var problems []string
	awaitPromise := func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
	if err := chromedp.Evaluate(waitForPage, &problems, awaitPromise).Do(ctx); err != nil {
		return nil, fmt.Errorf("waiting for the page and its check: %w", err)
	}
	if len(problems) > 0 {
		return nil, &CheckError{Problems: trimProblems(problems)}
	}
	_, _, _, _, _, size, err := page.GetLayoutMetrics().Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("measuring the page: %w", err)
	}
	height := 1.0
	if size != nil {
		height = max(math.Ceil(size.Height), 1)
	}
	png, err := page.CaptureScreenshot().
		WithFormat(page.CaptureScreenshotFormatPng).
		WithCaptureBeyondViewport(true).
		WithFromSurface(true).
		WithClip(&page.Viewport{Width: float64(width), Height: height, Scale: 1}).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("taking the screenshot: %w", err)
	}
	return png, nil
}

func trimProblems(in []string) []string {
	out := make([]string, 0, min(len(in), maxProblems))
	for _, p := range in[:min(len(in), maxProblems)] {
		if len(p) > maxProblemLen {
			cut := maxProblemLen
			for cut > 0 && !utf8.RuneStart(p[cut]) {
				cut--
			}
			p = p[:cut] + "…"
		}
		out = append(out, p)
	}
	if len(in) > maxProblems {
		out = append(out, fmt.Sprintf("and %d more", len(in)-maxProblems))
	}
	return out
}
