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
	"errors"
	"strings"
	"unicode"
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
	TypeHello          = "hello"
	TypeNTPPing        = "ntp_ping"
	TypeCreateRoom     = "create_room"
	TypeJoinRoom       = "join_room"
	TypeReady          = "ready"
	TypeSettingsUpdate = "settings_update"
	TypeSetFreemods    = "set_freemods"
	TypeStartMatch     = "start_match"
	TypeKick           = "kick"
	TypeTransferHost   = "transfer_host"
	TypeChatSend       = "chat_send"
	TypeEventBatch     = "event_batch"
	TypeFinish         = "finish"
	TypeLeave          = "leave"

	// Server -> client.
	TypeHelloOK    = "hello_ok"
	TypeError      = "error"
	TypeNTPPong    = "ntp_pong"
	TypeRoomState  = "room_state"
	TypeCountdown  = "countdown"
	TypeChat       = "chat"
	TypeKicked     = "kicked"
	TypePeerBatch  = "peer_batch"
	TypePeerStatus = "peer_status"
	TypeMatchEnd   = "match_end"
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
	// CodeForbidden marks a room action attempted without the required role
	// (e.g. a host-only message sent by a non-host seat).
	CodeForbidden = "forbidden"
	// CodeNotReady is start_match's rejection when the seat/ready gate is unmet.
	CodeNotReady = "not_ready"
	// CodeRateLimited throttles chat_send when the sender's token bucket is empty.
	CodeRateLimited = "rate_limited"
	CodeInternal    = "internal"
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

// Match-end reasons carried in a MatchEnd frame's reason field.
const (
	ReasonAllFinished  = "all_finished"
	ReasonDeadline     = "deadline"
	ReasonFinishWindow = "finish_window"
)

// AFK rules (docs/PROTOCOL.md §6). Two measures, because "AFK" has two shapes.
//
// AfkTrailingMs — the seat has produced NOTHING for this long, right now. This
// is the one a player feels: stand up mid-race and you are out 15 s later, in
// EVERY mode. It is the only AFK rule time mode gets, because a timed run cannot
// be completed early — stopping is a legitimate way to end one, so only a
// continuous hole proves absence rather than a decision.
//
// AfkKickShare — the idle SHARE of the elapsed window, monkeytype's AFK measure
// (one-second buckets with no input) transposed onto the server's receive clock,
// which is all a relay can see. It catches the seat that never really played:
// someone who typed two words in a minute is gone even if they twitch every 14 s.
// Words mode only: there, a seat that never finishes blocks the others, while a
// timed match ends on its own deadline. AfkWarmupMs holds the share rule off
// until the sample is worth judging (a 3 s-old match is 100% idle by construction);
// the trailing rule needs no warm-up — 15 s of silence is 15 s of silence.
//
// FinishWindowMs is the window that opens when the first seat finishes a
// words-mode match — at close every still-racing seat is dnf'd and the match
// ends with reason finish_window.
const (
	AfkTrailingMs  = 15_000
	AfkKickShare   = 0.6
	AfkWarmupMs    = 10_000
	AfkBucketMs    = 1_000
	FinishWindowMs = 120_000
)

// Envelope is the discriminator-only view of any frame. Decode into this first
// to learn the message type, then decode the full bytes into the concrete type.
type Envelope struct {
	Type string `json:"type"`
}

// --- Shared room/match value types ---

// TextMods are the text-affecting generation mods. They are part of the room
// Settings (host-controlled) because they change the generated text and MUST be
// identical for every player in a match (everyone types the same text).
type TextMods struct {
	Punctuation bool `json:"punctuation"`
	Numbers     bool `json:"numbers"`
	RandomCase  bool `json:"randomCase"`
	Reverse     bool `json:"reverse"`
}

// TextSource is the discriminated source of the match text. v0 has the single
// kind "seeded" (server-generated seed + dictionary). The quote phase adds
// {kind:"quote", quoteId} additively, so decoders MUST tolerate unknown kinds.
// The seed itself is never part of Settings — it is server-generated and travels
// only in Countdown (a player-chosen seed would be a pre-practiced map).
type TextSource struct {
	Kind    string `json:"kind"`
	QuoteID string `json:"quoteId,omitempty"`
}

// Settings is the host-controlled room configuration. Every field is text- or
// match-affecting and shared identically by all players. DurationMs applies to
// the "time" mode and WordCount to the "words" mode; the field not matching Mode
// is zero (omitted).
type Settings struct {
	Name       string     `json:"name"`
	Visibility string     `json:"visibility"`
	Mode       string     `json:"mode"`
	DurationMs int        `json:"durationMs,omitempty"`
	WordCount  int        `json:"wordCount,omitempty"`
	Lang       string     `json:"lang"`
	DictHash   string     `json:"dictHash"`
	TextMods   TextMods   `json:"textMods"`
	TextSource TextSource `json:"textSource"`
}

