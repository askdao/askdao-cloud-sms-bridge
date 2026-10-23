package phone

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func sentTexts(c *fakeClient) []string {
	var out []string
	c.get(func(f *fakeClient) {
		for _, r := range f.sent {
			out = append(out, r.GetMessagePayload().GetMessageInfo()[0].GetMessageContent().GetContent())
		}
	})
	return out
}

func sentCount(c *fakeClient) int { return len(sentTexts(c)) }

func lastSent(c *fakeClient) *gmproto.SendMessageRequest {
	var r *gmproto.SendMessageRequest
	c.get(func(f *fakeClient) { r = f.sent[len(f.sent)-1] })
	return r
}

// echo emits the phone's echo of a sent request, as libgm delivers it.
func echo(c *fakeClient, req *gmproto.SendMessageRequest, id string) {
	c.emit(&libgm.WrappedMessage{Message: &gmproto.Message{
		MessageID: id, ConversationID: req.GetConversationID(), TmpID: req.GetTmpID(), ParticipantID: "1",
		Timestamp: time.Now().UnixMicro(), MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_OUTGOING_COMPLETE},
	}})
}

func TestSendGoesOutInOrderPerConversation(t *testing.T) {
	h, c, sink := readyManager(t)
	h.mgr.echoWait = 5 * time.Second
	c.emit(groupConv("25", gmproto.ConversationType_SMS))

	h.mgr.Send(groupID, "zzBody1")
	h.mgr.Send(groupID, "zzBody2")
	h.mgr.Send(groupID, "zzBody3")
	eventually(t, "first send", func() bool { return sentCount(c) == 1 })
	h.mgr.Send("25", "zzBodyOther") // another conversation does not wait
	eventually(t, "other conversation", func() bool { return sentCount(c) == 2 })
	time.Sleep(30 * time.Millisecond)
	if got := sentTexts(c); got[0] != "zzBody1" || got[1] != "zzBodyOther" {
		t.Fatalf("sent %v: the second message must wait for the first one's echo", got)
	}
	var first *gmproto.SendMessageRequest
	c.get(func(f *fakeClient) { first = f.sent[0] })
	echo(c, first, "1001")
	eventually(t, "second send", func() bool { return sentCount(c) == 3 })
	echo(c, lastSent(c), "1002")
	eventually(t, "third send", func() bool { return sentCount(c) == 4 })
	echo(c, lastSent(c), "1003")
	if got := sentTexts(c); strings.Join(got, ",") != "zzBody1,zzBodyOther,zzBody2,zzBody3" {
		t.Errorf("order: %v", got)
	}
	settle(t, c, sink)
	if got := nonMarkers(sink); len(got) != 0 {
		t.Errorf("own echoes were forwarded: %+v", got)
	}
	assertNoText(t, h)
}

func TestSendNextAfterEchoTimeoutOrFailure(t *testing.T) {
	h, c, _ := readyManager(t)
	h.mgr.echoWait = 20 * time.Millisecond
	c.get(func(f *fakeClient) { f.sendStatus = gmproto.SendMessageResponse_FAILURE_3 })
	h.mgr.Send(groupID, "zzBody1") // refused by the phone
	eventually(t, "first send", func() bool { return sentCount(c) == 1 })
	c.get(func(f *fakeClient) { f.sendStatus = gmproto.SendMessageResponse_SUCCESS })
	h.mgr.Send(groupID, "zzBody2") // never echoed
	h.mgr.Send(groupID, "zzBody3")
	eventually(t, "all sent", func() bool { return sentCount(c) == 3 })
	if got := strings.Join(sentTexts(c), ","); got != "zzBody1,zzBody2,zzBody3" {
		t.Errorf("order: %s", got)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "FAILURE_3") || !strings.Contains(logs, "has not echoed") {
		t.Errorf("failure and missing echo should be logged: %s", logs)
	}
	assertNoText(t, h)
}

