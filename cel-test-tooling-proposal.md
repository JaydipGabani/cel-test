<!-- KEP Metadata
title: CEL Test Tooling for Kubernetes
kep-number: NNNN
status: provisional
authors:
  - TBD
owning-sig: sig-api-machinery
related-issues:
  - https://github.com/kubernetes/kubernetes/issues/135351
  - https://github.com/kubernetes/kubernetes/issues/130570
creation-date: 2026-02-01
-->

# KEP-NNNN: CEL Test Tooling for Kubernetes

## Table of Contents

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [CEL Features and Environments](#cel-features-and-environments)
  - [How CEL Environments Are Built Today](#how-cel-environments-are-built-today)
  - [Package Location](#package-location)
  - [Design Principles](#design-principles)
  - [Testing Levels](#testing-levels)
  - [Declarative Test Format: *_test.cel](#declarative-test-format-_testcel)
  - [Core API](#core-api)
  - [Framework Adaptation: Preamble Variables and Runner Variants](#framework-adaptation-preamble-variables-and-runner-variants)
  - [Architecture: Shared Base, Feature Extensions, Preamble Variables](#architecture-shared-base-feature-extensions-preamble-variables)
  - [Error Handling and Concurrency](#error-handling-and-concurrency)
  - [Usage Examples](#usage-examples)
  - [Comparison with Existing Tools](#comparison-with-existing-tools)
  - [CLI Tool Design](#cli-tool-design)
  - [Implementation Plan](#implementation-plan)
  - [Graduation Criteria](#graduation-criteria)
  - [Open Questions for sig-api-machinery](#open-questions-for-sig-api-machinery)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Implementation History](#implementation-history)
<!-- /toc -->

## Summary

Kubernetes uses CEL (Common Expression Language) across 7 use cases spanning 5 distinct CEL environments, each with its own variables, and custom types. Currently there is **no official testing utility** that sets up the correct CEL environment for any of these features. This KEP proposes:

1. **A Go testing package** (`k8s.io/apiserver/pkg/cel/testing/celtest`) that wraps the existing K8s CEL infrastructure into a simple API for evaluating CEL expressions in Go tests — using the real K8s CEL environment, not a custom one.
2. **A standalone CLI tool** (`kubernetes-sigs/cel-test`) that discovers and runs declarative `*_test.cel` YAML test files so that policy authors who don't write Go can test CEL expressions locally.

## Motivation

Users writing CEL expressions for Kubernetes must currently either deploy to a cluster (slow, no shift-left), use Gatekeeper-specific tooling like `gator` (covers Gatekeeper policies only, not other K8s CEL features), or roll their own evaluator (inevitably incomplete). The building blocks exist inside `k8s.io/apiserver`, they just aren't packaged for external testing use.

### Goals

- Provide a single Go package that can evaluate CEL expressions in the K8s CEL environment for all 7 CEL features (VAP, MAP, CRD validation, matchConditions, DRA, AuthN, AuthZ).
- Support **per-expression**, **per-variable**, **whole-policy**, and **compile-check** testing levels.
- Enable **shift-left** testing: pure Go, zero cluster dependency, sub-second test runs.
- Support **feature-specific presets** (future) so users get the correct variables, types, and libraries for each K8s CEL context. In the MVP, the correct environment is selected by calling the appropriate `Eval*` method.
- Support **K8s version pinning** for reproducible tests across Kubernetes releases.
- Support **framework preamble variables** (e.g., Gatekeeper's `anyObject`/`params`) so policy frameworks can inject their runtime variables into the test environment.
- Provide a **declarative test format** (`*_test.cel`) for YAML-based test cases colocated with policy source.
- Provide a **standalone CLI tool** (`celtest`) so that policy authors can test CEL expressions without writing Go.

### Non-Goals

- **Replacing gator or other integration test tools.** This package tests CEL expressions in isolation; gator tests full ConstraintTemplate + suite.yaml end-to-end.
- **Runtime evaluation in production.** This is a `testing`-only package, not an admission controller or policy engine.
- **Replacing `kubectl-validate` or schema validation.** This package evaluates CEL expressions, not Kubernetes resource schemas.

## Proposal

### User Stories

**Story 1: VAP Policy Author**
As a ValidatingAdmissionPolicy author, I want to test my CEL validation expressions locally in Go tests so that I can iterate quickly without deploying to a cluster.

**Story 2: Gatekeeper Policy Developer**
As a Gatekeeper library contributor, I want to test individual CEL variables (e.g., `containers`, `badContainers`) in isolation so that I can debug policy logic at the expression level, not just pass/fail at the whole-policy level. I need the test environment to include Gatekeeper's injected preamble variables (`anyObject`, `params`).

**Story 3: DRA Driver Developer**
As a Dynamic Resource Allocation driver author, I want to test my device selector CEL expressions with the correct DRA environment (custom `Semver` type, map-with-default attribute lookups) so that I can validate selectors before deploying drivers.

**Story 4: K8s AuthN/AuthZ Config Author**
As a cluster administrator configuring OIDC claims mapping or authorization webhook match conditions, I want to verify my CEL expressions compile and evaluate correctly against sample JWT claims and SubjectAccessReview specs.

**Story 5: CRD Validation Rule Author**
As a CRD developer adding `x-kubernetes-validations` rules to my custom resource, I want to test expressions like `self.spec.replicas <= self.spec.maxReplicas` locally so that I can catch type errors and logic bugs before applying the CRD to a cluster. CRD validation rules are the most widely-used CEL feature in Kubernetes — every CRD with validation rules needs this.

**Story 6: Policy Author (CLI)**
As a security team lead who writes VAP policies in YAML but not Go, I want to run `celtest run src/...` in CI to validate all my CEL expressions without maintaining Go test files or understanding Go tooling.

### Risks and Mitigations

**Risk: Dependency weight of `k8s.io/apiserver`.**
The package lives in `k8s.io/apiserver`, which is a large module. Users who only need CEL testing pull in the full apiserver dependency tree.
*Mitigation:* This is a `testing`-only package—it is imported only in `*_test.go` files, so the dependency does not affect production binaries. If dependency weight becomes a concern, the package can be extracted to `sigs.k8s.io/cel-testing` in a later phase.

**Risk: Environment drift between test package and production.**
If the test package constructs its CEL environment differently from the real admission/CRD/DRA code paths, tests could pass but production could fail.
*Mitigation:* The evaluator reuses upstream K8s code at two levels:

| Layer | Upstream code reused | Identical to production? |
|---|---|---|
| **Base environment** | `environment.MustBaseEnvSet(ver)` | ✅ Yes — same versioned libraries |
| **Variable composition** | `admissioncel.NewCompositedCompiler()` ([composition.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go)), `CompileAndStoreVariables()` | ✅ Yes — same compiler, same type propagation |
| **Typed declarations** | `admissioncel.BuildRequestType()`, `BuildNamespaceType()` (in [compile.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go)) | ✅ Yes — same typed DeclTypes |
| **Namespace filtering** | `admissioncel.CreateNamespaceObject()` (in [condition.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go)) | ✅ Yes — same safe-field filter |
| **Evaluation loop** | Custom loop mirroring `ForInput()` | ⚠️ Equivalent — see below |

The evaluation loop is the one piece that is reimplemented rather than called directly. The upstream `ForInput()` method (in [condition.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go) and [composition.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go)) requires `admission.VersionedAttributes` — a type tied to the full K8s admission pipeline — which cannot be cleanly constructed from unstructured test input. The activation bindings are implemented in [activation.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go) (`evaluationActivation` struct). The custom loop follows the same preamble → variables → validations ordering and uses the same `StoredExpressionsEnv` for evaluation.

**This is why the package should live in `k8s.io/apiserver`**: inside the K8s tree, it can either call `ForInput()` directly (by constructing `VersionedAttributes` from internal types) or expose a simpler evaluation method that accepts unstructured inputs. The prototype proves the API design; the upstream implementation eliminates the evaluation gap.

**Risk: DRA lives in a separate module (`k8s.io/dynamic-resource-allocation`).**
The DRA CEL environment cannot be reached from `k8s.io/apiserver` without a cross-module dependency.
*Mitigation:* Phase 1-2 covers only apiserver-resident features (VAP, MAP, CRD, matchConditions). DRA support (Phase 3) either adds a dependency or lives in `k8s.io/dynamic-resource-allocation/cel/testing`. A thin orchestrator in `sigs.k8s.io/cel-testing` can unify them.

**Risk: Version pinning backward compatibility.**
If a user pins `WithVersion(1, 28)` but the package ships with K8s 1.33, the base environment code may have changed behavior for how it handles older version gating.
*Mitigation:* `environment.MustBaseEnvSet(version)` is designed specifically for this. It accepts a compatibility version and only enables libraries and features available at that version. This is the same mechanism the K8s API server uses for rollback safety (`DefaultCompatibilityVersion()`). The test package delegates entirely to this function, so version pinning inherits the same backward compatibility guarantees as the API server itself.

## Design Details

### CEL Features and Environments

Kubernetes uses CEL across **7 use cases** spanning **5 distinct CEL environments** (VAP/MAP/matchConditions share the same admission env, AuthN has 2 sub-envs). Each has its own variables and custom types:

| # | Feature | Package | Variables | Custom Types/Libraries | Env |
|---|---------|---------|-----------|----------------------|---|
| 1 | **ValidatingAdmissionPolicy (VAP)** | `k8s.io/apiserver/pkg/admission/plugin/cel` | `object`, `oldObject`, `request`, `params`, `namespaceObject`, `authorizer`, `variables` | AdmissionRequest, Namespace, Authorizer types | Admission |
| 2 | **MutatingAdmissionPolicy (MAP)** | same as VAP + `mutation.go` | same as VAP | `library.JSONPatch` (adds `jsonpatch.escapeKey()`), `mutation.DynamicTypeResolver` | Admission (extended) |
| 3 | **CRD Validation Rules** | `k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel` | `self`, `oldSelf` | Schema-derived types from OpenAPI | CRD |
| 4 | **Webhook matchConditions** | `k8s.io/apiserver/pkg/admission/plugin/cel` (same package as VAP via `ConditionCompiler`) | `object`, `oldObject`, `request`, `params` | AdmissionRequest type | Admission (subset) |
| 5 | **Dynamic Resource Allocation (DRA)** | `k8s.io/dynamic-resource-allocation/cel` | `device` (with `.driver`, `.attributes`, `.capacity`) | `DRADevice` typed object, custom map-with-default. Note: `Semver` type is now in the base env since 1.33 via `library.SemverLib` | DRA |
| 6 | **Authentication (AuthN)** | `k8s.io/apiserver/pkg/authentication/cel` | `claims` OR `user` (two separate envs via `mustBuildEnvs()`) | `kubernetes.UserInfo` typed object (username, uid, groups, extra), claims as `map(string, any)` | AuthN (×2) |
| 7 | **Authorization (AuthZ)** | `k8s.io/apiserver/pkg/authorization/cel` | `request` (SubjectAccessReviewSpec) | `kubernetes.SubjectAccessReviewSpec`, `kubernetes.ResourceAttributes` (with optional `fieldSelector`/`labelSelector` behind `AuthorizeWithSelectors` feature gate), `kubernetes.NonResourceAttributes`, `kubernetes.SelectorRequirement` | AuthZ |

### How CEL Environments Are Built Today

All 7 features follow the same pattern:
```
MustBaseEnvSet(ver) → .Extend(feature-specific variables + types) → .Env(StoredExpressions) → Compile → Program → Eval
```

| Feature | Call site |
|---|---|
| VAP | `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go` |
| Webhook matchConditions | `staging/src/k8s.io/apiserver/pkg/admission/plugin/webhook/generic/webhook.go` |
| CRD Validation | `staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel/validation.go` |
| DRA | `staging/src/k8s.io/dynamic-resource-allocation/cel/compile.go` |
| AuthN, AuthZ | `staging/src/k8s.io/apiserver/pkg/apis/apiserver/validation/validation.go` |

The building blocks exist in `k8s.io/apiserver` — they just aren't packaged for external testing use.

### Package Location

This design has two deliverables that follow established Kubernetes contribution patterns:

**1. Go library → `k8s.io/apiserver/pkg/cel/testing/celtest`**

The core Go API (`NewEvaluator`, `EvalExpression`, `CompileCheck`, `WithPreambleVariables`) lives in `k8s.io/apiserver` staging. This follows the precedent of other testing packages in the K8s tree:

| Existing package | What it provides |
|---|---|
| `k8s.io/client-go/testing` | Fake client, reactors for unit testing API interactions |
| `k8s.io/apiserver/pkg/admission/testing` | Helpers for admission webhook tests |
| `k8s.io/apiserver/pkg/storage/testing` | Store test suite functions for etcd |

The library ships with K8s releases, stays close to the CEL environment source code it wraps, and can use internal types directly. If DRA support (Phase 3) requires cross-module dependencies, the DRA-specific evaluator will live in `k8s.io/dynamic-resource-allocation/cel/testing`.

**2. CLI tool → `kubernetes-sigs/cel-test` (Phase 1b)**

A standalone CLI that discovers and runs `*_test.cel` files. The CLI is a thin wrapper (~50 LOC `main()`) over the Go library. It lives in a separate `kubernetes-sigs/cel-test` repo with its own release cycle, following the precedent of `kubernetes-sigs/kubectl-validate` and `kubernetes-sigs/kubetest2`.

Commands:
- `celtest run src/...` — discover and run all `*_test.cel` files
- `celtest compile src/policy/src.cel` — compile-check all expressions
- `celtest eval 'has(object.metadata.labels)' --input pod.yaml` — evaluate a single expression

### Design Principles

1. **Use the real K8s CEL environment** — no custom env, no drift
2. **Feature-specific evaluation methods** — different CEL contexts (VAP, CRD, matchConditions) have different available variables and libraries, selected by which `Eval*` method you call
3. **Per-expression testing** — test individual CEL expressions and variables, not just whole policies
4. **Declarative test files** — `*_test.cel` YAML files colocated with policy source, auto-discovered
5. **Table-test friendly** — designed for Go `testing.T` with `[]struct` patterns (Go API) or YAML test cases (declarative)
6. **Versioned** — pin to a K8s version for reproducible tests
7. **Zero cluster dependency** — pure Go, no informers, no API server

### Testing Levels

The tooling supports three levels of testing, each addressing a different need:

| Level | API | What it tests | Who needs it |
|---|---|---|---|
| **Per-expression** | `EvalExpression(expr, input, vars)` | Does a single CEL expression return the expected value? | CEL expression *authors* |
| **Per-variable** | `variable:` in `*_test.cel` | Does a specific policy variable compute correctly for given input? | Policy *developers* |
| **Whole-policy** | `EvalAdmission(policy, input)` or `expect.allowed:` in `*_test.cel` | Does the complete policy allow/deny correctly? | Policy *consumers* |
| **Compile-check** | `CompileCheck(expr)` | Does the expression compile in the real K8s env? | CI/linting |

**Per-expression and per-variable testing is the primary value add** — gator and integration tests already cover whole-policy testing. What's missing in the ecosystem is the ability to test individual CEL expressions in isolation with the correct K8s environment.

> **Compile-check shorthand:** `err := eval.CompileCheck(\`object.metadata.labels.exists(k, k == "app")\`)` — returns a descriptive `error` if the expression fails to compile, `nil` otherwise.

### Declarative Test Format: `*_test.cel`

Policy source files use the `*.cel` extension. Test files use the `*_test.cel` suffix convention — matching Go's `*_test.go` and OPA's `*_test.rego` patterns. A test file is paired with its policy file by base name: `foo.cel` is tested by `foo_test.cel` in the same directory.

**File naming convention:**

| File | Role | Example |
|---|---|---|
| `*.cel` | Policy source (variables + validations) | `privileged.cel` |
| `*_test.cel` | Test file for the same-named policy | `privileged_test.cel` |

**Discovery rules:**
1. Walk the directory tree for `*_test.cel` files
2. For each `foo_test.cel`, look for `foo.cel` in the same directory
3. If `foo.cel` exists → load it as the policy under test
4. If `foo.cel` doesn't exist → the test file must have `mode: expression` (standalone, no policy needed)

A single Go test runner discovers and runs all `*_test.cel` files automatically. The `celtest` CLI (Phase 1b) also discovers and runs these files directly.

```yaml
# src/pod-security-policy/privileged-containers/src_test.cel
tests:
# ---- Variable-level tests (per-expression) ----
- name: "containers extracts spec.containers"
  variable: containers          # test this specific variable
  object:                       # input
    spec:
      containers:
      - name: nginx
        image: nginx
  expect:
    size: 1                     # assert the result has 1 element

- name: "badContainers finds privileged container"
  variable: badContainers
  object:
    spec:
      containers:
      - name: nginx
        image: nginx
        securityContext:
          privileged: true
  expect:
    size: 1
    contains: "Privileged container is not allowed: nginx"

- name: "badContainers skips exempt image"
  variable: badContainers
  params:
    exemptImages: ["nginx"]
  object:
    spec:
      containers:
      - name: nginx
        image: nginx
        securityContext:
          privileged: true
  expect:
    value: []                   # exact value match

# ---- Whole-policy tests ----
- name: "denies privileged container"
  object:
    metadata:
      name: test-pod
    spec:
      containers:
      - name: nginx
        image: nginx
        securityContext:
          privileged: true
  expect:
    allowed: false
    messageContains: "Privileged container is not allowed: nginx"

- name: "allows on UPDATE regardless"
  request:
    operation: UPDATE
  object:
    metadata:
      name: test-pod
    spec:
      containers:
      - name: nginx
        image: nginx
        securityContext:
          privileged: true
  expect:
    allowed: true

# ---- Expression tests (arbitrary CEL) ----
- name: "string(map) fails at runtime"
  expression: 'string(object.metadata.labels)'
  object:
    metadata:
      labels:
        app: web
  expect:
    error: true
    errorContains: "no such overload"
```

**Test runner — replaces hand-written Go test functions:**
```go
// One line discovers and runs all *_test.cel files
func TestCELPolicies(t *testing.T) {
    celtest.DiscoverAndRunTests(t, "../../src")
}
```

**Assertion options:**

| Field | Applies to | Description |
|---|---|---|
| `expect.value` | variable, expression | Exact value match |
| `expect.size` | variable, expression | List/map/string length |
| `expect.contains` | variable, expression | String/list containment |
| `expect.allowed` | whole-policy | Validation pass/fail |
| `expect.messageContains` | whole-policy | Violation message check |
| `expect.error` | all | Expect an evaluation error |
| `expect.errorContains` | all | Error message substring |

#### Formal YAML Schema

The complete schema for `*_test.cel` files:

```yaml
# TestFile schema
mode: string          # Optional. "policy" (default) or "expression".
                      # "policy": requires companion .cel file (foo_test.cel → foo.cel).
                      #   Supports variable, expression, and whole-policy tests.
                      # "expression": self-contained, each test evaluates its expression field directly.
                      #   - variable: tests are FORBIDDEN (no policy variables exist).
                      #   - Whole-policy tests (expect.allowed) are FORBIDDEN.
                      #   - No companion .cel file is needed.
                      #   - object, oldObject, params, request inputs are all available.
tests:                # Required. Array of TestCase, minimum 1.
  - name: string      # Required. Unique within file. Used as Go subtest name.

    # --- Test type (mutually exclusive, pick at most one) ---
    variable: string   # Test a specific named variable from the policy. Only in mode: policy.
    expression: string # Test an arbitrary CEL expression. Works in both modes.
    # (neither):       # Test the whole policy (allowed/denied). Only in mode: policy.

    # --- Input ---
    object: map        # Optional. The object being admitted/validated.
    oldObject: map     # Optional. Previous version (for UPDATE/DELETE operations).
    params: map        # Optional. Policy parameters.
                       # In Gatekeeper mode: automatically wrapped in {spec: {parameters: <params>}}.
                       # In raw/vanilla mode: passed as-is to the CEL environment.
    request: map       # Optional. Request metadata. Default: {operation: "CREATE"}.

    # --- Assertions ---
    expect:            # Required.
      # For variable and expression tests:
      value: any       # Optional. Exact value match (with numeric normalization).
      size: int        # Optional. Length of list, map, or string result.
      contains: string # Optional. Substring match on string representation of result,
                       #   or element containment for lists.

      # For whole-policy tests (mode: policy only):
      allowed: bool    # Optional. Whether the policy should allow the input.
      messageContains: string  # Optional. Substring match on joined violation messages.

      # For error testing (all test types):
      error: bool      # Optional. If true, expect evaluation to produce an error.
      errorContains: string  # Optional. Substring match on the error message.
```

**Validation rules:**
- `variable` and `expression` are mutually exclusive. If both are set, the runner rejects the test case.
- In `mode: expression`, `variable` tests and `expect.allowed` tests are rejected at parse time.
- `expect.allowed` and `expect.messageContains` are only valid for whole-policy tests (neither `variable` nor `expression` set).
- `expect.value`, `expect.size`, and `expect.contains` are only valid for `variable` or `expression` tests.
- Numeric comparison normalizes int/int64/float64 for cross-type equality (CEL returns int64, YAML parses as int).
- Empty slices: `nil` and `[]` are treated as equal in `expect.value` matching.

#### Expression Mode

For standalone CEL expressions — such as CRD `x-kubernetes-validations` rules, DRA device selectors, or AuthZ match conditions — that don't use the variables/validations policy structure, set `mode: expression` at the top of the test file. In expression mode, each test case evaluates its `expression` field directly against the input. No companion `.cel` policy file is needed.

```yaml
# crd-validation/replicas_test.cel
mode: expression

tests:
- name: "replicas within limit"
  expression: "object.spec.replicas <= object.spec.maxReplicas"
  object:
    spec:
      replicas: 3
      maxReplicas: 5
  expect:
    value: true

- name: "replicas exceed limit"
  expression: "object.spec.replicas <= object.spec.maxReplicas"
  object:
    spec:
      replicas: 10
      maxReplicas: 5
  expect:
    value: false

- name: "name format validation"
  expression: 'object.metadata.name.matches("^[a-z][a-z0-9-]*$")'
  object:
    metadata:
      name: "Valid-Name"
  expect:
    value: false
```

When `mode: expression` is set, `DiscoverAndRunTests` does not require a companion `.cel` policy file — the test file is self-contained.

### Core API

```go
package celtest

import (
    "k8s.io/apimachinery/pkg/util/version"
)

// Evaluator compiles and evaluates CEL expressions using the real K8s environment.
type Evaluator struct {
    envSet           *environment.EnvSet
    version          *version.Version
    preambleVars     []Variable  // framework-injected variables evaluated before policy variables
}

// Option configures an Evaluator.
type Option func(*Evaluator)

// WithVersion sets the K8s compatibility version (default: latest).
// The base environment is versioned — some libraries are only available from certain K8s versions:
//   - 1.0+: URLs, Regex, Lists (K8s custom), Strings (v0 until 1.29)
//   - 1.27+: Authz library
//   - 1.28+: Quantity, OptionalTypes, cross-type numeric comparisons
//   - 1.29+: AST validators, Strings (v2 upgrade), Sets
//   - 1.30+: IP, CIDR
//   - 1.31+: Format, AuthzSelectors (feature-gated)
//   - 1.32+: TwoVarComprehensions
//   - 1.33+: Semver
//   - 1.34+: Lists ext (cel-go v3)
//
// Note: JSONPatch is NOT in the base environment. It exists as library.JSONPatch()
// in k8s.io/apiserver/pkg/cel/library but is only added by the MAP (mutation)
// feature-specific extension, not by MustBaseEnvSet.
func WithVersion(major, minor uint) Option { ... }

// NewEvaluator creates a CEL evaluator.
// Internally calls environment.MustBaseEnvSet() and extends with admission-style
// variables and types — the exact same code path as the K8s API server.
// The correct CEL context is selected by which Eval* method you call.
func NewEvaluator(opts ...Option) (*Evaluator, error) { ... }

// ========== VAP / MAP / matchConditions ==========

// AdmissionInput represents input for admission-style CEL evaluation.
// Used by EvalAdmission for VAP, MAP, and matchConditions.
//
// Fields map to CEL variables following the same conventions as the K8s API server
// (see k8s.io/apiserver/pkg/admission/plugin/cel/activation.go evaluationActivation):
//
//   | AdmissionInput field | CEL variable            | CEL compile-time type              | Default if nil                      |
//   |----------------------|-------------------------|------------------------------------|-------------------------------------|
//   | Object               | object                  | DynType                            | nil (null in CEL)                   |
//   | OldObject            | oldObject               | DynType                            | nil (null in CEL)                   |
//   | Params               | params                  | DynType                            | nil (null in CEL)                   |
//   | Request              | request                 | kubernetes.AdmissionRequest        | Synthesized: {operation: "CREATE"}  |
//   | Namespace            | namespaceObject         | kubernetes.Namespace               | nil (null in CEL)                   |
//
// All map[string]interface{} values are the unstructured representation of the K8s object,
// matching the runtime behavior of the API server which converts typed objects to
// unstructured before CEL evaluation.
//
// Request is typed as *admissionv1.AdmissionRequest and internally converted to
// unstructured (map[string]interface{}) for CEL binding — matching the API server's
// CreateAdmissionRequest → convertObjectToUnstructured path. If nil, a minimal
// request with only Operation set is synthesized.
//
// Namespace is typed as *corev1.Namespace and internally filtered to safe fields
// (matching the API server's CreateNamespaceObject) then converted to unstructured.
// If nil, the namespaceObject CEL variable resolves to null.
//
// authorizer and authorizer.requestResource are not yet supported (Phase 5).
type AdmissionInput struct {
    Object    map[string]interface{}       // → CEL `object` (DynType)
    OldObject map[string]interface{}       // → CEL `oldObject` (DynType)
    Params    map[string]interface{}       // → CEL `params` (DynType)
    Request   *admissionv1.AdmissionRequest // → CEL `request` (kubernetes.AdmissionRequest)
    Namespace *corev1.Namespace             // → CEL `namespaceObject` (kubernetes.Namespace)
}

// VAPPolicy represents a parsed VAP-style policy (variables + validations).
type VAPPolicy struct {
    Variables   []Variable
    Validations []Validation
}

type Variable struct {
    Name       string
    Expression string
}

type Validation struct {
    Expression        string
    Message           string  // static message on failure
    MessageExpression string  // CEL expression for dynamic message
}

// AdmissionResult holds the evaluation outcome.
type AdmissionResult struct {
    Allowed    bool
    Violations []Violation
    Cost       int64  // total CEL evaluation cost
}

type Violation struct {
    Expression string
    Message    string
    Error      error
}

> Cost tracking uses the same `cel.OptTrackCost` and `PerCallLimit` as the K8s API server. The `Cost` field reports the total CEL evaluation cost in K8s cost units. This helps policy authors detect expressions that may exceed the runtime cost budget before deploying to a cluster. The `AdmissionResult.Cost` field and `WithCostLimit` option are defined in Phase 1a; cost tracking implementation ships in Phase 1a with the default of no limit (cost is reported but never causes failures).

// WithCostLimit sets a CEL cost budget for evaluation. If an expression exceeds
// this limit, Eval* methods return an error. Default: no limit (cost is tracked
// and reported in AdmissionResult.Cost but never causes failures).
//
// Use celtest.PerCallLimit for the K8s production limit:
//   eval, _ := celtest.NewEvaluator(celtest.WithCostLimit(celtest.PerCallLimit))
//
// The K8s production limit is defined in k8s.io/apiserver/pkg/apis/cel/config.go.
func WithCostLimit(limit int64) Option { ... }

// PerCallLimit is the K8s API server's per-expression cost limit.
// Expressions exceeding this limit are rejected at runtime.
const PerCallLimit = cel.PerCallLimit  // currently 1,000,000 (from k8s.io/apiserver/pkg/apis/cel)

// EvalAdmission evaluates a VAP/MAP/matchCondition policy against admission input.
func (e *Evaluator) EvalAdmission(policy *VAPPolicy, input *AdmissionInput) (*AdmissionResult, error) { ... }

// ========== Feature-Specific Evaluators (Phase 2-4, designed when implemented) ==========
//
// The following features will get dedicated Eval* methods in later phases:
//   - Phase 2: EvalCRDRule(expression, *CRDInput) — CRD x-kubernetes-validations with self/oldSelf
//   - Phase 3: EvalDRASelector(expression, *DRAInput) — DRA device selectors with typed device object
//   - Phase 4: EvalAuthNClaims(expression, *AuthNClaimsInput) — OIDC claims mapping
//              EvalAuthNUser(expression, *AuthNUserInput) — user info mapping  
//              EvalAuthZ(expression, *AuthZInput) — authorization match conditions
//
// These APIs are not defined here to avoid speculative design. They will be designed
// during their respective implementation phases with input from the teams that own
// those features (sig-auth, DRA working group).
//
// In the meantime, EvalExpression() provides basic testing for any CEL expression
// using the admission-style environment with DynType variables.

// ========== Common / Cross-Feature ==========

// EvalExpression evaluates a single CEL expression in the evaluator's environment.
// Uses the same AdmissionInput as EvalAdmission — all CEL variables (object, oldObject,
// params, request, namespaceObject) are available. extraVars allows injecting additional
// variables into the activation (e.g., for testing sub-expressions that reference
// computed variables).
func (e *Evaluator) EvalExpression(expr string, input *AdmissionInput, extraVars map[string]interface{}) (interface{}, error) { ... }

// CompileCheck validates that a CEL expression compiles without errors.
// Uses the version-gated environment (NewExpressionsEnv) so that WithVersion
// correctly restricts which libraries are available — e.g., WithVersion(1, 28)
// makes ip() fail to compile because it was introduced in 1.31.
// Evaluation methods (EvalAdmission, EvalExpression) use StoredExpressionsEnv
// which includes all libraries regardless of version, matching the API server's
// behavior for already-stored expressions.
// Returns a descriptive error including the CEL compiler's error message and position, or nil if valid.
func (e *Evaluator) CompileCheck(expr string) error { ... }

// ParseVAPPolicy parses a VAP policy from YAML (the variables/validations format).
func ParseVAPPolicy(yamlContent string) (*VAPPolicy, error) { ... }

// ParseVAPPolicyFile reads and parses a VAP policy YAML file.
func ParseVAPPolicyFile(path string) (*VAPPolicy, error) { ... }

// ========== Declarative Test Runner ==========

// DiscoverAndRunTests walks srcRoot for *_test.cel files and runs them.
// Uses Gatekeeper preamble variables (anyObject, params) and parameter wrapping.
// For vanilla VAP / Kyverno / standalone expressions, use DiscoverAndRunTestsRaw.
func DiscoverAndRunTests(t *testing.T, srcRoot string) { ... }

// DiscoverAndRunTestsRaw walks srcRoot for *_test.cel files and runs them
// without preamble variables or parameter wrapping.
func DiscoverAndRunTestsRaw(t *testing.T, srcRoot string) { ... }
```

### Framework Adaptation: Preamble Variables and Runner Variants

Policy frameworks like Gatekeeper and Kyverno inject runtime-computed variables and transform parameter structures before CEL evaluation. The test tooling must replicate this to produce accurate results. There are two adaptation layers:

**1. Preamble Variables** — CEL expressions evaluated before the policy's own variables, injecting framework-specific bindings. Configured via `WithPreambleVariables`.

**2. Parameter Wrapping** — Gatekeeper wraps user parameters inside a constraint CRD structure (`{spec: {parameters: <userParams>}}`), so the preamble `params` expression can unwrap them. Vanilla VAP and Kyverno pass parameters directly.

The declarative test runner provides two variants to handle these differences:

| Runner | Preamble | Params | Use case |
|---|---|---|---|
| `DiscoverAndRunTests(t, root)` | Gatekeeper (`anyObject`, `params`) | Wrapped in `{spec: {parameters: ...}}` | gatekeeper-library `src/` |
| `DiscoverAndRunTestsRaw(t, root)` | None | Passed as-is | Vanilla VAP, Kyverno, standalone expressions |

For custom frameworks, use `RunTestFileWithEvaluator` with a custom evaluator that has the framework's preamble variables set via `WithPreambleVariables`.

```go
// WithPreambleVariables registers CEL variable expressions that are evaluated
// BEFORE the policy's own variables.
//
// Preamble variables are real CEL expressions compiled and evaluated in the
// same K8s CEL environment. They can reference the base admission variables
// (object, oldObject, params, request) and each other (in order).
//
// When EvalAdmission runs, the evaluation order is:
//   1. Preamble variables (anyObject, params)         ← from WithPreambleVariables
//   2. Policy variables (containers, badContainers)   ← from VAPPolicy.Variables (src.cel)
//   3. Validation expressions                         ← from VAPPolicy.Validations (src.cel)
func WithPreambleVariables(vars ...Variable) Option { ... }
```

### Architecture: Shared Base, Feature Extensions, Preamble Variables

```
┌──────────────────────────────────────────────────────────┐
│              environment.MustBaseEnvSet(ver)               │
│  URLs, Regex, Lists, Strings, Sets, Quantity, Authz       │
│  IP, CIDR, Format, AuthzSelectors, Semver                 │
│  OptionalTypes, TwoVarComprehensions                      │
│  (versioned by K8s release: 1.0 → 1.34+)                 │
│  Note: JSONPatch is NOT in base — added by MAP extension  │
└────────────────────────┬─────────────────────────────────┘
                         │ .Extend()
        ┌───────┬────────┼───────┬───────┬───────┬───────┐
        ▼       ▼        ▼       ▼       ▼       ▼       ▼
      VAP     MAP      CRD     Match   DRA    AuthN   AuthZ
   object   +jsonpat  self    object  device  claims  request
   oldObj   ch escape  oldSelf  oldObj  +bind   user   +field/
   request  +dyntype  (typed)  request +map          label
   params                      params  default       selector
   ns/authz
   variables
        │
        │ WithPreambleVariables (optional, per-framework)
        ▼
  ┌──────────────────────────────────────┐
  │ Gatekeeper:                          │
  │   anyObject = object ?? oldObject    │
  │   params = params.spec.parameters    │
  ├──────────────────────────────────────┤
  │ Kyverno (hypothetical):              │
  │   element = ...                      │
  │   elementIndex = ...                 │
  ├──────────────────────────────────────┤
  │ Custom framework:                    │
  │   myVar = <any CEL expression>       │
  └──────────────────┬───────────────────┘
                     │
                     ▼
  ┌──────────────────────────────────────┐
  │ Policy variables (from src.cel):     │
  │   containers = variables.anyObject...│
  │   badContainers = ...                │
  └──────────────────┬───────────────────┘
                     │
                     ▼
  ┌──────────────────────────────────────┐
  │ Validations (from src.cel):          │
  │   variables.isUpdate || size(...)==0  │
  └──────────────────────────────────────┘
```

### Usage Examples

#### Testing a Policy File with Preamble Variables (Gatekeeper Library)

Gatekeeper injects two runtime variables (`anyObject`, `params`) that are **not** in `src.cel`.
Use `WithPreambleVariables` to replicate this:

```go
// Reusable Gatekeeper evaluator — create once, use for all policies
func newGatekeeperEvaluator() (*celtest.Evaluator, error) {
    return celtest.NewEvaluator(
        celtest.WithPreambleVariables(
            // Gatekeeper's BindObjectV1Beta1: unified object access
            celtest.Variable{
                Name:       "anyObject",
                Expression: `has(request.operation) && request.operation == "DELETE" && object == null ? oldObject : object`,
            },
            // Gatekeeper's BindParamsV1Beta1: unwrap constraint CRD parameters
            celtest.Variable{
                Name:       "params",
                Expression: `!has(params.spec) ? null : !has(params.spec.parameters) ? null: params.spec.parameters`,
            },
        ),
    )
}

func TestPrivilegedContainers(t *testing.T) {
    eval, err := newGatekeeperEvaluator()
    if err != nil {
        t.Fatal(err)
    }
    // src.cel only contains user variables (containers, badContainers, etc.)
    // The preamble variables (anyObject, params) are evaluated first automatically
    policy, err := celtest.ParseVAPPolicyFile("src/pod-security-policy/privileged-containers/src.cel")
    if err != nil {
        t.Fatal(err)
    }

    result, err := eval.EvalAdmission(policy, &celtest.AdmissionInput{
        Object: map[string]interface{}{
            "metadata": map[string]interface{}{"name": "test-pod"},
            "spec": map[string]interface{}{
                "containers": []interface{}{
                    map[string]interface{}{
                        "name": "nginx", "image": "nginx",
                        "securityContext": map[string]interface{}{"privileged": true},
                    },
                },
            },
        },
        // Pass the RAW constraint object — params preamble variable unwraps it
        Params: map[string]interface{}{
            "spec": map[string]interface{}{
                "parameters": map[string]interface{}{},
            },
        },
    })
    if err != nil {
        t.Fatal(err)
    }
    if result.Allowed {
        t.Error("expected denial for privileged container")
    }
}
```

#### Per-Expression Table Test
```go
func TestFilterExpression(t *testing.T) {
    eval, _ := celtest.NewEvaluator()
    tests := []struct {
        name   string
        expr   string
        object map[string]interface{}
        want   interface{}
    }{
        {
            name:   "finds privileged containers",
            expr:   `object.spec.containers.filter(c, has(c.securityContext) && c.securityContext.privileged)`,
            object: map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{
                map[string]interface{}{"name": "safe", "securityContext": map[string]interface{}{"privileged": false}},
                map[string]interface{}{"name": "bad", "securityContext": map[string]interface{}{"privileged": true}},
            }}},
            want: 1, // expect 1 result
        },
        {
            name:   "has labels check",
            expr:   `has(object.metadata.labels)`,
            object: map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "web"}}},
            want:   true,
        },
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := eval.EvalExpression(tt.expr, &celtest.AdmissionInput{Object: tt.object}, nil)
            if err != nil {
                t.Fatal(err)
            }
            // assert result...
        })
    }
}
```

### Comparison with Existing Tools

| Tool | Env Accuracy | API | Cluster | Scope |
|---|---|---|---|---|
| **This proposal** | ✅ Real K8s env | ✅ Simple Go API | ❌ No | All 7 CEL features |
| gator CLI | ✅ Real K8s env | ⚠️ YAML suite files | ❌ No | Gatekeeper policies only (OPA + CEL templates) |
| kaptest | ⚠️ Third-party | ✅ Simple | ❌ No | VAP only |
| kubectl-validate (#130570) | ✅ Real K8s env | ⚠️ CLI tool | ❌ No | Schema validation |
| Custom `cel-go` (celeval) | ❌ Incomplete env | ✅ Simple Go API | ❌ No | Custom subset |

### CLI Tool Design

The `celtest` CLI (`kubernetes-sigs/cel-test`, Phase 1b) is a thin wrapper over the Go library. This section specifies its commands, flags, output format, and configuration.

#### Commands

##### `celtest run [paths...]`

Discover and run `*_test.cel` files.

```bash
# Run all tests under src/ recursively
celtest run src/

# Run a specific test file
celtest run src/pod-security-policy/privileged-containers/src_test.cel

# Run multiple directories
celtest run src/pod-security-policy/ src/replica-limit/
```

**Flags:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--version` | `string` | latest | K8s compatibility version (e.g., `1.31`). Maps to `WithVersion()`. |
| `--preamble` | `string` | none | Path to preamble config file (see below). |
| `--output` / `-o` | `string` | `text` | Output format: `text`, `json`, `junit`. |
| `--verbose` / `-v` | `bool` | false | Show passing tests and evaluation details. |
| `--fail-fast` | `bool` | false | Stop on first test failure. |
| `--cost-limit` | `int` | 0 (none) | CEL cost budget per expression. Maps to `WithCostLimit()`. |

##### `celtest compile [files...]`

Compile-check all expressions in policy files without evaluating.

```bash
# Compile-check a single policy
celtest compile src/privileged-containers/src.cel

# Compile-check all policies under a directory
celtest compile src/
```

**Flags:**

| Flag | Type | Default | Description |
|---|---|---|---|
| `--version` | `string` | latest | K8s compatibility version. Restricts available libraries. |
| `--feature` | `string` | `admission` | CEL context: `admission`, `crd`, `dra`, `authn-claims`, `authn-user`, `authz`. Determines which variables and types are available. |
| `--output` / `-o` | `string` | `text` | Output format: `text`, `json`. |

##### `celtest eval '<expression>' [flags]`

Evaluate a single CEL expression interactively.

```bash
# Evaluate with object from YAML file
celtest eval 'object.spec.replicas > 3' --object pod.yaml

# Evaluate with inline JSON
celtest eval 'has(object.metadata.labels)' --object '{"metadata":{"labels":{"app":"web"}}}'

# Evaluate with params and request
celtest eval 'object.spec.replicas <= params.maxReplicas' \
  --object deployment.yaml \
  --params '{"maxReplicas": 5}'

# Evaluate with specific K8s version
celtest eval 'semver("1.2.3").isGreaterThan(semver("1.0.0"))' --version 1.33
```

**Flags:**

| Flag | Type | Description |
|---|---|---|
| `--object` | `string` | Path to YAML/JSON file or inline JSON for `object` variable. |
| `--old-object` | `string` | Path/inline for `oldObject` variable. |
| `--params` | `string` | Path/inline for `params` variable. |
| `--request` | `string` | Path/inline for `request` variable. Default: `{"operation":"CREATE"}`. |
| `--version` | `string` | K8s compatibility version. |
| `--feature` | `string` | CEL context (same as `compile`). |

#### Exit Codes

| Code | Meaning |
|---|---|
| `0` | All tests passed / expression compiled / evaluation succeeded |
| `1` | One or more test failures or evaluation errors |
| `2` | Configuration error (bad flags, unparseable YAML, missing files) |
| `3` | CEL compilation error (for `compile` and `eval` commands) |

#### Output Formats

**Text (default):**
```
=== RUN   src/privileged-containers/src_test.cel
--- PASS: containers extracts spec.containers (0.002s)
--- PASS: badContainers finds privileged container (0.001s)
--- FAIL: allows on UPDATE regardless (0.001s)
        expected allowed=true, got allowed=false
=== FAIL: src/privileged-containers/src_test.cel (2 passed, 1 failed)

FAIL	2 files, 5 passed, 1 failed
```

**JSON (`-o json`):**
```json
{
  "files": [
    {
      "path": "src/privileged-containers/src_test.cel",
      "tests": [
        {"name": "containers extracts spec.containers", "passed": true, "duration": "2ms"},
        {"name": "allows on UPDATE regardless", "passed": false, "error": "expected allowed=true, got allowed=false", "duration": "1ms"}
      ]
    }
  ],
  "summary": {"total": 5, "passed": 4, "failed": 1}
}
```

**JUnit XML (`-o junit`):**
Standard JUnit XML format for CI systems (Jenkins, GitHub Actions, etc.).

#### Configuration File (`.celtest.yaml`)

For projects with multiple policies, a `.celtest.yaml` file at the repository root provides defaults:

```yaml
# .celtest.yaml
version: "1.31"          # Default K8s compatibility version
output: text             # Default output format

# Preamble variables for framework-specific testing
preamble:
  variables:
  - name: anyObject
    expression: 'has(request.operation) && request.operation == "DELETE" && object == null ? oldObject : object'
  - name: params
    expression: '!has(params.spec) ? null : !has(params.spec.parameters) ? null : params.spec.parameters'
  # Parameter wrapping (Gatekeeper wraps params in {spec: {parameters: ...}})
  wrapParams: true
```

The `--preamble` flag can also point to this file explicitly:
```bash
celtest run src/ --preamble .celtest.yaml
```

Without `--preamble` or `.celtest.yaml`, the CLI runs in raw mode (equivalent to `DiscoverAndRunTestsRaw`).

#### Example CI Workflow (GitHub Actions)

```yaml
name: CEL Policy Tests
on: [push, pull_request]
jobs:
  cel-test:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: '1.23'
    - run: go install sigs.k8s.io/cel-test/cmd/celtest@latest
    - run: celtest run src/ --version 1.31 -o junit > test-results.xml
    - uses: dorny/test-reporter@v1
      if: always()
      with:
        name: CEL Tests
        path: test-results.xml
        reporter: java-junit
```

#### Error Reporting

Compilation errors include the expression text, error position, and a descriptive message:
```
ERROR: src/policy.cel:3 - variable "maxReplicas"
  | expression: params.maxReplica
  |                    ^^^^^^^^^^^
  | ERROR: undecayed reference to field 'maxReplica' (did you mean 'maxReplicas'?)
```

Evaluation errors show the test name, expression, and runtime error:
```
--- FAIL: string(map) fails at runtime
  expression: string(object.metadata.labels)
  error: no such overload: string(map[string]string)
  expected: error=true ✓
```

### Implementation Plan

#### Phase 1a: Core Go Library (MVP)
- `NewEvaluator` with admission-style environment (covers VAP, MAP compile/expression testing, matchConditions)
- `EvalAdmission` for full VAP policy evaluation (validations returning allowed/denied)
- `EvalExpression` for single expression testing
- `CompileCheck` for compilation validation
- `ParseVAPPolicy` / `ParseVAPPolicyFile` helpers
- `WithVersion` for K8s version pinning
- `WithPreambleVariables` for framework injection (Gatekeeper, Kyverno)
- Declarative `*_test.cel` runner (`DiscoverAndRunTests`)
- **Note**: VAP, MAP, and matchConditions share the same base admission env (`k8s.io/apiserver/pkg/admission/plugin/cel/compile.go`) with different `OptionalVariableDeclarations` flags (`HasParams`, `HasAuthorizer`, `HasPatchTypes`). MAP mutation evaluation (applying JSONPatch mutations and returning patched objects) is out of scope for Phase 1a — `EvalAdmission` evaluates validation expressions only. MAP mutation semantics require `mutation.DynamicTypeResolver` and produce different result types.

#### Phase 1b: CLI Tool (`kubernetes-sigs/cel-test`)
- `celtest run` — discovers and runs `*_test.cel` files (wraps `DiscoverAndRunTests`)
- `celtest compile` — compile-checks all expressions in a policy file
- `celtest eval` — evaluates a single expression against YAML/JSON input
- `--version` flag maps to `WithVersion`
- `--preamble` flag or config file for framework-specific preamble variables
- Installable via `go install sigs.k8s.io/cel-test/cmd/celtest@latest`
- **Note**: The CLI is a thin `main()` wrapper over the Go library. No separate API surface to stabilize.

#### Phase 2: CRD Validation Rules
- `EvalCRDRule` with `self` / `oldSelf` variables
- Schema-aware type checking (optional OpenAPI schema input)
- `EvalCRDRule(expr string, input *CRDInput) (bool, string, error)`
- Transition rule support (expressions referencing `oldSelf`)
- **Note**: CRD env is built in `k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel/compilation.go` using `prepareEnvSet()` which extends base with `ScopedVarName`/`OldScopedVarName` and schema-derived `DeclType`

#### Phase 3: DRA Device Selectors
- `EvalDRASelector` with `device` variable (typed `DRADevice` object)
- `EvalDRASelector` returning `(bool, error)`
- Map-with-default behavior for attribute/capacity lookups (matching `newStringInterfaceMapWithDefault`)
- **Note**: As of K8s 1.33, `library.SemverLib` has been promoted to the base environment (`k8s.io/apiserver/pkg/cel/library/semverlib.go`), so it is available in all CEL contexts without DRA-specific setup. DRA's unique aspects are the `DRADevice` typed object and `ext.Bindings()` from `k8s.io/dynamic-resource-allocation/cel`, plus the custom `mapper` that returns empty maps for unknown attribute domains.

#### Phase 4: Authentication & Authorization
- `EvalAuthNClaims` and `EvalAuthNUser` with dual envs: `claims` (map) and `user` (typed UserInfo)
- `EvalAuthZ` with typed `SubjectAccessReviewSpec` including optional `fieldSelector`/`labelSelector`
- `EvalAuthNClaims`, `EvalAuthNUser`, `EvalAuthZ` methods
- **Note**: AuthN uses two separate `EnvSet` instances (`mustBuildEnvs()` in `authentication/cel/compile.go`) keyed by variable name: `"claims"` env (variable `claims` typed `map(string, any)`) and `"user"` env (variable `user` typed `kubernetes.UserInfo` with fields: username, uid, groups, extra). AuthZ uses a single env with `request` typed as `kubernetes.SubjectAccessReviewSpec`. AuthZ includes AST analysis (`celast.PreOrderVisit`) for field/label selector usage detection, stored in `CompilationResult.UsesFieldSelector`/`UsesLabelSelector`.

#### Phase 5: Advanced Features
- Authorizer mock support for `authorizer` / `authorizer.requestResource` variables
- `testing.T` integration helpers (`celtest.Assert`, `celtest.RequireAllowed`, etc.)
- Benchmark support for expression cost comparison

### Graduation Criteria

#### Alpha (Phase 1a-1b)
- `EvalAdmission` fully functional for VAP/matchConditions validation evaluation (MAP expression testing supported; MAP mutation evaluation deferred)
- `EvalExpression` and `CompileCheck` for expression-level testing
- `WithPreambleVariables` supports framework injection (Gatekeeper validated)
- Package located in `k8s.io/apiserver/pkg/cel/testing/celtest`
- `celtest` CLI available via `kubernetes-sigs/cel-test` with `run`, `compile`, `eval` commands
- Unit tests cover admission evaluation, version pinning, and preamble variable ordering

#### Beta (Phase 1-4)
- All 7 CEL features supported via feature-specific `Eval*` methods
- Declarative `*_test.cel` format with `DiscoverAndRunTests` runner
- CLI supports all feature-specific evaluation modes
- DRA support either in-tree or via `k8s.io/dynamic-resource-allocation/cel/testing`
- Adopted by at least 1 project (gatekeeper-library) for CEL policy testing

#### GA (Stable)
- Adopted by 2+ projects outside of gatekeeper-library (e.g., Kyverno, a DRA driver, an AuthN config repo)
- API stable with no breaking changes for 2 K8s releases
- Cost tracking matches production behavior
- Documentation published on kubernetes.io

### Open Questions for sig-api-machinery


2. **Should AuthN's dual-env pattern (claims vs user) be modeled as two presets or one with a sub-option?**
   **Proposed: Two sub-methods on one evaluator** (`EvalAuthNClaims`, `EvalAuthNUser`) rather than two presets. This avoids preset proliferation.

3. **How should CRD schema-typed `self`/`oldSelf` be handled — require OpenAPI schema input, or support untyped `DynType` for simpler testing?**
   **Proposed: Support both.** Default to `DynType` for simplicity; optionally accept an OpenAPI schema for type-checked evaluation.

4. **Should this package provide assertion helpers (`celtest.RequireAllowed(t, result)`) or keep it minimal?**
   **Proposed: Keep the core API minimal.** Assertion helpers (`celtest.RequireAllowed`) can be added in Phase 5 or by downstream projects.

5. **What's the right level of cost tracking granularity to expose in test results?**
   **Proposed: Expose total cost as `int64` on result types.** Per-expression cost breakdown is future work.

## Test Plan

Unit tests for the `celtest` package itself will cover:

- **Preset construction**: Each of the 7 feature-specific `Eval*` methods creates the correct CEL environment with the expected variables and types available.
- **Version gating**: `WithVersion(1, 28)` correctly makes `ip()` unavailable; `WithVersion(1, 32)` makes all libraries available.
- **Preamble variable ordering**: Preamble variables are evaluated before policy variables; policy variables can reference preamble results via `variables.*`.
- **EvalAdmission correctness**: Known-good and known-bad policies produce the expected `Allowed`/`Violations` results.
- **CompileCheck**: Valid expressions return no errors; invalid expressions return descriptive compile errors.
- **Declarative runner**: `DiscoverAndRunTests` correctly discovers `*_test.cel` files, parses YAML test cases, and reports pass/fail per test case.
- **Error propagation**: Runtime CEL errors (e.g., `no such overload`) are correctly captured in `expect.error` / `expect.errorContains`.

Integration testing is deferred to adopting projects (gatekeeper-library, upstream K8s) where real policies exercise the full evaluation path.

## Drawbacks

- **Heavy dependency**: The package depends on `k8s.io/apiserver`, which is a large module. This is acceptable for `*_test.go` imports but makes the package unsuitable as a lightweight standalone library.
- **Tied to K8s release cadence**: Since the package reuses K8s internal CEL environment code, it must be updated with each K8s release. New CEL libraries or environment changes in K8s require a corresponding package update.
- **No CEL unit test framework**: Unlike Rego (which has `opa test`), CEL has no standalone test runner. The Go library fills the gap for Go developers; the `celtest` CLI (Phase 1b) provides the standalone runner experience for YAML-only policy authors.

## Alternatives

### Why not improve gator?

gator already provides CEL testing for Gatekeeper policies via `suite.yaml`. However:
- gator is Gatekeeper-specific — it requires `ConstraintTemplate` CRDs and Gatekeeper's variable injection model.
- gator only covers Gatekeeper admission policies (both OPA and CEL templates). It cannot test CRD validation rules, DRA selectors, AuthN/AuthZ expressions, or non-Gatekeeper VAPs.
- gator tests whole policies, not individual expressions or variables.

This proposal complements gator by providing expression-level and cross-feature testing that gator does not address.

### Why not standalone cel-go?

Using `github.com/google/cel-go` directly gives full control but:
- You must manually construct the CEL environment, which means replicating K8s's `MustBaseEnvSet()`, all custom libraries (Quantity, IP, CIDR, Semver, etc.), and feature-specific typed variables.
- The resulting environment will inevitably drift from the real K8s environment as libraries are added or changed across releases.
- The `celeval` package in gatekeeper-library took this approach and discovered it was missing libraries and type definitions that caused false positives in tests.

### Why not a CLI tool instead of a Go library?

A CLI tool alone (like `kubectl-validate`) would be more accessible to non-Go users, but the Go library is the correct foundation because:
- A Go API enables table-driven tests, IDE integration, and debugger support that a CLI cannot provide.
- The Go library can be imported by other tools (gator, Kyverno CLI, custom admission controllers) — a CLI cannot.
- K8s internal tests need the Go API directly; they don't shell out to CLI tools.

This KEP includes **both**: Phase 1a delivers the Go library, Phase 1b wraps it in a CLI. The Go library is the engine; the CLI is the user-facing interface. Both ship in the same release cycle.

---

*Status: Provisional*
*Date: February 2026*
