package profile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/badges"
	"github.com/typemore/typemore-server/internal/profile"
)

// The handle grammars, and specifically the property the whole "store a handle,
// never a URL" decision rests on: nothing that could redirect a reader
// somewhere of the writer's choosing can get stored. A pattern that admitted a
// scheme, a host or a path would make the renderer's fixed prefix a suggestion.
func TestValidateLinkRefusesAnythingUrlShaped(t *testing.T) {
	// Every one of these is a real thing a user might paste, and each must be
	// refused for EVERY kind — the renderer's prefix is what owns the host, and
	// none of these can be pasted onto a prefix and still land where it says.
	urlShaped := []string{
		"https://github.com/egor",
		"http://twitch.tv/egor",
		"//evil.example.com",
		"github.com/egor",
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"egor/../../admin",
		"egor?next=https://evil.example",
		"egor#fragment",
		"egor egor",
		"egor\nhttps://evil.example",
		"@egor",
		"",
	}
	for _, kind := range profile.LinkKinds() {
		for _, handle := range urlShaped {
			assert.Error(t, profile.ValidateLink(kind, handle),
				"%s must refuse %q", kind, handle)
		}
	}
}

func TestValidateLinkPerKindGrammar(t *testing.T) {
	tests := []struct {
		kind, handle string
		ok           bool
	}{
		// GitHub: single hyphens inside, never at either end, 1-39 chars.
		{"github", "egor", true},
		{"github", "e", true},
		{"github", "Eg0r-Kill", true},
		{"github", "-egor", false},
		{"github", "egor-", false},
		{"github", "egor_kill", false}, // underscores are not GitHub's grammar
		{"github", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false}, // 40

		// YouTube handles, stored without the @, 3-30 chars.
		{"youtube", "typemore", true},
		{"youtube", "type.more_1-x", true},
		{"youtube", "ab", false},
		{"youtube", "type more", false},

		// Twitch logins: 4-25, letters/digits/underscore only.
		{"twitch", "egor", true},
		{"twitch", "eg0r_kill", true},
		{"twitch", "ego", false},
		{"twitch", "egor-kill", false},

		// A service this build does not link to is refused by NAME, so a client
		// cannot invent a prefix by inventing a kind.
		{"tiktok", "egor", false},
		{"", "egor", false},
	}
	for _, tc := range tests {
		err := profile.ValidateLink(tc.kind, tc.handle)
		if tc.ok {
			assert.NoError(t, err, "%s/%s should be accepted", tc.kind, tc.handle)
			continue
		}
		assert.Error(t, err, "%s/%s should be refused", tc.kind, tc.handle)
	}
}

// The kinds are a closed set: the renderer holds one URL prefix per kind, so a
// kind with no prefix would be a link nobody can build. This is what makes that
// pairing checkable from the Go side.
func TestLinkKindsAreTheDocumentedThree(t *testing.T) {
	assert.Equal(t, []string{"github", "twitch", "youtube"}, profile.LinkKinds())
}

// The badge list is the server's ONLY say about a badge: grant this code, or
// refuse it. Nothing about how it renders is here, and that is the contract.
func TestKnownBadgesAreACloseSet(t *testing.T) {
	require.NotEmpty(t, badges.Codes())
	for _, code := range badges.Codes() {
		assert.True(t, badges.Known(code))
	}
	for _, unknown := range []string{"", "STAFF", "staff ", "not_a_badge", "'; DROP TABLE"} {
		assert.False(t, badges.Known(unknown), "%q must not be grantable", unknown)
	}
}