func TestSendRequestShape(t *testing.T) {
	h, c, _ := readyManager(t)
	c.get(func(f *fakeClient) { f.autoEcho = true })
	h.mgr.Send(groupID, "zzBody1")
	eventually(t, "send", func() bool { return sentCount(c) == 1 })
	r := lastSent(c)
	p := r.GetMessagePayload()
	if r.GetConversationID() != groupID || p.GetConversationID() != groupID {
		t.Errorf("conversation: %q / %q", r.GetConversationID(), p.GetConversationID())
	}
	if r.GetTmpID() == "" || p.GetTmpID() != r.GetTmpID() || p.GetTmpID2() != r.GetTmpID() {
		t.Errorf("tmp ids differ: %q %q %q", r.GetTmpID(), p.GetTmpID(), p.GetTmpID2())
	}
	if p.GetParticipantID() != "1" {
		t.Errorf("participant = %q, want the conversation's default outgoing id", p.GetParticipantID())
	}
	if r.GetSIMPayload().GetSIMNumber() != 1 {
		t.Errorf("SIM payload not set: %v", r.GetSIMPayload())
	}
	if r.GetReply() != nil {
		t.Error("no quoted replies on this channel")
	}
}

func TestSendToNumberUsesTheOneToOneConversation(t *testing.T) {
	h, c, _ := readyManager(t)
	c.get(func(f *fakeClient) {
		f.autoEcho = true
		f.byNumber["+15550000003"] = &gmproto.Conversation{ConversationID: "41", DefaultOutgoingID: "1", Type: gmproto.ConversationType_SMS}
	})
	h.mgr.Send("+15550000003", "zzBody1")
	eventually(t, "send", func() bool { return sentCount(c) == 1 })
	var req *gmproto.GetOrCreateConversationRequest
	c.get(func(f *fakeClient) { req = f.createReqs[0] })
	n := req.GetNumbers()[0]
	if len(req.GetNumbers()) != 1 || n.GetNumber() != "+15550000003" || n.GetNumber2() != "+15550000003" || n.GetMysteriousInt() != 2 {
		t.Errorf("lookup request: %v", req)
	}
	if got := lastSent(c).GetConversationID(); got != "41" {
		t.Errorf("sent to conversation %q, want 41", got)
	}
}

func TestSendToUnusableTargetSendsNothing(t *testing.T) {
	h, c, _ := readyManager(t)
	h.mgr.Send("someone", "zzBody1")
	h.mgr.Send("24a", "zzBody2")
	h.mgr.Send("+15550000004", "zzBody3") // the phone has no conversation for it
	eventually(t, "three failures logged", func() bool { return strings.Count(h.logs.String(), "send failed") == 3 })
	if sentCount(c) != 0 {
		t.Errorf("sent %v", sentTexts(c))
	}
	assertNoText(t, h)
}

func TestReactLikeInRCSAndSMSConversations(t *testing.T) {
	h, c, _ := readyManager(t)
	c.emit(groupConv("25", gmproto.ConversationType_SMS)) // an MMS group is SMS-typed
	h.mgr.React(groupID, "500", "like")
	h.mgr.React("25", "501", "like")
	h.mgr.React(groupID, "502", "love") // not supported: ignored
	eventually(t, "two reactions", func() bool {
		n := 0
		c.get(func(f *fakeClient) { n = len(f.reactions) })
		return n == 2
	})
	time.Sleep(20 * time.Millisecond)
	c.get(func(f *fakeClient) {
		if len(f.reactions) != 2 {
			t.Fatalf("reactions: %d", len(f.reactions))
		}
		ids := map[string]bool{}
		for i, r := range f.reactions { // two conversations: no order between them
			ids[r.GetMessageID()] = true
			if r.GetAction() != gmproto.SendReactionRequest_ADD {
				t.Errorf("reaction %d: %v", i, r)
			}
			if r.GetReactionData().GetUnicode() != "👍" || r.GetReactionData().GetType() != gmproto.EmojiType_LIKE {
				t.Errorf("reaction %d is not 👍: %v", i, r.GetReactionData())
			}
			if r.GetSIMPayload().GetSIMNumber() != 1 {
				t.Errorf("reaction %d has no SIM payload", i)
			}
		}
		if !ids["500"] || !ids["501"] {
			t.Errorf("reacted to %v, want 500 (RCS group) and 501 (MMS group)", ids)
		}
	})
	if !strings.Contains(h.logs.String(), "only like is supported") {
		t.Error("the ignored reaction should be logged")
	}
}

