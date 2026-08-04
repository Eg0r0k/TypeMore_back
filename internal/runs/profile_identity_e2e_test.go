package runs_test

// The profile's identity half end-to-end (00029, docs/PROFILE.md): the bio, the
// board, the social links and the badge showcase, over real HTTP.
//
// Every one of these is about a boundary the server owns and a client cannot be
// trusted with: what a handle may be, whose badges you may display, and what a
// stranger sees of a profile whose owner closed it.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ownProfile struct {
	Bio      *string `json:"bio"`
	Keyboard *string `json:"keyboard"`
	Links    []struct {
		Kind   string `json:"kind"`
		Handle string `json:"handle"`
	} `json:"links"`
	Badges []struct {
		Code  string `json:"code"`
		Shown bool   `json:"shown"`
		Order *int32 `json:"order"`
	} `json:"badges"`
	KnownBadges []string `json:"knownBadges"`
}

type publicHeader struct {
	Name     string  `json:"name"`
	Public   bool    `json:"public"`
	Bio      *string `json:"bio"`
	Keyboard *string `json:"keyboard"`
	Links    []struct {
		Kind   string `json:"kind"`
		Handle string `json:"handle"`
	} `json:"links"`
	Badges []string `json:"badges"`
}

// The round trip: write a profile, read it back on the owner's route and on the
// public one. The links carry HANDLES — the public payload must never grow a
// URL, because the renderer's fixed prefix is what owns the host.
func TestProfilePatchRoundTripsToThePublicHeader(t *testing.T) {
	h := newHarness(t)
	h.login("identity@example.com", "correct horse battery", "identityplayer")

	own := decodeInto[ownProfile](t, h.patch("/api/v1/me/profile", map[string]any{
		"bio":      "  types words, sometimes correctly  ",
		"keyboard": "Keychron Q1 / Gateron Brown",
		"links":    map[string]string{"github": "Eg0r-Kill", "twitch": "egor_kill"},
	}))
	require.NotNil(t, own.Bio)
	assert.Equal(t, "types words, sometimes correctly", *own.Bio, "the bio is trimmed")
	require.NotNil(t, own.Keyboard)
	assert.Len(t, own.Links, 2)

	h.logout()
	header := decodeInto[publicHeader](t, h.get("/api/v1/users/identityplayer"))
	require.NotNil(t, header.Bio)
	assert.Equal(t, "types words, sometimes correctly", *header.Bio)
	require.Len(t, header.Links, 2, "links are ordered by kind, so the header renders stably")
	assert.Equal(t, "github", header.Links[0].Kind)
	assert.Equal(t, "Eg0r-Kill", header.Links[0].Handle)
	assert.NotContains(t, header.Links[0].Handle, "/", "a handle is never a URL")
}

// A PATCH is partial, and clearing is a different instruction from omitting.
func TestProfilePatchIsPartialAndClearsWithEmptyString(t *testing.T) {
	h := newHarness(t)
	h.login("partialprofile@example.com", "correct horse battery", "partialprofile")

	h.patch("/api/v1/me/profile", map[string]any{
		"bio":      "first",
		"keyboard": "board",
		"links":    map[string]string{"github": "egor"},
	})

	// Touching only the bio must leave the board and the link alone.
	own := decodeInto[ownProfile](t, h.patch("/api/v1/me/profile", map[string]any{"bio": "second"}))
	require.NotNil(t, own.Bio)
	assert.Equal(t, "second", *own.Bio)
	require.NotNil(t, own.Keyboard, "an unmentioned field must not move")
	assert.Len(t, own.Links, 1)

	// "" clears: the empty string is never STORED, so it can mean exactly one
	// thing on the wire.
	own = decodeInto[ownProfile](t, h.patch("/api/v1/me/profile", map[string]any{
		"bio":   "",
		"links": map[string]string{"github": ""},
	}))
	assert.Nil(t, own.Bio, "an empty bio clears to null rather than storing ''")
	assert.Empty(t, own.Links, "an empty handle removes the link's row")
	require.NotNil(t, own.Keyboard, "and still nothing else moved")

	// An empty patch is a client bug, and the whole route needs a session.
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{}), http.StatusBadRequest)
	h.logout()
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{"bio": "x"}), http.StatusUnauthorized)
}

