package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/runlimits"
)

func TestVersionIsOne(t *testing.T) {
	// The frontend contract (docs/PROTOCOL.md) pins protocol version 1.
	assert.Equal(t, 1, protocol.Version)
}

func TestConstructorsStampTypeAndVersion(t *testing.T) {
	t.Run("hello_ok", func(t *testing.T) {
		m := protocol.NewHelloOK("p1")
		assert.Equal(t, protocol.TypeHelloOK, m.Type)
		assert.Equal(t, "p1", m.PlayerID)
		assert.Equal(t, protocol.Version, m.ServerVersion)
	})

	t.Run("error", func(t *testing.T) {
		m := protocol.NewError(protocol.CodeBadMessage, "boom")
		assert.Equal(t, protocol.TypeError, m.Type)
		assert.Equal(t, protocol.CodeBadMessage, m.Code)
		assert.Equal(t, "boom", m.Message)
	})

	t.Run("ntp_pong", func(t *testing.T) {
		m := protocol.NewNTPPong(1, 2, 3)
		assert.Equal(t, protocol.TypeNTPPong, m.Type)
		assert.EqualValues(t, 1, m.T0)
		assert.EqualValues(t, 2, m.T1)
		assert.EqualValues(t, 3, m.T2)
	})
}

// TestOutboundJSONShape asserts the exact JSON keys the frontend will parse.
func TestOutboundJSONShape(t *testing.T) {
	tests := []struct {
		name    string
		msg     any
		wantSub []string
	}{
		{
			name:    "hello_ok",
			msg:     protocol.NewHelloOK("abc"),
			wantSub: []string{`"type":"hello_ok"`, `"playerId":"abc"`, `"serverVersion":1`},
		},
		{
			name:    "error",
			msg:     protocol.NewError(protocol.CodeVersionMismatch, "nope"),
			wantSub: []string{`"type":"error"`, `"code":"version_mismatch"`, `"message":"nope"`},
		},
		{
			name:    "ntp_pong",
			msg:     protocol.NewNTPPong(10, 20, 30),
			wantSub: []string{`"type":"ntp_pong"`, `"t0":10`, `"t1":20`, `"t2":30`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.msg)
			require.NoError(t, err)
			got := string(b)
			for _, sub := range tc.wantSub {
				assert.Contains(t, got, sub, "marshaled = %s", got)
			}
		})
	}
}

// TestEnvelopeThenConcreteDecode exercises the documented two-step decode: read
// the discriminator, then the concrete payload.
func TestEnvelopeThenConcreteDecode(t *testing.T) {
	raw := []byte(`{"type":"hello","protocolVersion":1,"nick":"neo"}`)

	var env protocol.Envelope
	require.NoError(t, json.Unmarshal(raw, &env))
	require.Equal(t, protocol.TypeHello, env.Type)

	var h protocol.Hello
	require.NoError(t, json.Unmarshal(raw, &h))
	assert.Equal(t, 1, h.ProtocolVersion)
	assert.Equal(t, "neo", h.Nick)
}

// TestEventBatchKeepsEventsOpaque confirms events survive a decode/encode round
// trip untouched (the server does not interpret them this phase).
func TestEventBatchKeepsEventsOpaque(t *testing.T) {
	raw := []byte(`{"type":"event_batch","matchId":"m1","playerId":"p1","version":1,"events":[{"k":"insert","seq":1},{"k":"commit"}]}`)

	var eb protocol.EventBatch
	require.NoError(t, json.Unmarshal(raw, &eb))
	require.Len(t, eb.Events, 2)
	assert.JSONEq(t, `{"k":"insert","seq":1}`, string(eb.Events[0]))
	assert.JSONEq(t, `{"k":"commit"}`, string(eb.Events[1]))
}

func TestValidNick(t *testing.T) {
	tests := []struct {
		name string
		nick string
		want bool
	}{
		{"empty", "", false},
		{"single", "a", true},
		{"max 16 ascii", strings.Repeat("a", 16), true},
		{"over 16 ascii", strings.Repeat("a", 17), false},
		{"counts runes not bytes", strings.Repeat("é", 16), true}, // 32 bytes, 16 runes
		{"over by runes", strings.Repeat("é", 17), false},
		{"multibyte within bound", "Егор", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, protocol.ValidNick(tc.nick))
		})
	}
}

