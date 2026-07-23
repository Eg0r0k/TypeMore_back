// Package protocol defines the wire format for the TypeMore realtime
// (WebSocket) API. It is the Go mirror of docs/PROTOCOL.md and is the single
// source of truth for message shapes on the server side.
//
// # Framing
//
// Every frame is a JSON *text* frame shaped like {"type": <string>, ...payload}.
// The "type" field is the discriminator carried by the Type field on every
// message struct. To decode an incoming frame, first unmarshal into Envelope to
// read the discriminator, then unmarshal the full bytes into the concrete type
// the discriminator names.
//
// # Versioning
//
// Version is numeric and changes only on breaking wire changes. The server
// speaks exactly one version and never translates between versions: a client
// that announces a different version in its hello is rejected with a
// version_mismatch error and disconnected (see docs/PROTOCOL.md).
//
// This package is intentionally pure data: types, constants, small validation
// helpers, and constructors. It contains no transport or game logic so that it
// can be shared verbatim as the contract with the frontend repository.
package protocol

import (
	"encoding/json"
	"unicode/utf8"
)

// Version is the protocol version this server speaks. It is transmitted as a
// JSON number in the hello frame's protocolVersion field.
const Version = 1

// Nick length bounds, in Unicode code points (runes). See ValidNick.
const (
	NickMinLen = 1
	NickMaxLen = 16
)

// Message type discriminators. These are the exact strings that appear in the
// "type" field of every frame.
const (
	// Client -> server.
	TypeHello      = "hello"
	TypeNTPPing    = "ntp_ping"
	TypeCreateRoom = "create_room"
	TypeJoinRoom   = "join_room"
	TypeReady      = "ready"
	TypeEventBatch = "event_batch"
	TypeLeave      = "leave"

	// Server -> client.
	TypeHelloOK    = "hello_ok"
	TypeError      = "error"
	TypeNTPPong    = "ntp_pong"
	TypeRoomState  = "room_state"
	TypeCountdown  = "countdown"
	TypePeerBatch  = "peer_batch"
	TypePeerStatus = "peer_status"
)

// Error codes carried in an Error frame's code field. version_mismatch is the
// only code that also closes the connection (the server performs the close
// after the frame is sent).
const (
	CodeVersionMismatch = "version_mismatch"
	CodeBadMessage      = "bad_message"
	CodeRoomNotFound    = "room_not_found"
	CodeRoomFull        = "room_full"
	CodeNotInRoom       = "not_in_room"
	CodeInternal        = "internal"
)

// Peer status values carried in a PeerStatus frame's status field.
const (
	StatusJoined       = "joined"
	StatusLeft         = "left"
	StatusDisconnected = "disconnected"
	StatusReconnected  = "reconnected"
	StatusFinished     = "finished"
	StatusDNF          = "dnf"
)

// Envelope is the discriminator-only view of any frame. Decode into this first
// to learn the message type, then decode the full bytes into the concrete type.
type Envelope struct {
	Type string `json:"type"`
}

// --- Client -> server messages ---

// Hello is the first frame a client must send. protocolVersion must equal
// Version or the connection is rejected; nick is the guest display identity.
type Hello struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocolVersion"`
	Nick            string `json:"nick"`
}

// NTPPing carries the client's clock reading (t0) at send time. The server
// answers with an NTPPong; the client repeats this several times to estimate
// the clock offset it will use to convert countdown times (see docs/PROTOCOL.md).
type NTPPing struct {
	Type string `json:"type"`
	T0   int64  `json:"t0"`
}

// CreateRoom asks the server to open a new room. Payload is finalized in the
// relay phase; it is defined here to complete the contract.
type CreateRoom struct {
	Type string `json:"type"`
}

// JoinRoom asks to join an existing room by its human-safe code.
type JoinRoom struct {
	Type string `json:"type"`
	Code string `json:"code"`
}

// Ready marks the sending player ready to start.
type Ready struct {
	Type string `json:"type"`
}

// EventBatch relays a batch of the frontend's log-v1 GameEvent objects. The
// events are opaque to the server in this phase (structurally validated in the
// relay phase), hence json.RawMessage.
type EventBatch struct {
	Type     string            `json:"type"`
	MatchID  string            `json:"matchId"`
	PlayerID string            `json:"playerId"`
	Version  int               `json:"version"`
	Events   []json.RawMessage `json:"events"`
}

// Leave voluntarily removes the sending player from their room.
type Leave struct {
	Type string `json:"type"`
}

// --- Server -> client messages ---

// HelloOK acknowledges a valid hello and assigns the player their server-issued
// id (which doubles as the reconnect token in the relay phase).
type HelloOK struct {
	Type          string `json:"type"`
	PlayerID      string `json:"playerId"`
	ServerVersion int    `json:"serverVersion"`
}

// NewHelloOK builds a HelloOK for the given player id, stamping the current
// protocol Version as serverVersion.
func NewHelloOK(playerID string) HelloOK {
	return HelloOK{Type: TypeHelloOK, PlayerID: playerID, ServerVersion: Version}
}

// Error reports a problem to the client. See the Code* constants for values.
type Error struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewError builds an Error frame.
func NewError(code, message string) Error {
	return Error{Type: TypeError, Code: code, Message: message}
}

// NTPPong answers an NTPPing. t0 is echoed unchanged; t1 is the server clock at
// receive; t2 is the server clock at send.
type NTPPong struct {
	Type string `json:"type"`
	T0   int64  `json:"t0"`
	T1   int64  `json:"t1"`
	T2   int64  `json:"t2"`
}

// NewNTPPong builds an NTPPong echoing t0 and reporting the server receive (t1)
// and send (t2) timestamps.
func NewNTPPong(t0, t1, t2 int64) NTPPong {
	return NTPPong{Type: TypeNTPPong, T0: t0, T1: t1, T2: t2}
}

// Player is a room member as seen in RoomState.
type Player struct {
	PlayerID string `json:"playerId"`
	Nick     string `json:"nick"`
	Ready    bool   `json:"ready"`
}

// RoomState is the full snapshot of a room, broadcast on any change. settings
// is opaque this phase (finalized in the relay phase).
type RoomState struct {
	Type     string          `json:"type"`
	Code     string          `json:"code"`
	Players  []Player        `json:"players"`
	Settings json.RawMessage `json:"settings"`
}

// Countdown announces the shared start. goAtServerMs is the server-clock instant
// of t=0 ("go"); clients convert it to local time via their NTP offset and
// schedule the local 3-2-1. seed/dictHash/lang/config describe the match text.
// config is opaque this phase.
type Countdown struct {
	Type         string          `json:"type"`
	GoAtServerMs int64           `json:"goAtServerMs"`
	Seed         int64           `json:"seed"`
	DictHash     string          `json:"dictHash"`
	Lang         string          `json:"lang"`
	Config       json.RawMessage `json:"config"`
}

// PeerBatch relays another player's events to this client, order preserved per
// player. Events are opaque (see EventBatch).
type PeerBatch struct {
	Type     string            `json:"type"`
	PlayerID string            `json:"playerId"`
	Events   []json.RawMessage `json:"events"`
}

// PeerStatus reports a lifecycle transition of a peer. See the Status* constants.
type PeerStatus struct {
	Type     string `json:"type"`
	PlayerID string `json:"playerId"`
	Status   string `json:"status"`
}

// ValidNick reports whether nick satisfies the 1-16 code point bound. Length is
// counted in runes, not bytes, so multi-byte nicks are measured as the user
// perceives them.
func ValidNick(nick string) bool {
	n := utf8.RuneCountInString(nick)
	return n >= NickMinLen && n <= NickMaxLen
}
