// [INPUT]: github.com/rs/zerolog ConsoleWriter
// [OUTPUT]: New (the bridge's own logger), Library (the logger handed to libgm, allowlisted fields only)
// [POS]: internal/logx, the single place log lines are shaped; used by cmd/sms-bridge and passed into internal/phone
// [PROTOCOL]: Update this header on change, then check CLAUDE.md

// Package logx builds the bridge's loggers: one line per event, UTC timestamp first,
// info and below to one writer, warnings and errors to another (launchd's two log files).
package logx

import (
	"io"
	"time"

	"github.com/rs/zerolog"
)

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// libraryFields are the only fields kept from libgm's log events. libgm logs decrypted
// payloads and response bodies in fields such as data, evt_data, decrypted_data and
// resp_body; an allowlist keeps those and any future ones out of the logs.
var libraryFields = map[string]bool{
	zerolog.TimestampFieldName: true,
	zerolog.LevelFieldName:     true,
	zerolog.MessageFieldName:   true,
	zerolog.ErrorFieldName:     true,
	"component":                true,
	"listen_id":                true,
	"status_code":              true,
	"statusCode":               true,
	"sleep_seconds":            true,
	"skip_count":               true,
	"timeout_count":            true,
	"ping_id":                  true,
	"urgent":                   true,
	"count":                    true,
	"reason":                   true,
	"bugle_route":              true,
	"message_action":           true,
	"action_type":              true,
}

// New returns the bridge's own logger at info level.
func New(out, errOut io.Writer) zerolog.Logger {
	return newLogger(out, errOut, nil)
}

// Library returns the logger for libgm at info level: same lines as New, but every field
// outside the allowlist is dropped before formatting.
func Library(out, errOut io.Writer) zerolog.Logger {
	return newLogger(out, errOut, func(evt map[string]any) error {
		for k := range evt {
			if !libraryFields[k] {
				delete(evt, k)
			}
		}
		return nil
	}).With().Str("component", "libgm").Logger()
}

func newLogger(out, errOut io.Writer, prepare func(map[string]any) error) zerolog.Logger {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	console := func(w io.Writer) zerolog.ConsoleWriter {
		return zerolog.ConsoleWriter{
			Out:           w,
			NoColor:       true,
			TimeFormat:    timeFormat,
			TimeLocation:  time.UTC,
			FormatPrepare: prepare,
		}
	}
	w := splitWriter{low: console(out), high: console(errOut)}
	return zerolog.New(w).Level(zerolog.InfoLevel).With().Timestamp().Logger()
}

// splitWriter sends warnings and above to high, everything else to low.
type splitWriter struct {
	low, high io.Writer
}

func (s splitWriter) Write(p []byte) (int, error) { return s.low.Write(p) }

func (s splitWriter) WriteLevel(l zerolog.Level, p []byte) (int, error) {
	if l >= zerolog.WarnLevel && l != zerolog.NoLevel {
		return s.high.Write(p)
	}
	return s.low.Write(p)
}