// The 👍 shares the conversation's queue, so it never overtakes an earlier message.
func TestReactWaitsBehindEarlierSend(t *testing.T) {
	h, c, _ := readyManager(t)
	h.mgr.echoWait = 5 * time.Second
	h.mgr.Send(groupID, "zzBody1")
	h.mgr.React(groupID, "500", "like")
	eventually(t, "send", func() bool { return sentCount(c) == 1 })
	time.Sleep(30 * time.Millisecond)
	c.get(func(f *fakeClient) {
		if len(f.reactions) != 0 {
			t.Fatal("the reaction overtook the message")
		}
	})
	echo(c, lastSent(c), "1001")
	eventually(t, "reaction", func() bool {
		n := 0
		c.get(func(f *fakeClient) { n = len(f.reactions) })
		return n == 1
	})
}

// fakeRenderer stands in for render.Renderer.Render.
type fakeRenderer struct {
	mu    sync.Mutex
	calls []string      // "width|html"
	gate  chan struct{} // when set, render waits for it
	png   []byte
	err   error
}

func (r *fakeRenderer) render(ctx context.Context, html string, width int) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, fmt.Sprintf("%d|%s", width, html))
	gate := r.gate
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.png, r.err
}

func (r *fakeRenderer) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

var fakePNG = []byte("\x89PNG\r\n\x1a\nzzPixels")

func imageManager(t *testing.T) (*harness, *fakeClient, *fakeRenderer) {
	t.Helper()
	h, c, _ := readyManager(t)
	r := &fakeRenderer{png: fakePNG}
	h.mgr.render = r.render
	return h, c, r
}

func report() Image {
	return Image{
		HTML: "<!doctype html><p>zzBodyHTML</p>", Width: 1080, Filename: "report.png",
		Caption: "zzBodyCaption", Fallback: []string{"zzBodyFallback1", "", "zzBodyFallback2"},
	}
}

// sentParts describes every sent request: "image:<media id>" for a media part, the text otherwise.
func sentParts(c *fakeClient) []string {
	var out []string
	c.get(func(f *fakeClient) {
		for _, r := range f.sent {
			var parts []string
			for _, p := range r.GetMessagePayload().GetMessageInfo() {
				if m := p.GetMediaContent(); m != nil {
					parts = append(parts, "image:"+m.GetMediaID())
				} else {
					parts = append(parts, p.GetMessageContent().GetContent())
				}
			}
			out = append(out, strings.Join(parts, "+"))
		}
	})
	return out
}

func isMedia(req *gmproto.SendMessageRequest) bool {
	return req.GetMessagePayload().GetMessageInfo()[0].GetMediaContent() != nil
}

func assertImageLog(t *testing.T, h *harness, want ...string) {
	t.Helper()
	eventually(t, "send_image summary", func() bool { return strings.Contains(h.logs.String(), `send_image: `) })
	logs := h.logs.String()
	for _, w := range want {
		if !strings.Contains(logs, w) {
			t.Errorf("logs lack %s:\n%s", w, logs)
		}
	}
	if strings.Count(logs, `send_image: `) != 1 {
		t.Errorf("want exactly one summary line:\n%s", logs)
	}
	if strings.Contains(logs, "zzKeyval999") || strings.Contains(logs, "zzPixels") {
		t.Error("logs contain the media key or the image")
	}
	assertNoText(t, h)
}

