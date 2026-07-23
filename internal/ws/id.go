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
