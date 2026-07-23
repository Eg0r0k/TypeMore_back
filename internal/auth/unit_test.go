package auth_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/auth"
)

// These are pure unit tests: they need no database and do not touch ensureDB.

func TestPasswordHashVerify(t *testing.T) {
	const pw = "correct horse battery staple"

	hash, err := auth.HashPassword(pw)
	require.NoError(t, err)
	assert.Contains(t, hash, "$argon2id$", "hash should be PHC-encoded")

	ok, err := auth.VerifyPassword(pw, hash)
	require.NoError(t, err)
	assert.True(t, ok, "correct password should verify")

	ok, err = auth.VerifyPassword("wrong password", hash)
	require.NoError(t, err)
	assert.False(t, ok, "wrong password must not verify")

	// A second hash of the same password differs (random salt).
	hash2, err := auth.HashPassword(pw)
	require.NoError(t, err)
	assert.NotEqual(t, hash, hash2)

	// A malformed hash is an error, not a panic or a false positive.
	_, err = auth.VerifyPassword(pw, "not-a-valid-hash")
	assert.Error(t, err)
}

func TestRateLimiter(t *testing.T) {
	// Burst of 2, slow refill: the third immediate call is denied.
	rl := auth.NewInMemoryRateLimiter(time.Hour, 2)
	assert.True(t, rl.Allow("ip-a"))
	assert.True(t, rl.Allow("ip-a"))
	assert.False(t, rl.Allow("ip-a"), "third call exceeds burst")

	// A different key has its own bucket.
	assert.True(t, rl.Allow("ip-b"))

	// A non-positive burst disables limiting entirely.
	off := auth.NewInMemoryRateLimiter(time.Hour, 0)
	for range 5 {
		assert.True(t, off.Allow("ip-a"))
	}
}