func TestSendImageSendsMediaThenCaption(t *testing.T) {
	h, c, r := imageManager(t)
	c.get(func(f *fakeClient) { f.autoEcho = true })
	h.mgr.SendImage(groupID, report())
	eventually(t, "image and caption", func() bool { return sentCount(c) == 2 })
	if got := strings.Join(sentParts(c), ","); got != "image:media-1,zzBodyCaption" {
		t.Fatalf("sent %s, want the image then the caption", got)
	}
	r.mu.Lock()
	if r.calls[0] != "1080|<!doctype html><p>zzBodyHTML</p>" {
		t.Errorf("render call %q", r.calls[0])
	}
	r.mu.Unlock()
	c.get(func(f *fakeClient) {
		if len(f.uploads) != 1 || string(f.uploads[0].data) != string(fakePNG) || f.uploads[0].name != "report.png" || f.uploads[0].mime != "image/png" {
			t.Errorf("uploads: %+v", f.uploads)
		}
		req := f.sent[0]
		p := req.GetMessagePayload()
		if len(p.GetMessageInfo()) != 1 {
			t.Errorf("the media message has %d parts, want 1 (the caption goes separately)", len(p.GetMessageInfo()))
		}
		m := p.GetMessageInfo()[0].GetMediaContent()
		if m.GetMediaID() != "media-1" || string(m.GetDecryptionKey()) != "zzKeyval999" || m.GetMimeType() != "image/png" {
			t.Errorf("media part: %v", m)
		}
		if req.GetConversationID() != groupID || p.GetConversationID() != groupID || p.GetParticipantID() != "1" {
			t.Errorf("addressing: %v", req)
		}
		if req.GetTmpID() == "" || p.GetTmpID() != req.GetTmpID() || p.GetTmpID2() != req.GetTmpID() || req.GetSIMPayload().GetSIMNumber() != 1 {
			t.Errorf("tmp ids or SIM: %v", req)
		}
	})
	assertImageLog(t, h, `"outcome":"image"`, `"render_ms":`, fmt.Sprintf(`"png_bytes":%d`, len(fakePNG)), `"caption":"sent"`)
}

func TestSendImageWithoutCaptionOrFilename(t *testing.T) {
	h, c, _ := imageManager(t)
	c.get(func(f *fakeClient) { f.autoEcho = true })
	img := report()
	img.Caption, img.Filename = "", ""
	h.mgr.SendImage(groupID, img)
	assertImageLog(t, h, `"outcome":"image"`, `"caption":"none"`)
	if got := strings.Join(sentParts(c), ","); got != "image:media-1" {
		t.Errorf("sent %s", got)
	}
	c.get(func(f *fakeClient) {
		if f.uploads[0].name != defaultImageName {
			t.Errorf("file name %q", f.uploads[0].name)
		}
	})
}

// Anything failing before the phone accepted the image: the fallback texts go instead, in order.
func TestSendImageFailuresSendFallback(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(c *fakeClient, r *fakeRenderer, img *Image)
		sent    string // what reaches SendMessage
		uploads int
		reason  string
	}{
		{"render fails", func(_ *fakeClient, r *fakeRenderer, _ *Image) {
			r.err = errors.New("the page's check failed: font missing")
		},
			"zzBodyFallback1,zzBodyFallback2", 0, "font missing"},
		{"upload fails", func(c *fakeClient, _ *fakeRenderer, _ *Image) { c.uploadErr = errors.New("unexpected status code 500") },
			"zzBodyFallback1,zzBodyFallback2", 1, "uploading"},
		{"phone refuses the image", func(c *fakeClient, _ *fakeRenderer, _ *Image) {
			c.sendResult = func(req *gmproto.SendMessageRequest) (gmproto.SendMessageResponse_Status, error) {
				if isMedia(req) {
					return gmproto.SendMessageResponse_FAILURE_3, nil
				}
				return gmproto.SendMessageResponse_SUCCESS, nil
			}
		}, "image:media-1,zzBodyFallback1,zzBodyFallback2", 1, "FAILURE_3"},
		{"sending the image errors", func(c *fakeClient, _ *fakeRenderer, _ *Image) {
			c.sendResult = func(req *gmproto.SendMessageRequest) (gmproto.SendMessageResponse_Status, error) {
				if isMedia(req) {
					return 0, errors.New("phone not responding")
				}
				return gmproto.SendMessageResponse_SUCCESS, nil
			}
		}, "image:media-1,zzBodyFallback1,zzBodyFallback2", 1, "phone not responding"},
		{"no html", func(_ *fakeClient, _ *fakeRenderer, img *Image) { img.HTML = "" },
			"zzBodyFallback1,zzBodyFallback2", 0, "no html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, c, r := imageManager(t)
			img := report()
			c.get(func(f *fakeClient) {
				f.autoEcho = true
				tc.setup(f, r, &img)
			})
			h.mgr.SendImage(groupID, img)
			assertImageLog(t, h, `"outcome":"fallback"`, `"fallback_sent":2`, `"fallback_texts":3`, tc.reason)
			if got := strings.Join(sentParts(c), ","); got != tc.sent {
				t.Errorf("sent %s, want %s (no caption)", got, tc.sent)
			}
			c.get(func(f *fakeClient) {
				if len(f.uploads) != tc.uploads {
					t.Errorf("%d uploads, want %d", len(f.uploads), tc.uploads)
				}
			})
			if img.HTML == "" && r.callCount() != 0 {
				t.Error("rendered a frame without html")
			}
		})
	}
}

