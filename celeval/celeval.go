// Package celeval is a thin wrapper around celtest (k8s.io/apiserver/pkg/cel/testing/celtest),
// the upstream K8s CEL test package. It re-exports the upstream types and adds
// YAML parsing for policy files and declarative test format support.
//
// When the upstream package ships in k8s.io/apiserver, this wrapper becomes unnecessary —
// consumers can import celtest directly. This package exists only because the upstream
// package doesn't exist yet; the vendored copy demonstrates what it would look like.
package celeval

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
	"k8s.io/apiserver/pkg/cel/testing/celtest"
)

// Re-export upstream types so testfile.go and tests can use them without
// importing the upstream package directly.
type (
	Variable        = celtest.Variable
	Validation      = celtest.Validation
	VAPPolicy       = celtest.VAPPolicy
	AdmissionInput  = celtest.AdmissionInput
	AdmissionResult = celtest.AdmissionResult
	Violation       = celtest.Violation
	Evaluator       = celtest.Evaluator
	Option          = celtest.Option
)

// Re-export upstream functions.
var (
	NewEvaluator         = celtest.NewEvaluator
	WithVersion          = celtest.WithVersion
	WithPreambleVariables = celtest.WithPreambleVariables
	WithCostLimit        = celtest.WithCostLimit
	GatekeeperPreamble   = celtest.GatekeeperPreamble
)

// ParseVAPPolicyFile reads and parses a VAP policy YAML file.
// This is the YAML layer — the upstream celtest package doesn't include
// YAML parsing since policy file format is a downstream concern.
func ParseVAPPolicyFile(path string) (*VAPPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading CEL file: %w", err)
	}
	return ParseVAPPolicy(string(data))
}

// ParseVAPPolicy parses a VAP policy from a YAML string.
func ParseVAPPolicy(content string) (*VAPPolicy, error) {
	// yaml tags on Variable/Validation/VAPPolicy are needed for parsing
	var raw struct {
		Variables []struct {
			Name       string `yaml:"name"`
			Expression string `yaml:"expression"`
		} `yaml:"variables"`
		Validations []struct {
			Expression        string `yaml:"expression"`
			Message           string `yaml:"message,omitempty"`
			MessageExpression string `yaml:"messageExpression,omitempty"`
		} `yaml:"validations"`
	}
	if err := yaml.Unmarshal([]byte(content), &raw); err != nil {
		return nil, fmt.Errorf("parsing CEL policy YAML: %w", err)
	}

	policy := &VAPPolicy{}
	for _, v := range raw.Variables {
		policy.Variables = append(policy.Variables, Variable{Name: v.Name, Expression: v.Expression})
	}
	for _, v := range raw.Validations {
		policy.Validations = append(policy.Validations, Validation{
			Expression:        v.Expression,
			Message:           v.Message,
			MessageExpression: v.MessageExpression,
		})
	}
	return policy, nil
}
