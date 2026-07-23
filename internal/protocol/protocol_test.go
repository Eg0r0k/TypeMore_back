package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
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
