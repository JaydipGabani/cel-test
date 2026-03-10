/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cel

// This file provides testing helpers for evaluating CEL expressions
// without requiring the full admission pipeline (admission.VersionedAttributes).
// These functions are intended for use by the celtest testing package.

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/interpreter"
	"k8s.io/apimachinery/pkg/util/version"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/environment"
)

// CreateTestEnv creates a properly-typed CEL environment for admission-style
// testing with the same variable declarations as the production admission path
// (object/oldObject as DynType, request as kubernetes.AdmissionRequest,
// namespaceObject as kubernetes.Namespace).
//
// This mirrors the unexported createEnvForOpts but is designed for test tooling.
func CreateTestEnv(baseEnv *environment.EnvSet, opts OptionalVariableDeclarations) (*environment.EnvSet, error) {
	requestType := BuildRequestType()
	namespaceType := BuildNamespaceType()

	var envOpts []cel.EnvOption
	envOpts = append(envOpts,
		cel.Variable(ObjectVarName, cel.DynType),
		cel.Variable(OldObjectVarName, cel.DynType),
		cel.Variable(NamespaceVarName, namespaceType.CelType()),
		cel.Variable(RequestVarName, requestType.CelType()),
	)
	if opts.HasParams {
		envOpts = append(envOpts, cel.Variable(ParamsVarName, cel.DynType))
	}
	// Note: HasAuthorizer is intentionally not handled here.
	// Authorizer testing requires mock authorizer objects (Phase 5).

	extended, err := baseEnv.Extend(
		environment.VersionedOptions{
			IntroducedVersion: version.MajorMinor(1, 0),
			EnvOptions:        envOpts,
			// DeclTypes must be registered so the CEL environment can resolve
			// structured types like kubernetes.AdmissionRequest and kubernetes.Namespace.
			// This matches the production createEnvForOpts.
			DeclTypes: []*apiservercel.DeclType{
				namespaceType,
				requestType,
			},
		},
		environment.StrictCostOpt,
	)
	if err != nil {
		return nil, fmt.Errorf("environment misconfigured: %w", err)
	}
	return extended, nil
}

// TestActivation is a CEL activation for evaluating admission expressions
// from unstructured inputs. It implements the same variable resolution as the
// internal evaluationActivation but accepts map[string]interface{} directly
// instead of requiring admission.VersionedAttributes.
type TestActivation struct {
	Object    interface{} // map[string]interface{} or nil → CEL "object"
	OldObject interface{} // map[string]interface{} or nil → CEL "oldObject"
	Params    interface{} // map[string]interface{} or nil → CEL "params"
	Request   interface{} // map[string]interface{} or nil → CEL "request"
	Namespace interface{} // map[string]interface{} or nil → CEL "namespaceObject"
	Variables interface{} // lazy variables (ref.Val) or map → CEL "variables"
}

// ResolveName implements the cel-go interpreter.Activation interface.
// The variable names match the upstream evaluationActivation exactly.
func (a *TestActivation) ResolveName(name string) (interface{}, bool) {
	switch name {
	case ObjectVarName:
		return a.Object, true
	case OldObjectVarName:
		return a.OldObject, true
	case ParamsVarName:
		return a.Params, true
	case RequestVarName:
		return a.Request, true
	case NamespaceVarName:
		return a.Namespace, true
	case VariableVarName:
		return a.Variables, true
	default:
		return nil, false
	}
}

// Parent returns nil — no parent activation chain.
// Implements interpreter.Activation.
func (a *TestActivation) Parent() interpreter.Activation {
	return nil
}
