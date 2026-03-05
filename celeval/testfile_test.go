package celeval_test

import (
	"testing"

	"github.com/JaydipGabani/cel-test/celeval"
)

// TestCELPolicies discovers and runs all *_test.cel files under src/.
func TestCELPolicies(t *testing.T) {
	celeval.DiscoverAndRunTests(t, "../testdata/gatekeeper")
}
