package moderation_test

// GrantBadgeBySystem — the entry point a future achievements pipeline calls.
// Same idempotence as an operator's grant; the difference under test is that
// NOBODY is recorded behind the act, and the admin surface renders exactly
// that absence.

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemGrantRecordsNoActorAndStaysIdempotent(t *testing.T) {
	h := newAdminHarness(t)
	earner := h.user(t, "earner")
	ctx := context.Background()

	granted, err := h.store.GrantBadgeBySystem(ctx, earner, "beta_tester")
	require.NoError(t, err)
	assert.Equal(t, "beta_tester", granted.Code)
	assert.True(t, granted.Granted())

	// Earning it twice is earning it once — the shape an achievements pipeline
	// re-running over history depends on.
	_, err = h.store.GrantBadgeBySystem(ctx, earner, "beta_tester")
	require.NoError(t, err)

	list := badgesOf(t, h, "earner")
	require.Len(t, list, 1, "a re-earned badge must not stack a second row")
	assert.Empty(t, list[0].GrantedBy, "a system grant has no actor to record")
	// Earned, not displayed: the showcase stays the owner's act, exactly as
	// with an operator's grant.
	assert.False(t, list[0].Shown)

	// An unknown code is refused the same way the operator path refuses it.
	_, err = h.store.GrantBadgeBySystem(ctx, earner, "a_badge_from_the_future")
	require.Error(t, err)

	// And the admin surface can still revoke a system grant.
	resp, _ := h.do(t, http.MethodDelete, "/users/earner/badges/beta_tester", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
