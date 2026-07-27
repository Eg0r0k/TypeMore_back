package ws

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/typemore/typemore-server/internal/protocol"
)

// Chat rate-limit token bucket: burst of 5, fully refilled over 2s (one token
// every 400ms). Matches the shared limiter's intent without importing the auth
// domain into the transport layer.
const (
	chatBurst  = 5
	chatRefill = 400 * time.Millisecond
)

// allowChat reports whether this seat may send a chat message now, consuming a
// token if so. A token-bucket refilling one token per chatRefill up to chatBurst.
func (st *seat) allowChat(now time.Time) bool {
	if st.chatLast.IsZero() {
		st.chatTokens = chatBurst
	} else {
		st.chatTokens = min(float64(chatBurst), st.chatTokens+now.Sub(st.chatLast).Seconds()/chatRefill.Seconds())
	}
	st.chatLast = now
	if st.chatTokens >= 1 {
		st.chatTokens--
		return true
	}
	return false
}

// chat validates, rate-limits, and broadcasts a lobby chat message.
func (r *Room) chat(sess *session, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	text = strings.TrimSpace(text)
	if n := utf8.RuneCountInString(text); n < protocol.ChatTextMinLen || n > protocol.ChatTextMaxLen {
		r.errLocked(sess, protocol.CodeBadMessage, "chat text must be 1-200 characters")
		return
	}
	if !st.allowChat(time.Now()) {
		r.errLocked(sess, protocol.CodeRateLimited, "chat rate limit exceeded")
		return
	}
	msg := protocol.Chat{Type: protocol.TypeChat, From: st.playerID, Text: text, Ts: nowMs()}
	for _, s := range r.seats {
		r.deliverLocked(s, msg)
	}
}

// systemChatLocked broadcasts a server system chat message to every live seat.
func (r *Room) systemChatLocked(kind, text string) {
	msg := protocol.Chat{
		Type: protocol.TypeChat,
		From: protocol.ChatFromSystem,
		Kind: kind,
		Text: text,
		Ts:   nowMs(),
	}
	for _, s := range r.seats {
		r.deliverLocked(s, msg)
	}
}
