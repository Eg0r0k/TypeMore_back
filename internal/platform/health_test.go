package platform

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An instance running without a review policy is a supported deployment. A
// SILENT one is the worst outcome of making the policy removable: somebody
// inherits a server, believes its leaderboards are defended, and finds out
// otherwise from the leaderboards.
//
// The startup WARN covers whoever reads boot logs. /healthz covers everyone
// else — a monitor, a deploy check, the next maintainer.

func healthBody(t *testing.T, review ReviewPolicy) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	HealthHandler(review).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

func TestHealthzSaysWhenNothingIsJudging(t *testing.T) {
	body := healthBody(t, ReviewPolicy{Enabled: false, Version: "none"})

	var review ReviewPolicy
	require.NoError(t, json.Unmarshal(body["reviewPolicy"], &review))

	assert.False(t, review.Enabled)
	assert.Equal(t, "none", review.Version, "the version is what lands on every run this instance judges")
	require.NotEmpty(t, review.Warning, "a disabled policy must say so in words, not only in a boolean")
	// The sentence has to state the CONSEQUENCE. A reader deciding whether this
	// matters is usually not the person who turned it off.
	assert.Contains(t, review.Warning, "leaderboards are not protected")
	assert.Contains(t, review.Warning, "SELF_HOST.md")

	// Still 200: an instance without anti-cheat is a deployment, not an outage.
	// Returning 503 would take a working server out of a load balancer.
	assert.JSONEq(t, `"ok"`, string(body["status"]))
}

func TestHealthzSaysWhenSomethingIsJudging(t *testing.T) {
	body := healthBody(t, ReviewPolicy{Enabled: true, Version: "2"})

	var review ReviewPolicy
	require.NoError(t, json.Unmarshal(body["reviewPolicy"], &review))

	assert.True(t, review.Enabled)
	assert.Equal(t, "2", review.Version)
	assert.Empty(t, review.Warning, "an enabled policy has nothing to warn about")
}

// /healthz is a PUBLIC endpoint. It may say whether a policy is running and
// which version — operational facts — and nothing about what the policy
// decides. "Is anti-cheat on" is not the answer key; a weight or a threshold
// would be.
func TestHealthzLeaksNothingAboutWhatThePolicyDecides(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthHandler(ReviewPolicy{Enabled: true, Version: "2"}).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))

	body := rec.Body.String()
	for _, secret := range []string{
		"weight", "threshold", "suspicion", "bot_cadence", "sustained_superhuman",
	} {
		assert.NotContains(t, strings.ToLower(body), secret,
			"/healthz mentions %q: it reports WHETHER a policy runs, never what it decides", secret)
	}
}