// The image went out; the caption failing is only logged: a fallback would repeat the report.
func TestSendImageCaptionFailureSendsNoFallback(t *testing.T) {
	h, c, _ := imageManager(t)
	c.get(func(f *fakeClient) {
		f.autoEcho = true
		f.sendResult = func(req *gmproto.SendMessageRequest) (gmproto.SendMessageResponse_Status, error) {
			if isMedia(req) {
				return gmproto.SendMessageResponse_SUCCESS, nil
			}
			return gmproto.SendMessageResponse_FAILURE_2, nil
		}
	})
	h.mgr.SendImage(groupID, report())
	assertImageLog(t, h, `"outcome":"image"`, `"caption":"failed"`, "FAILURE_2")
	if got := strings.Join(sentParts(c), ","); got != "image:media-1,zzBodyCaption" {
		t.Errorf("sent %s, want the image and the caption only", got)
	}
}

// The frame is one job in the conversation's queue: it waits for an earlier send, nothing else
// gets between the image and its caption (not even during the render), the caption waits for
// the image's echo, and a later send waits for the caption.
func TestSendImageKeepsItsPlaceInTheQueue(t *testing.T) {
	h, c, r := imageManager(t)
	h.mgr.echoWait = 5 * time.Second
	r.gate = make(chan struct{})
	h.mgr.Send(groupID, "zzBody0")
	h.mgr.SendImage(groupID, report())
	h.mgr.Send(groupID, "zzBody9")
	eventually(t, "first send", func() bool { return sentCount(c) == 1 })
	time.Sleep(30 * time.Millisecond)
	if r.callCount() != 0 {
		t.Fatal("the render started before the earlier message was echoed")
	}
	echo(c, lastSent(c), "1001")
	eventually(t, "render started", func() bool { return r.callCount() == 1 })
	time.Sleep(30 * time.Millisecond)
	if n := sentCount(c); n != 1 {
		t.Fatalf("%d sent during the render: %v", n, sentParts(c))
	}
	close(r.gate)
	eventually(t, "image sent", func() bool { return sentCount(c) == 2 })
	time.Sleep(30 * time.Millisecond)
	if n := sentCount(c); n != 2 {
		t.Fatalf("the caption overtook the image's echo: %v", sentParts(c))
	}
	echo(c, lastSent(c), "1002")
	eventually(t, "caption sent", func() bool { return sentCount(c) == 3 })
	time.Sleep(30 * time.Millisecond)
	if n := sentCount(c); n != 3 {
		t.Fatalf("the later send overtook the caption: %v", sentParts(c))
	}
	echo(c, lastSent(c), "1003")
	eventually(t, "later send", func() bool { return sentCount(c) == 4 })
	if got := strings.Join(sentParts(c), ","); got != "zzBody0,image:media-1,zzBodyCaption,zzBody9" {
		t.Errorf("order: %s", got)
	}
}

// Fallback texts are sent inside the same job: a send queued during the render stays behind them.
func TestSendImageFallbackKeepsItsPlaceInTheQueue(t *testing.T) {
	h, c, r := imageManager(t)
	c.get(func(f *fakeClient) { f.autoEcho = true })
	r.gate = make(chan struct{})
	r.err = errors.New("the page's check failed")
	h.mgr.SendImage(groupID, report())
	eventually(t, "render started", func() bool { return r.callCount() == 1 })
	h.mgr.Send(groupID, "zzBody9")
	close(r.gate)
	eventually(t, "all sent", func() bool { return sentCount(c) == 3 })
	if got := strings.Join(sentParts(c), ","); got != "zzBodyFallback1,zzBodyFallback2,zzBody9" {
		t.Errorf("order: %s", got)
	}
}