// The server is the source of truth for handle shapes. A pasted URL — and
// anything else that could redirect a reader somewhere of the writer's choosing
// — has to be refused here, because the renderer only pastes onto a prefix.
func TestProfileRefusesUrlShapedHandlesAndOverlongText(t *testing.T) {
	h := newHarness(t)
	h.login("badlinks@example.com", "correct horse battery", "badlinks")

	for _, handle := range []string{
		"https://github.com/egor", "github.com/egor", "javascript:alert(1)",
		"//evil.example.com", "egor/../admin", "egor?x=1", "@egor",
	} {
		requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
			"links": map[string]string{"github": handle},
		}), http.StatusBadRequest)
	}
	// A service this build does not link to is refused by NAME, so a client
	// cannot invent a prefix by inventing a kind.
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"links": map[string]string{"tiktok": "egor"},
	}), http.StatusBadRequest)

	// The length caps are the schema's, restated by the handler so the refusal
	// is a message rather than a constraint violation.
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"bio": strings.Repeat("я", 251),
	}), http.StatusBadRequest)
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"keyboard": strings.Repeat("k", 101),
	}), http.StatusBadRequest)
	// 250 RUNES of Cyrillic is legal: the cap counts characters, so a
	// non-ASCII bio gets the room an ASCII one gets.
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"bio": strings.Repeat("я", 250),
	}), http.StatusOK)

	h.logout()
	header := decodeInto[publicHeader](t, h.get("/api/v1/users/badlinks"))
	assert.Empty(t, header.Links, "nothing a refusal touched was stored")
}

// The showcase is validated against what the account HOLDS. A real badge
// somebody else was granted is exactly the request this refuses.
func TestShowcaseAcceptsOnlyBadgesTheAccountHolds(t *testing.T) {
	h := newHarness(t)
	holder := h.login("holder@example.com", "correct horse battery", "badgeholder")

	// Nothing granted yet: every code is somebody else's.
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"showcase": []string{"staff"},
	}), http.StatusBadRequest)
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"showcase": []string{"not_a_badge"},
	}), http.StatusBadRequest)

	h.grantBadge(t, holder, "beta_tester")
	h.grantBadge(t, holder, "translator")

	// Granted but not shown: holding and displaying are separate acts.
	own := decodeInto[ownProfile](t, h.get("/api/v1/me/profile"))
	require.Len(t, own.Badges, 2)
	for _, b := range own.Badges {
		assert.False(t, b.Shown, "a grant does not display itself")
	}
	h.logout()
	header := decodeInto[publicHeader](t, h.get("/api/v1/users/badgeholder"))
	assert.Empty(t, header.Badges, "an unarranged showcase renders nothing at all")

	// The owner arranges it, and the ORDER they gave is the order served.
	h.loginAs("holder@example.com", "correct horse battery")
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"showcase": []string{"translator", "beta_tester"},
	}), http.StatusOK)
	// A badge cannot be shown twice, and one they do not hold cannot ride in
	// beside ones they do.
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"showcase": []string{"translator", "translator"},
	}), http.StatusBadRequest)
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"showcase": []string{"translator", "staff"},
	}), http.StatusBadRequest)

	h.logout()
	header = decodeInto[publicHeader](t, h.get("/api/v1/users/badgeholder"))
	assert.Equal(t, []string{"translator", "beta_tester"}, header.Badges,
		"the showcase is served in its owner's order")
}

// A revoked badge leaves the public page by itself. The read's predicate is the
// revocation, never the display_order — an operator taking a badge away must
// not depend on its owner tidying up afterwards.
func TestRevokedBadgeLeavesTheShowcaseWithNoOwnerAction(t *testing.T) {
	h := newHarness(t)
	userID := h.login("revoked@example.com", "correct horse battery", "revokedplayer")
	h.grantBadge(t, userID, "staff")
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"showcase": []string{"staff"},
	}), http.StatusOK)

	h.logout()
	header := decodeInto[publicHeader](t, h.get("/api/v1/users/revokedplayer"))
	require.Equal(t, []string{"staff"}, header.Badges)

	h.revokeBadge(t, userID, "staff")

	header = decodeInto[publicHeader](t, h.get("/api/v1/users/revokedplayer"))
	assert.Empty(t, header.Badges, "a revoked badge is gone from the public page immediately")

	// And it is gone from the owner's pool too, so they cannot re-show it.
	h.loginAs("revoked@example.com", "correct horse battery")
	own := decodeInto[ownProfile](t, h.get("/api/v1/me/profile"))
	assert.Empty(t, own.Badges)
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"showcase": []string{"staff"},
	}), http.StatusBadRequest)
}

