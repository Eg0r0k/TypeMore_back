package auth_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMeJSONContract pins the exact wire field names of GET /me. The frontend
// valibot-parses the response strictly, so a casing or naming drift (e.g.
// display_name / created_at) would silently break the client. Asserting the raw
// JSON key set — rather than decoding into a struct, which would tolerate a
// renamed field — is what makes this a contract guard.
func TestMeJSONContract(t *testing.T) {
	h := newHarness(t)
	h.registerVerifyLogin("contract@example.com", "password-123", "Contract")

	resp := h.get(mePath)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(readBody(t, resp)), &fields))

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"createdAt", "displayName", "id"}, keys,
		"GET /me must expose exactly {id, displayName, createdAt} in lower-camelCase")
}