// Freemods are the per-player, log-provable mods chosen in the lobby. They COUNT
// toward the match score (only log-provable mods multiply the score) and are
// broadcast in RoomState and frozen into Countdown. Personal visual mods
// (blind/fading/flashlight) are client-local, never on the wire, and never
// scored.
type Freemods struct {
	Difficulty string `json:"difficulty"`
	MinWpm     int    `json:"minWpm"`
	Nospace    bool   `json:"nospace"`
}

// --- Client -> server messages ---

// Hello is the first frame a client must send. protocolVersion must equal
// Version or the connection is rejected. Nick is OPTIONAL and, for guest
// connections, IGNORED: the server assigns a unique per-room "Guest-XXXX"
// identity, and authenticated connections (session cookie on the WS upgrade)
// use the account displayName. The field is retained for forward compatibility.
type Hello struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocolVersion"`
	Nick            string `json:"nick,omitempty"`
	// ResumeToken is optional: present only on a reconnect to reclaim a seat
	// still inside its grace window — any phase, lobby or mid-match (see
	// docs/PROTOCOL.md §6). It is the 256-bit secret handed out in a prior
	// HelloOK, distinct from the peer-visible PlayerID.
	ResumeToken string `json:"resumeToken,omitempty"`
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

// Ready sets the sending player's ready flag. Ready is OPTIONAL: absent means
// true (mark ready); an explicit false clears the flag.
type Ready struct {
	Type  string `json:"type"`
	Ready *bool  `json:"ready,omitempty"`
}

// SettingsUpdate replaces the room Settings. Host-only and valid only between
// matches; it resets every seat's ready flag. Ack is a fresh RoomState.
type SettingsUpdate struct {
	Type     string   `json:"type"`
	Settings Settings `json:"settings"`
}

// SetFreemods sets the sending player's freemods. Any seat may send it, but only
// between matches (a set arriving during a match is rejected). Ack is RoomState.
type SetFreemods struct {
	Type     string   `json:"type"`
	Freemods Freemods `json:"freemods"`
}

// StartMatch is the host-only request to begin. It is valid iff there are at
// least two seats and every non-host seat is ready; otherwise it is rejected
// with CodeNotReady. On success the server emits a Countdown.
type StartMatch struct {
	Type string `json:"type"`
}

// Kick removes another seat (host-only). The kicked client receives a Kicked
// frame and is dropped from the room; its connection stays open so it may join
// elsewhere.
type Kick struct {
	Type     string `json:"type"`
	PlayerID string `json:"playerId"`
}

// TransferHost hands the host role to another seat (host-only). Ack is RoomState.
type TransferHost struct {
	Type     string `json:"type"`
	PlayerID string `json:"playerId"`
}

// ChatSend posts a lobby chat message (1-200 characters after trimming). It is
// rate-limited per sender; the server broadcasts a Chat frame to the room.
type ChatSend struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// EventBatch relays a batch of the frontend's log-v1 GameEvent objects. The
// events are opaque to the server in this phase (structurally validated in the
// relay phase), hence json.RawMessage.
type EventBatch struct {
	Type     string `json:"type"`
	MatchID  string `json:"matchId"`
	PlayerID string `json:"playerId"`
	// BatchSeq is a monotonic, per-player counter (starting at 1) so the server
	// can detect gaps/duplicates during backlog replay after a reconnect.
	BatchSeq int               `json:"batchSeq"`
	Version  int               `json:"version"`
	Events   []json.RawMessage `json:"events"`
}

// Finish signals that the sending player's run is over for the match identified
// by matchId. The server broadcasts a peer_status for that player and ends the
// match once every seat is finished/dnf/left.
//
// Forfeit says the run ended WITHOUT a result to show: the sender is abandoning
// it, not completing it (today: a page reload, which keeps the seat but destroys
// the run — see docs/PROTOCOL.md §6). Such a seat is recorded dnf, never
// finished, and does not open the words-mode finish window: it did not finish.
type Finish struct {
	Type    string `json:"type"`
	MatchID string `json:"matchId"`
	Forfeit bool   `json:"forfeit,omitempty"`
}

// Leave voluntarily removes the sending player from their room.
type Leave struct {
	Type string `json:"type"`
}

// --- Server -> client messages ---

