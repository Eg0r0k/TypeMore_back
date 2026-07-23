package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// tokenBytesLen is the raw entropy of session and email tokens: 256 bits. That
// is far beyond guessable and matches the resumeToken sizing in the protocol.
const tokenBytesLen = 32

// newToken returns a fresh random token as a URL-safe string (the value handed
// to the client) together with its SHA-256 hash (the value stored server-side).
//
// Only the hash is ever persisted, so a database leak does not expose usable
// tokens. The plaintext string exists only long enough to put in a cookie or an
// email link and is never logged.
func newToken() (plaintext string, hash []byte, err error) {
	raw := make([]byte, tokenBytesLen)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:], nil
}

// hashToken maps a token string received from a client back to the stored hash
// form for lookup. It returns an error if the string is not valid token
// encoding (so a malformed cookie/link is rejected, not treated as a miss on a
// garbage hash).
func hashToken(plaintext string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(plaintext)
	if err != nil || len(raw) != tokenBytesLen {
		return nil, errInvalidToken
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}
