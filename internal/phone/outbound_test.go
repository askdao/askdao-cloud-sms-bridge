package phone

import (
	"strings"
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