// The identity half is profile CONTENT, so it rides the profile's own gate: a
// closed profile answers a stranger with the same header it always did — a
// name, a join date and the closed flag — and nothing its owner wrote.
func TestClosedProfileWithholdsTheIdentityHalf(t *testing.T) {
	h := newHarness(t)
	userID := h.login("closedidentity@example.com", "correct horse battery", "closedidentity")
	h.grantBadge(t, userID, "staff")
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"bio":      "a bio nobody may read",
		"links":    map[string]string{"github": "secretive"},
		"showcase": []string{"staff"},
	}), http.StatusOK)
	requireStatus(t, h.patch("/api/v1/me/settings", map[string]bool{"profilePublic": false}),
		http.StatusOK)

	// The OWNER still previews their own page whole.
	header := decodeInto[publicHeader](t, h.get("/api/v1/users/closedidentity"))
	require.NotNil(t, header.Bio)
	assert.Equal(t, []string{"staff"}, header.Badges)

	h.logout()
	raw := string(readBody(t, h.get("/api/v1/users/closedidentity")))
	assert.NotContains(t, raw, "a bio nobody may read")
	assert.NotContains(t, raw, "secretive")
	assert.NotContains(t, raw, "staff")

	closed := decodeInto[publicHeader](t, h.get("/api/v1/users/closedidentity"))
	assert.Equal(t, "closedidentity", closed.Name, "closed is a STATE — the page still exists")
	assert.False(t, closed.Public)
	assert.Nil(t, closed.Bio)
	assert.Empty(t, closed.Badges)
}

// The header's key set, re-asserted WITH the identity half in play: the
// allowlist grew by exactly the fields this feature added, and the account
// facts that must never ride along still do not.
func TestPublicHeaderAllowlistAfterIdentity(t *testing.T) {
	h := newHarness(t)
	userID := h.login("allowlist2@example.com", "correct horse battery", "allowlisttwo")
	h.grantBadge(t, userID, "staff")
	requireStatus(t, h.patch("/api/v1/me/profile", map[string]any{
		"bio":      "everything filled in",
		"keyboard": "a board",
		"links":    map[string]string{"github": "egor"},
		"showcase": []string{"staff"},
	}), http.StatusOK)
	h.logout()

	header := decodeInto[map[string]any](t, h.get("/api/v1/users/allowlisttwo"))
	got := make([]string, 0, len(header))
	for k := range header {
		got = append(got, k)
	}
	sort.Strings(got)
	assert.Equal(t, []string{"badges", "bio", "joined", "keyboard", "links", "name", "public"}, got,
		"a new field on the public header is a DELIBERATE disclosure — update this snapshot in the same commit that argues why")

	raw := string(readBody(t, h.get("/api/v1/users/allowlisttwo")))
	for _, secret := range []string{
		"allowlist2@example.com", "password", "provider",
		"keyboardPublic", "restricted", "role", "permissions",
	} {
		assert.NotContains(t, raw, secret, "the public header must not carry %q", secret)
	}
}

// grantBadge / revokeBadge go through the moderation STORE rather than the
// admin HTTP surface: this suite has no admin session, and what these tests are
// about is the profile's behaviour once a grant exists — the admin surface's
// own idempotency and audit trail are pinned in internal/moderation.
func (h *harness) grantBadge(t *testing.T, userID, code string) {
	t.Helper()
	_, err := h.moderation.GrantBadge(context.Background(), uuid.MustParse(userID), code, nil)
	require.NoError(t, err)
}

func (h *harness) revokeBadge(t *testing.T, userID, code string) {
	t.Helper()
	revoked, err := h.moderation.RevokeBadge(context.Background(), uuid.MustParse(userID), code, nil)
	require.NoError(t, err)
	require.True(t, revoked, "the badge must have been live to revoke")
}
