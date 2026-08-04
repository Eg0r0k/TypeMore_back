package ws

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/typemore/typemore-server/internal/protocol"
)

// chat validates, rate-limits, and broadcasts a lobby chat message.
//
// The budget is the SESSION's (ratelimit.go), not the seat's. It used to be the
// seat's, which meant leave + join_room minted a fresh seat with a full burst
// and the limit bounded nothing at all.
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
	if !sess.chats.allow(time.Now(), chatBurst, chatRefill) {
		r.errLocked(sess, protocol.CodeRateLimited, "chat rate limit exceeded")
		return
	}
	r.touchLocked()
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