// TestValidateSettings covers the two text paths a room can be configured for.
// They validate DIFFERENT things and neither may be accepted in the other's
// shape: a seeded room without a dictionary fingerprint and a quote room
// without an id are equally unplayable, and accepting either here would only
// move the failure to every client at countdown.
func TestValidateSettings(t *testing.T) {
	seeded := func(mutate func(*protocol.Settings)) protocol.Settings {
		s := protocol.Settings{
			Name:       "room",
			Visibility: protocol.VisibilityPrivate,
			Mode:       protocol.ModeWords,
			WordCount:  25,
			Lang:       "english",
			DictHash:   "e2e00001",
			TextSource: protocol.TextSource{Kind: protocol.TextSourceSeeded},
		}
		if mutate != nil {
			mutate(&s)
		}
		return s
	}
	quote := func(mutate func(*protocol.Settings)) protocol.Settings {
		s := seeded(func(s *protocol.Settings) {
			s.Mode = protocol.ModeQuote
			s.WordCount = 7
			s.DictHash = ""
			s.TextSource = protocol.TextSource{Kind: protocol.TextSourceQuote, QuoteID: "q-1"}
		})
		if mutate != nil {
			mutate(&s)
		}
		return s
	}

	tests := []struct {
		name     string
		settings protocol.Settings
		wantErr  string
	}{
		{"seeded words", seeded(nil), ""},
		{"seeded time", seeded(func(s *protocol.Settings) {
			s.Mode = protocol.ModeTime
			s.WordCount = 0
			s.DurationMs = 30000
		}), ""},
		{"seeded without dictHash", seeded(func(s *protocol.Settings) { s.DictHash = "" }), "dictHash is required"},
		{"seeded with a quote source", seeded(func(s *protocol.Settings) {
			s.TextSource = protocol.TextSource{Kind: protocol.TextSourceQuote, QuoteID: "q-1"}
		}), "textSource.kind must be 'seeded'"},

		// A quote match carries no dictionary at all: `code_python` and friends
		// are quote-only languages with no served word list to fingerprint.
		{"quote", quote(nil), ""},
		{"quote without a dictHash is fine", quote(func(s *protocol.Settings) { s.DictHash = "" }), ""},
		{"quote without an id", quote(func(s *protocol.Settings) {
			s.TextSource.QuoteID = ""
		}), "textSource.quoteId is required for quote mode"},
		{"quote with a seeded source", quote(func(s *protocol.Settings) {
			s.TextSource = protocol.TextSource{Kind: protocol.TextSourceSeeded}
		}), "quote mode requires textSource.kind 'quote'"},
		{"quote without a word count", quote(func(s *protocol.Settings) {
			s.WordCount = 0
		}), "wordCount must be positive for quote mode"},
		{"quote without a lang", quote(func(s *protocol.Settings) { s.Lang = "" }), "lang is required"},

		{"unknown mode", seeded(func(s *protocol.Settings) {
			s.Mode = "zen"
		}), "mode must be 'time', 'words' or 'quote'"},

		// The free-text identifiers are bounded because every one of them is
		// echoed far beyond the sender — into each seat's room_state, the
		// countdown, the persisted match row, and (for an open room) the
		// ANONYMOUS lobby listing. Unbounded, one frame under the transport's
		// own 2 MiB limit became multi-megabyte public GET responses.
		{"lang at the ceiling", seeded(func(s *protocol.Settings) {
			s.Lang = strings.Repeat("a", protocol.LangMaxLen)
		}), ""},
		{"lang past the ceiling", seeded(func(s *protocol.Settings) {
			s.Lang = strings.Repeat("a", protocol.LangMaxLen+1)
		}), "lang must be at most 32 characters"},
		{"dictHash past the ceiling", seeded(func(s *protocol.Settings) {
			s.DictHash = strings.Repeat("f", protocol.DictHashMaxLen+1)
		}), "dictHash must be at most 64 characters"},
		{"quoteId past the ceiling", quote(func(s *protocol.Settings) {
			s.TextSource.QuoteID = strings.Repeat("q", protocol.QuoteIDMaxLen+1)
		}), "textSource.quoteId must be at most 64 characters"},

		// The dimensions are bounded by the SAME numbers the HTTP ingest path
		// uses (internal/runlimits): a match the run endpoint would refuse is a
		// match whose results nobody can submit, and `durationMs` also arms the
		// deadline timer and decides how long every seat's capture is retained.
		{"words at the ceiling", seeded(func(s *protocol.Settings) {
			s.WordCount = runlimits.MaxWordCount
		}), ""},
		{"words past the ceiling", seeded(func(s *protocol.Settings) {
			s.WordCount = runlimits.MaxWordCount + 1
		}), "wordCount must be at most 3000"},
		{"time at the ceiling", seeded(func(s *protocol.Settings) {
			s.Mode = protocol.ModeTime
			s.WordCount = 0
			s.DurationMs = runlimits.MaxDurationMs
		}), ""},
		{"time past the ceiling", seeded(func(s *protocol.Settings) {
			s.Mode = protocol.ModeTime
			s.WordCount = 0
			s.DurationMs = runlimits.MaxDurationMs + 1
		}), "durationMs must be at most 3600000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := protocol.ValidateSettings(tc.settings)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantErr, err.Error())
		})
	}
}

// TestIsCounted pins the property the room relies on: a quote match ends the
// same way a words match does — by running out of text, not out of clock.
func TestIsCounted(t *testing.T) {
	assert.True(t, protocol.IsCounted(protocol.ModeWords))
	assert.True(t, protocol.IsCounted(protocol.ModeQuote))
	assert.False(t, protocol.IsCounted(protocol.ModeTime))
}
