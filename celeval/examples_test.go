package celeval_test

import (
	"testing"

	"github.com/JaydipGabani/cel-test/celeval"
)

// TestExamplePolicies runs declarative tests for non-Gatekeeper examples.
// No preamble variables — vanilla VAP and Kyverno-style policies.
func TestExamplePolicies(t *testing.T) {
	celeval.DiscoverAndRunTestsRaw(t, "../examples")
}