// HelloOK acknowledges a valid hello and assigns the player their peer-visible
// server-issued id; the separate ResumeToken (relay phase) is the reconnect
// capability.
type HelloOK struct {
	Type          string `json:"type"`
	PlayerID      string `json:"playerId"`
	ServerVersion int    `json:"serverVersion"`
	// ResumeToken is a fresh 256-bit secret (64 hex chars) the client presents in
	// a later Hello to reclaim its seat after a disconnect. Populated by the relay
	// phase; empty (and omitted) otherwise.
	ResumeToken string `json:"resumeToken,omitempty"`
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

// Player is a room seat as seen in RoomState. IsGuest distinguishes a
// server-assigned "Guest-XXXX" identity from an authenticated displayName;
// Freemods are the seat's log-provable, score-affecting mod picks.
type Player struct {
	PlayerID string   `json:"playerId"`
	Nick     string   `json:"nick"`
	IsGuest  bool     `json:"isGuest"`
	Ready    bool     `json:"ready"`
	Freemods Freemods `json:"freemods"`
}

// RoomMatch identifies the match currently in flight, carried in RoomState only
// while one is running. A client that reconnects mid-match (page reload: the
// seat survives via its resumeToken, the tab's game state does not) learns from
// this that it holds a seat in a match it cannot play, and forfeits instead of
// silently stalling the other players until the deadline.
type RoomMatch struct {
	MatchID      string `json:"matchId"`
	GoAtServerMs int64  `json:"goAtServerMs"`
}

// RoomState is the full snapshot of a room, broadcast on any change. Name and
// Visibility are surfaced at the top level for convenience and also live inside
// Settings. HostPlayerID names the current host seat. Match is present iff a
// match is running.
type RoomState struct {
	Type         string     `json:"type"`
	Code         string     `json:"code"`
	Name         string     `json:"name"`
	Visibility   string     `json:"visibility"`
	HostPlayerID string     `json:"hostPlayerId"`
	Settings     Settings   `json:"settings"`
	Players      []Player   `json:"players"`
	Match        *RoomMatch `json:"match,omitempty"`
}

// CountdownPlayer is a seat's frozen freemod snapshot as carried in Countdown.
type CountdownPlayer struct {
	PlayerID string   `json:"playerId"`
	Freemods Freemods `json:"freemods"`
}

// Countdown announces the shared start. goAtServerMs is the server-clock instant
// of t=0 ("go"); clients convert it to local time via their NTP offset and
// schedule the local 3-2-1. Settings (including textSource, lang, dictHash and
// textMods) and the per-player freemod snapshot are frozen for the match; Seed
// is the server-generated generation seed (never chosen by a client), an integer
// in [0, 2^32-1].
type Countdown struct {
	Type         string            `json:"type"`
	MatchID      string            `json:"matchId"`
	GoAtServerMs int64             `json:"goAtServerMs"`
	Seed         int64             `json:"seed"`
	Settings     Settings          `json:"settings"`
	Players      []CountdownPlayer `json:"players"`
}

// Chat is a lobby chat frame. For a player message From is the sender's playerId
// and Kind is empty; for a server system message From is "system" and Kind is
// one of join|leave|settings_changed|host_changed. Ts is the server send time
// in Unix milliseconds.
type Chat struct {
	Type string `json:"type"`
	From string `json:"from"`
	Kind string `json:"kind,omitempty"`
	Text string `json:"text"`
	Ts   int64  `json:"ts"`
}

// Kicked notifies a client it was removed from its room by the host. The
// connection stays open so the client may join another room.
type Kicked struct {
	Type string `json:"type"`
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

// MatchResult is one frozen-roster seat's outcome inside a MatchEnd frame.
// FinishedAtMs is the server clock at receipt of that player's finish and is
// present only for status "finished". BatchCount/EventCount are the accepted
// event_batch envelope counts (0 for a silent player).
//
// AfkMs/AfkShare are the server's own idle measure over that seat's match window
// (§6): whole one-second buckets that carried no accepted event_batch, and their
// share of the window. They are derived from batch ARRIVAL times only — the
// server still never parses the opaque events, so these are transport facts, not
// gameplay metrics. AfkShare is the number the AFK dnf rule judges, published so
// the verdict is auditable client-side.
type MatchResult struct {
	PlayerID     string  `json:"playerId"`
	Status       string  `json:"status"`
	FinishedAtMs *int64  `json:"finishedAtMs,omitempty"`
	BatchCount   int     `json:"batchCount"`
	EventCount   int     `json:"eventCount"`
	AfkMs        int64   `json:"afkMs,omitempty"`
	AfkShare     float64 `json:"afkShare,omitempty"`
}

// MatchEnd announces the end of a match, exactly once per match, to every
// frozen-roster seat (graced seats receive it via their backlog on resume).
// Reason is one of the Reason* constants; Results covers the full frozen
// roster in no guaranteed order.
type MatchEnd struct {
	Type    string        `json:"type"`
	MatchID string        `json:"matchId"`
	Reason  string        `json:"reason"`
	Results []MatchResult `json:"results"`
}

// ValidNick reports whether nick satisfies the 1-16 code point bound. Length is
// counted in runes, not bytes, so multi-byte nicks are measured as the user
// perceives them.
func ValidNick(nick string) bool {
	n := utf8.RuneCountInString(nick)
	return n >= NickMinLen && n <= NickMaxLen
}

// Room name length bounds, in Unicode code points (runes).
const (
	RoomNameMinLen = 1
	RoomNameMaxLen = 32
)

// Room visibility values.
const (
	VisibilityOpen    = "open"
	VisibilityPrivate = "private"
)

// Match mode values. TimeMode is bounded by Settings.DurationMs, WordsMode by
// Settings.WordCount.
const (
	ModeTime  = "time"
	ModeWords = "words"
)

// Freemod difficulty values.
const (
	DifficultyNormal = "normal"
	DifficultyExpert = "expert"
	DifficultyMaster = "master"
)

// TextSource kinds. v0 has only "seeded"; the quote phase adds "quote".
const TextSourceSeeded = "seeded"

// System chat kinds (Chat.Kind when From == "system").
const (
	ChatFromSystem      = "system"
	ChatKindJoin        = "join"
	ChatKindLeave       = "leave"
	ChatKindSettings    = "settings_changed"
	ChatKindHostChanged = "host_changed"
)

// Chat text length bounds, in Unicode code points, after trimming.
const (
	ChatTextMinLen = 1
	ChatTextMaxLen = 200
)

// validMinWpm is the fixed set of accepted freemod minimum-WPM floors.
var validMinWpm = map[int]bool{0: true, 60: true, 80: true, 100: true}

// SanitizeRoomName trims the name, drops control characters, and truncates to
// RoomNameMaxLen runes. The result is what callers should store; validate its
// length with the return of ValidateSettings.
func SanitizeRoomName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	n := 0
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) {
			continue
		}
		if n >= RoomNameMaxLen {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// ValidateSettings reports the first problem with s, or nil if it is a valid
// room configuration. Name is validated as already sanitized (see
// SanitizeRoomName). The seed is never part of Settings and so is not validated
// here.
func ValidateSettings(s Settings) error {
	if n := utf8.RuneCountInString(s.Name); n < RoomNameMinLen || n > RoomNameMaxLen {
		return errors.New("name must be 1-32 characters")
	}
	if s.Visibility != VisibilityOpen && s.Visibility != VisibilityPrivate {
		return errors.New("visibility must be 'open' or 'private'")
	}
	switch s.Mode {
	case ModeTime:
		if s.DurationMs <= 0 {
			return errors.New("durationMs must be positive for time mode")
		}
	case ModeWords:
		if s.WordCount <= 0 {
			return errors.New("wordCount must be positive for words mode")
		}
	default:
		return errors.New("mode must be 'time' or 'words'")
	}
	if s.Lang == "" {
		return errors.New("lang is required")
	}
	if s.DictHash == "" {
		return errors.New("dictHash is required")
	}
	if s.TextSource.Kind != TextSourceSeeded {
		return errors.New("textSource.kind must be 'seeded'")
	}
	return nil
}

// ValidateFreemods reports the first problem with f, or nil if it is a valid
// freemod selection.
func ValidateFreemods(f Freemods) error {
	switch f.Difficulty {
	case DifficultyNormal, DifficultyExpert, DifficultyMaster:
	default:
		return errors.New("difficulty must be normal, expert, or master")
	}
	if !validMinWpm[f.MinWpm] {
		return errors.New("minWpm must be 0, 60, 80, or 100")
	}
	return nil
}

// DefaultFreemods is the freemod selection a seat starts with.
func DefaultFreemods() Freemods {
	return Freemods{Difficulty: DifficultyNormal, MinWpm: 0, Nospace: false}
}

// DefaultSettings is the configuration a freshly created room starts with. name
// is sanitized and, if empty, replaced with a generic fallback.
func DefaultSettings(name string) Settings {
	name = SanitizeRoomName(name)
	if name == "" {
		name = "Room"
	}
	return Settings{
		Name:       name,
		Visibility: VisibilityPrivate,
		Mode:       ModeTime,
		DurationMs: 30000,
		Lang:       "en",
		DictHash:   "en-default",
		TextMods:   TextMods{},
		TextSource: TextSource{Kind: TextSourceSeeded},
	}
}
