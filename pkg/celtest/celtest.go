// Package celtest re-exports the upstream k8s.io/apiserver/pkg/cel/testing/celtest
// package and adds downstream-specific helpers like GatekeeperPreamble.
//
// The core library lives upstream in k8s.io/apiserver. This package provides
// a convenient import path for the kubernetes-sigs/cel-test CLI tool.
package celtest

import (
"testing"

upstream "k8s.io/apiserver/pkg/cel/testing/celtest"
)

// Re-export all types from the upstream package.
type (
Evaluator       = upstream.Evaluator
Option          = upstream.Option
Variable        = upstream.Variable
Validation      = upstream.Validation
VAPPolicy       = upstream.VAPPolicy
AdmissionInput  = upstream.AdmissionInput
AdmissionResult = upstream.AdmissionResult
Violation       = upstream.Violation
)

// Re-export all functions from the upstream package.
var (
NewEvaluator                   = upstream.NewEvaluator
WithVersion                    = upstream.WithVersion
WithPreambleVariables          = upstream.WithPreambleVariables
WithCostLimit                  = upstream.WithCostLimit
ParseVAPPolicy                 = upstream.ParseVAPPolicy
ParseVAPPolicyFile             = upstream.ParseVAPPolicyFile
DiscoverAndRunTestsRaw         = upstream.DiscoverAndRunTestsRaw
DiscoverAndRunTestsWithEvaluator = upstream.DiscoverAndRunTestsWithEvaluator
RunTestFileWithEvaluator       = upstream.RunTestFileWithEvaluator
)

// PerCallLimit is the K8s API server's per-expression cost limit.
const PerCallLimit = upstream.PerCallLimit

// GatekeeperPreamble returns the standard Gatekeeper preamble variables
// (anyObject, params). This is downstream-specific to the Gatekeeper framework
// and not part of the upstream k8s.io/apiserver package.
func GatekeeperPreamble() []Variable {
return []Variable{
{
Name:       "anyObject",
Expression: `has(request.operation) && request.operation == "DELETE" && object == null ? oldObject : object`,
},
{
Name:       "params",
Expression: `!has(params.spec) ? null : !has(params.spec.parameters) ? null : params.spec.parameters`,
},
}
}

// DiscoverAndRunTests walks srcRoot for *_test.cel files and runs them
// using Gatekeeper preamble variables and parameter wrapping.
// This is a convenience wrapper for Gatekeeper-style policies.
// For vanilla VAP / Kyverno / standalone expressions, use DiscoverAndRunTestsRaw.
func DiscoverAndRunTests(t *testing.T, srcRoot string) {
eval, err := NewEvaluator(WithPreambleVariables(GatekeeperPreamble()...))
if err != nil {
t.Fatalf("creating evaluator: %v", err)
}
upstream.DiscoverAndRunTestsWithEvaluator(t, eval, srcRoot, true)
}
