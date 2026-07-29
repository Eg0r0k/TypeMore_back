//go:build anticheat

package policy_test

import (
	"testing"

	"github.com/typemore/typemore-server/internal/replay/policy"
	"github.com/typemore/typemore-server/internal/replay/policy/policytest"
)

// The shipped policy against the contract every judge must satisfy — the same
// set the open implementations run. That is the point of expressing it in the
// open: "is this still a judge" has one answer, and it does not depend on which
// build is asking.
func TestShippedPolicySatisfiesTheContract(t *testing.T) {
	policytest.RunContract(t, "review-policy", func() policy.Judge {
		j, err := policy.Provide(policy.Config{})
		if err != nil {
			t.Fatalf("build the shipped policy: %v", err)
		}
		return j
	})

	// And under tuning, because an operator's overrides must not be able to turn
	// it into something that is no longer a judge.
	policytest.RunContract(t, "review-policy/tuned", func() policy.Judge {
		j, err := policy.Provide(policy.Config{
			FlagWeights: "min-interval=0.9,paste=0.1", ReviewThreshold: 0.5})
		if err != nil {
			t.Fatalf("build the tuned policy: %v", err)
		}
		return j
	})
}
