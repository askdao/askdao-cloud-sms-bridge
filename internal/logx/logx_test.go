package logx

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
)

var utcLine = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z `)

func TestLinesStartWithUTCTimeAndSplitByLevel(t *testing.T) {
	var out, errOut bytes.Buffer
	log := New(&out, &errOut)
	log.Info().Str("conversation_id", "24").Msg("hello")
	log.Warn().Msg("careful")
	log.Debug().Msg("hidden")
	for _, line := range strings.Split(strings.TrimSpace(out.String()+errOut.String()), "\n") {
		if !utcLine.MatchString(line) {
			t.Errorf("line does not start with a UTC timestamp: %q", line)
		}
	}
	if !strings.Contains(out.String(), "hello") || strings.Contains(out.String(), "careful") {
		t.Errorf("info should go to out only: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "careful") || strings.Contains(errOut.String(), "hello") {
		t.Errorf("warn should go to errOut only: %q", errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), "hidden") {
		t.Error("debug lines should be dropped")
	}
	if !strings.Contains(out.String(), "conversation_id=24") {
		t.Errorf("own fields should be kept: %q", out.String())
	}
}

// libgm logs decrypted payloads and response bodies in fields; only allowlisted fields survive.
func TestLibraryDropsPayloadFields(t *testing.T) {
	var out, errOut bytes.Buffer
	log := Library(&out, &errOut)
	log.Warn().
		Str("evt_data", "zzPayload1").
		Str("decrypted_data", "zzPayload2").
		Bytes("resp_body", []byte("zzPayload3")).
		Str("data", "zzPayload4").
		Str("something_new", "zzPayload5").
		Int("status_code", 503).
		Err(errors.New("http 503 while polling")).
		Msg("Got unknown event type")
	log.Info().Str("data", "zzPayload6").Msg("Got settings event")
	all := out.String() + errOut.String()
	for _, p := range []string{"zzPayload1", "zzPayload2", "zzPayload3", "zzPayload4", "zzPayload5", "zzPayload6"} {
		if strings.Contains(all, p) {
			t.Errorf("library log kept a payload field (%s): %q", p, all)
		}
	}
	for _, want := range []string{"Got unknown event type", "status_code=503", "http 503 while polling", "component=libgm"} {
		if !strings.Contains(all, want) {
			t.Errorf("library log lost %q: %q", want, all)
		}
	}
	if !utcLine.MatchString(errOut.String()) {
		t.Errorf("library line does not start with a UTC timestamp: %q", errOut.String())
	}
}
