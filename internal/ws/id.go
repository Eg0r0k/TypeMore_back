package ws

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a random, URL-safe opaque identifier used as a player id. It is
// generated from crypto/rand so it is unguessable — in the relay phase the same
// value doubles as the reconnect token, so it must not be predictable.
//
// rand.Read from crypto/rand never returns an error on supported platforms; if
// the OS entropy source ever failed we would rather panic at startup of a
// connection than hand out a weak/empty token.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ws: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// newResumeToken returns a 256-bit (64 hex char) random secret used as the
// per-connection reconnect capability (docs/PROTOCOL.md §4 hello_ok / §6). It is
// distinct from and larger than the peer-visible player id, and is compared in
// constant time on reconnect.
func newResumeToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ws: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// newMatchID returns a wire/DB match identifier: an "m_" prefix plus 12 random
// hex chars. It doubles as the matches table primary key.
func newMatchID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ws: crypto/rand failed: " + err.Error())
	}
	return "m_" + hex.EncodeToString(b[:])
}
