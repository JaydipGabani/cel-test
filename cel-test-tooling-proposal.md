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
  - [Proposed Required](#proposed-required)
  - [Package Location](#package-location)
  - [Design Principles](#design-principles)
  - [Testing Levels](#testing-levels)
  - [Declarative Test Format: *_test.cel](#declarative-test-format-_testcel)
  - [Core API](#core-api)
  - [Framework Adaptation: Preamble Variables](#framework-adaptation-preamble-variables)
  - [Architecture](#architecture)
  - [Downstream Requirements](#downstream-requirements)
  - [Usage Examples](#usage-examples)
  - [Comparison with Existing Tools](#comparison-with-existing-tools)
  - [CLI Tool Design](#cli-tool-design)
  - [Implementation Plan](#implementation-plan)
  - [Graduation Criteria](#graduation-criteria)
  - [Open Questions for sig-api-machinery](#open-questions-for-sig-api-machinery)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

Kubernetes uses CEL (Common Expression Language) across 7 use cases spanning 5 distinct CEL environments, each with its own variables, and custom types. Currently there is **no official testing utility** that sets up the correct CEL environment for any of these features. This KEP proposes:

1. **A Go testing package** (`k8s.io/apiserver/pkg/cel/testing/celtest`) that wraps the existing K8s CEL infrastructure into a simple API for evaluating CEL expressions in Go tests — using the real K8s CEL environment, not a custom one.
2. **A standalone CLI tool** (`kubernetes-sigs/cel-test`) that discovers and runs declarative `*_test.cel` YAML test files so that policy authors who don't write Go can test CEL expressions locally.

**Scope of this KEP:** This KEP delivers admission-style CEL testing (VAP, MAP expression testing, matchConditions) via the Go library and CLI. Support for other CEL contexts (CRD validation, DRA, AuthN/AuthZ) should be proposed in separate KEPs.

## Motivation

Users writing CEL expressions for Kubernetes must currently either deploy to a cluster (slow, no shift-left), use Gatekeeper-specific tooling like `gator` (covers Gatekeeper policies only, not other K8s CEL features), or roll their own evaluator (inevitably incomplete). The building blocks exist inside `k8s.io/apiserver`, they just aren't packaged for external testing use.

### Goals

- Provide a Go package that can evaluate CEL expressions in the real K8s CEL environment, starting with admission-style features (VAP, MAP, matchConditions) and designed to extend to other CEL contexts (CRD validation, DRA, AuthN, AuthZ) in follow-up KEPs.
- Support **per-expression**, **per-variable**, **whole-policy**, and **compile-check** testing levels.
- Enable **shift-left** testing: pure Go, zero cluster dependency, sub-second test runs.
- Support **K8s version pinning** for reproducible tests across Kubernetes releases.
- Support **framework preamble variables** (e.g., Gatekeeper's `anyObject`/`params`) so policy frameworks can inject their runtime variables into the test environment.
- Provide a **declarative test format** (`*_test.cel`) for YAML-based test cases colocated with policy source.
- Provide a **standalone CLI tool** (`celtest`) so that policy authors can test CEL expressions without writing Go.

### Non-Goals

- **Replacing framework-specific integration test tools** (e.g., Gatekeeper's `gator test`). This package tests CEL expressions in isolation; framework tools test full policy objects end-to-end.
- **Runtime evaluation in production.** This is a `testing`-only package, not an admission controller or policy engine.
- **Replacing `kubectl-validate` or schema validation.** This package evaluates CEL expressions, not Kubernetes resource schemas.

## Proposal

### User Stories

**Story 1: VAP Policy Author**
As a ValidatingAdmissionPolicy author, I want to test my CEL validation expressions locally in Go tests so that I can iterate quickly without deploying to a cluster.

**Story 2: Gatekeeper Policy Developer**
As a Gatekeeper library contributor, I want to test individual CEL variables (e.g., `containers`, `badContainers`) in isolation so that I can debug policy logic at the expression level, not just pass/fail at the whole-policy level. I need the test environment to include Gatekeeper's injected preamble variables (`anyObject`, `params`).

**Story 3: Policy Author (CLI)**
As a security team lead who writes VAP policies in YAML but not Go, I want to run `celtest run src/...` in CI to validate all my CEL expressions without maintaining Go test files or understanding Go tooling.

### Risks and Mitigations

**Risk: Dependency weight of `k8s.io/apiserver`.**
Testing-only package — imported in `*_test.go` files only, so the dependency does not affect production binaries.

**Risk: Environment drift between test package and production.**
The evaluator reuses upstream K8s code at every layer:

| Layer | Upstream code reused | Fidelity |
|---|---|---|
| **Base environment** | `environment.MustBaseEnvSet(ver)` | ✅ Same code path |
| **Typed declarations** | `BuildRequestType()`, `BuildNamespaceType()` via `CreateTestEnv()` → unexported `createEnvForOpts()` | ✅ Same code path |
| **MAP extension** | `hasPatchTypes` VersionedOptions (`library.JSONPatch` + `mutation.DynamicTypeResolver`) | ✅ Same code path |
| **Variable composition** | `NewCompositedCompilerForTypeChecking()` → `CompileAndStoreVariable()` → `AddField()` | ✅ Same code path |
| **Namespace filtering** | `CreateNamespaceObject()` (already exported in `condition.go`) | ✅ Same code path |
| **Evaluation loop** | Custom loop mirroring `ForInput()` | ⚠️ Equivalent |

The evaluation loop is the one piece reimplemented rather than called directly. `ForInput()` (in [condition.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go) / [composition.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go)) requires `admission.VersionedAttributes` — tied to the full admission pipeline — which cannot be constructed from unstructured test input. The custom loop preserves the same preamble → variables → validations ordering and uses `StoredExpressionsEnv`.

**This is why the package should live in `k8s.io/apiserver`**: inside the K8s tree, it can call `ForInput()` directly or expose a simpler evaluation method that accepts unstructured inputs.

**Risk: Version pinning backward compatibility.**
`environment.MustBaseEnvSet(version)` accepts a compatibility version and only enables libraries available at that version — the same mechanism the API server uses for rollback safety. The test package delegates entirely to this function.

> **Caveat:** Version pinning controls which CEL libraries are *available*, not which *implementation* runs. The library code comes from the Go binary you built against (e.g., K8s 1.33), not from the pinned version. This matches the API server's own rollback guarantee — no more, no less.

## Design Details

### CEL Features and Environments

Kubernetes uses CEL across 7 use cases (VAP, MAP, CRD validation, matchConditions, DRA, AuthN, AuthZ) spanning 5 distinct environments. This KEP targets the **admission environment** shared by VAP, MAP, and matchConditions:

| # | Feature | Package | Variables | Custom Types/Libraries | Env |
|---|---------|---------|-----------|----------------------|---|
| 1 | **ValidatingAdmissionPolicy (VAP)** | `k8s.io/apiserver/pkg/admission/plugin/cel` | `object`, `oldObject`, `request`, `params`, `namespaceObject`, `authorizer`, `variables` | AdmissionRequest, Namespace, Authorizer types | Admission |
| 2 | **MutatingAdmissionPolicy (MAP)** | same as VAP + `mutation.go` | same as VAP | `library.JSONPatch` (adds `jsonPatch.escape()`), `mutation.DynamicTypeResolver` | Admission (extended) |
| 3 | **Webhook matchConditions** | `k8s.io/apiserver/pkg/admission/plugin/cel` (same package as VAP via `ConditionCompiler`) | `object`, `oldObject`, `request` | AdmissionRequest type | Admission (subset) |

### How CEL Environments Are Built Today

All K8s CEL features follow the same pattern:
```
MustBaseEnvSet(ver) → .Extend(feature-specific variables + types) → .Env(StoredExpressions) → Compile → Program → Eval
```

The admission features targeted by this KEP:

| Feature | Call site | Notes |
|---|---|---|
| VAP | `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go` | `mustBuildEnvs()` with `HasPatchTypes: false` |
| MAP | `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/mutating/plugin.go` | `mustBuildEnvs()` with `HasPatchTypes: true` |
| Webhook matchConditions | `staging/src/k8s.io/apiserver/pkg/admission/plugin/webhook/generic/webhook.go` | Same `compile.go` compiler via `ConditionCompiler` |

The building blocks exist in `k8s.io/apiserver` — they just aren't packaged for external testing use.

### Proposed Required

Upstream changes required in `kubernetes/kubernetes`:

**New package: `k8s.io/apiserver/pkg/cel/testing/celtest`** — core deliverable of Phase 1a:

| File | Contents |
|---|---|
| `evaluator.go` | `Evaluator`, `NewEvaluator()`, `EvalAdmission()`, `EvalExpression()`, `EvalVariable()`, `CompileCheck()`, options |
| `parse.go` | `ParseVAPPolicy()`, `ParseVAPPolicyFile()` — YAML parsing for `.cel` policy files |
| `runner.go` | `DiscoverAndRunTestsRaw()`, `DiscoverAndRunTestsWithEvaluator()`, `RunTestFileWithEvaluator()` |

**Modification: `k8s.io/apiserver/pkg/admission/plugin/cel/testing_helpers.go`** (new file) — exports `CreateTestEnv(baseEnv, opts)` (thin wrapper delegating to the unexported `createEnvForOpts()` in the same package) and `TestActivation` struct (implementing `interpreter.Activation` for unstructured inputs). A unit test (`TestCreateTestEnvEquivalence`) asserts equivalence with the production `mustBuildEnvs()` path. No changes to `compile.go` — `BuildRequestType()`, `BuildNamespaceType()`, `OptionalVariableDeclarations` are already exported.

Everything else — CLI tool, framework preambles, output formatters, config, examples — lives downstream.

### Package Location

**1. Go library → `k8s.io/apiserver/pkg/cel/testing/celtest`** — follows the precedent of `k8s.io/client-go/testing` and `k8s.io/apiserver/pkg/storage/testing`. Must live in `k8s.io/apiserver` because `CreateTestEnv()` needs same-package access to the unexported `createEnvForOpts()` and `hasPatchTypes`. A standalone module would have to duplicate this logic and risk drift. The dependency weight is acceptable since the package is imported only in `*_test.go` files.

**2. CLI tool → `kubernetes-sigs/cel-test` (Phase 1b)** — standalone CLI wrapping the Go library. Handles command/flag parsing, file discovery, output formatting (text, JSON, JUnit), and `.celtest.yaml` config. Follows the precedent of `kubernetes-sigs/kubectl-validate`.

Commands: `celtest run src/...`, `celtest compile src/policy/src.cel`

### Design Principles

1. **Use the real K8s CEL environment** — no custom env, no drift
2. **Feature-specific evaluation methods** — different CEL contexts (VAP, CRD, matchConditions) have different available variables and libraries, selected by which `Eval*` method you call
3. **Per-expression testing** — test individual CEL expressions and variables, not just whole policies
4. **Declarative test files** — `*_test.cel` YAML files colocated with policy source, auto-discovered
5. **Table-test friendly** — designed for Go `testing.T` with `[]struct` patterns (Go API) or YAML test cases (declarative)
6. **Versioned** — pin to a K8s version for reproducible tests
7. **Zero cluster dependency** — pure Go, no informers, no API server

### Testing Levels

The tooling supports four levels of testing, each addressing a different need:

| Level | API | What it tests | Who needs it |
|---|---|---|---|
| **Per-expression** | `EvalExpression(expr, input, vars)` | Does a single CEL expression return the expected value? | CEL expression *authors* |
| **Per-variable** | `variable:` in `*_test.cel` | Does a specific policy variable compute correctly for given input? | Policy *developers* |
| **Whole-policy** | `EvalAdmission(policy, input)` or `expect.allowed:` in `*_test.cel` | Does the complete policy allow/deny correctly? | Policy *consumers* |
| **Compile-check** | `CompileCheck(expr)` | Does the expression compile in the real K8s env? | CI/linting |

**Per-expression and per-variable testing is the primary value add** — gator and integration tests already cover whole-policy testing. What's missing in the ecosystem is the ability to test individual CEL expressions in isolation with the correct K8s environment.

> **Compile-check shorthand:** `err := eval.CompileCheck(\`object.metadata.labels.exists(k, k == "app")\`)` — returns a descriptive `error` if the expression fails to compile, `nil` otherwise.

### Declarative Test Format: `*_test.cel`

Test files use the `*_test.cel` suffix convention (matching Go's `*_test.go` and OPA's `*_test.rego`). A test file is paired with its policy by base name: `foo.cel` is tested by `foo_test.cel` in the same directory. Alternatively, `source:` can reference any `.cel` policy file explicitly.

**Discovery:** Walk for `*_test.cel` → find companion `foo.cel` in the same directory. No companion → must be `mode: expression`.

Policy source files use the `.cel` extension with a simple YAML format: top-level `variables:` and/or `validations:` keys. This is the only supported policy source format — the tool does not parse native K8s resource YAML (`ValidatingAdmissionPolicy`, `MutatingAdmissionPolicy`, etc.). Users with VAP/MAP YAML manifests should extract their CEL expressions into `.cel` files for testing.

```yaml
# src/pod-security-policy/privileged-containers/src_test.cel
tests:
# ---- Variable-level test ----
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

# ---- Whole-policy test ----
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

# ---- Expression test ----
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
func TestCELPolicies(t *testing.T) {
    celtest.DiscoverAndRunTestsRaw(t, "../../src")
}
```

#### Formal YAML Schema

```yaml
# TestFile schema
mode: string          # Optional. "policy" (default) or "expression".
source: string        # Optional. Explicit path to a .cel policy file (overrides auto-discovery).
tests:                # Required. Array of TestCase, minimum 1.
  - name: string      # Required. Unique within file. Used as Go subtest name.

    # --- Test type (mutually exclusive, pick at most one) ---
    variable: string   # Test a named policy variable. Only in mode: policy.
    expression: string # Test an arbitrary CEL expression. Works in both modes.
    # (neither):       # Test the whole policy (allowed/denied). Only in mode: policy.

    # --- Input ---
    object: map        # Optional. The object being admitted.
    oldObject: map     # Optional. Previous version (for UPDATE/DELETE).
    params: map        # Optional. Policy parameters.
    request: map       # Optional. Merged onto default (operation: CREATE).

    # --- Assertions ---
    expect:            # Required.
      value: any       # Exact value match (numeric normalization: int64 ≡ int).
      size: int        # Length of list, map, or string result.
      contains: string # String → substring match; list → element containment.
      allowed: bool    # Whole-policy: validation pass/fail.
      messageContains: string  # Whole-policy: violation message substring.
      error: bool      # Expect an evaluation error.
      errorContains: string  # Error message substring.
```

**Validation rules:** `variable` and `expression` are mutually exclusive. In `mode: expression`, `variable` and `expect.allowed` are rejected at parse time. `expect.allowed`/`expect.messageContains` are only valid for whole-policy tests. `expect.value`/`expect.size`/`expect.contains` are only valid for `variable`/`expression` tests. Empty slices: `nil` ≡ `[]`.

#### Expression Mode

For standalone CEL expressions without policy variables, set `mode: expression`:

```yaml
mode: expression
tests:
- name: "replicas within limit"
  expression: "object.spec.replicas <= object.spec.maxReplicas"
  object:
    spec: { replicas: 3, maxReplicas: 5 }
  expect:
    value: true
```

### Core API

```go
package celtest

import "k8s.io/apimachinery/pkg/util/version"

// Evaluator compiles and evaluates CEL expressions using the real K8s CEL
// environment. Focuses on admission-style CEL (VAP, MAP, matchConditions).
type Evaluator struct {
    envSet       *environment.EnvSet
    version      *version.Version
    preambleVars []Variable
}

type Option func(*Evaluator)

func WithVersion(major, minor uint) Option { ... }

// WithoutPatchTypes disables the MAP extension (library.JSONPatch +
// mutation.DynamicTypeResolver). Default: enabled.
func WithoutPatchTypes() Option { ... }

// NewEvaluator creates an evaluator via environment.MustBaseEnvSet() extended
// with admission variables/types via CreateTestEnv() → createEnvForOpts().
// MAP extension enabled by default; use WithoutPatchTypes() for pure VAP.
func NewEvaluator(opts ...Option) (*Evaluator, error) { ... }

// AdmissionInput maps to CEL variables per evaluationActivation in activation.go:
//
//   | Field     | CEL variable      | Type                        | Default            |
//   |-----------|-------------------|-----------------------------|--------------------|
//   | Object    | object            | DynType                     | nil (null)         |
//   | OldObject | oldObject         | DynType                     | nil (null)         |
//   | Params    | params            | DynType                     | nil (null)         |
//   | Request   | request           | kubernetes.AdmissionRequest | Operation="CREATE" |
//   | Namespace | namespaceObject   | kubernetes.Namespace        | nil (null)         |
//
// authorizer is not declared — expressions referencing it fail at compile time.
type AdmissionInput struct {
    Object    map[string]interface{}
    OldObject map[string]interface{}
    Params    map[string]interface{}
    Request   *admissionv1.AdmissionRequest
    Namespace *corev1.Namespace
}

type VAPPolicy struct {
    Variables   []Variable
    Validations []Validation
}

type AdmissionResult struct {
    Allowed    bool
    Violations []Violation
    Cost       int64 // total CEL cost in K8s cost units
}

type Violation struct {
    Expression string
    Message    string
    Error      error
}

// WithCostLimit sets a shared CEL cost budget for EvalAdmission, consumed
// sequentially: preamble → variables → validations. Matches the API server's
// runtimeCELCostBudget. Default: no limit (cost reported but never fails).
func WithCostLimit(limit int64) Option { ... }

const PerCallLimit = celconfig.PerCallLimit // 1,000,000

// EvalAdmission evaluates a VAP/MAP/matchCondition policy against input.
func (e *Evaluator) EvalAdmission(policy *VAPPolicy, input *AdmissionInput) (*AdmissionResult, error) { ... }

// EvalExpression evaluates a single CEL expression. All admission variables
// are available. extraVars injects additional activation bindings (e.g.,
// "variables.containers" to simulate a computed variable).
func (e *Evaluator) EvalExpression(expr string, input *AdmissionInput, extraVars map[string]interface{}) (interface{}, error) { ... }

// CompileCheck validates that a CEL expression compiles. Uses NewExpressions
// mode (strict version gating), while Eval* methods use StoredExpressions
// mode (permissive, for rollback safety). This means an expression can fail
// CompileCheck but succeed in EvalExpression if it uses a library introduced
// after the configured version.
func (e *Evaluator) CompileCheck(expr string) error { ... }

// ParseVAPPolicy parses a .cel policy file (top-level variables:/validations: keys).
func ParseVAPPolicy(yamlContent string) (*VAPPolicy, error) { ... }
func ParseVAPPolicyFile(path string) (*VAPPolicy, error) { ... }

// EvalVariable evaluates a single named variable, running preamble + all
// policy vars up to and including the target. Primary value add over whole-policy testing.
func (e *Evaluator) EvalVariable(policy *VAPPolicy, variableName string, input *AdmissionInput) (interface{}, error) { ... }

// ========== Declarative Test Runner ==========

// DiscoverAndRunTestsWithEvaluator walks srcRoot for *_test.cel files.
// wrapParams wraps params in {"spec":{"parameters":<params>}} (Gatekeeper convention).
func DiscoverAndRunTestsWithEvaluator(t *testing.T, eval *Evaluator, srcRoot string, wrapParams bool) { ... }

func DiscoverAndRunTestsRaw(t *testing.T, srcRoot string) { ... }

func RunTestFileWithEvaluator(t *testing.T, eval *Evaluator, testFilePath string, wrapParams bool) { ... }
```

### Framework Adaptation: Preamble Variables

Policy frameworks inject runtime-computed variables before CEL evaluation. **Preamble variables** are CEL expressions evaluated before the policy's own variables, configured via `WithPreambleVariables`. Parameter wrapping (e.g., Gatekeeper's `{spec: {parameters: ...}}`) is handled downstream.

```go
// WithPreambleVariables registers CEL expressions evaluated BEFORE policy variables.
// Evaluation order: preamble → policy variables → validations.
func WithPreambleVariables(vars ...Variable) Option { ... }
```

### Architecture

```
  environment.MustBaseEnvSet(ver)
          │ .Extend()
    ┌─────┼─────┐
    VAP   MAP  Match
    │
    │ WithPreambleVariables (optional)
    ▼
  Gatekeeper/Kyverno/Custom preamble
    ▼
  Policy variables (from src.cel)
    ▼
  Validations (from src.cel)
```

### Downstream Requirements

The upstream package provides the generic core. Downstream projects provide:
- **Framework preamble definitions** (e.g., Gatekeeper's `anyObject`/`params` CEL expressions)
- **Convenience runner wrappers** pre-configured with the correct preamble
- **CLI tool** (`kubernetes-sigs/cel-test`) — wraps the Go library with CLI parsing, output formatting, and config
- **Framework-specific test examples** and CI integration documentation

### Usage Examples

#### Gatekeeper Library (Preamble Variables)

```go
func newGatekeeperEvaluator() (*celtest.Evaluator, error) {
    return celtest.NewEvaluator(
        celtest.WithPreambleVariables(
            celtest.Variable{Name: "anyObject",
                Expression: `has(request.operation) && request.operation == "DELETE" && object == null ? oldObject : object`},
            celtest.Variable{Name: "params",
                Expression: `!has(params.spec) ? null : !has(params.spec.parameters) ? null: params.spec.parameters`},
        ),
    )
}

func TestPrivilegedContainers(t *testing.T) {
    eval, _ := newGatekeeperEvaluator()
    policy, _ := celtest.ParseVAPPolicyFile("src/pod-security-policy/privileged-containers/src.cel")
    result, err := eval.EvalAdmission(policy, &celtest.AdmissionInput{
        Object: map[string]interface{}{
            "metadata": map[string]interface{}{"name": "test-pod"},
            "spec": map[string]interface{}{
                "containers": []interface{}{
                    map[string]interface{}{"name": "nginx", "image": "nginx",
                        "securityContext": map[string]interface{}{"privileged": true}},
                },
            },
        },
        Params: map[string]interface{}{
            "spec": map[string]interface{}{"parameters": map[string]interface{}{}},
        },
    })
    if err != nil { t.Fatal(err) }
    if result.Allowed { t.Error("expected denial") }
}
```

### Comparison with Existing Tools

| Tool | Env Accuracy | API | Cluster | Scope |
|---|---|---|---|---|
| **This proposal** | ✅ Real K8s env | ✅ Simple Go API | ❌ No | Admission (VAP, MAP, matchConditions) |
| gator CLI | ✅ Real K8s env | ⚠️ YAML suite files | ❌ No | Gatekeeper policies only (OPA + CEL templates) |
| kaptest | ⚠️ Third-party | ✅ Simple | ❌ No | VAP only |
| kubectl-validate (#130570) | ✅ Real K8s env | ⚠️ CLI tool | ❌ No | Schema validation |
| Custom `cel-go` (celeval) | ❌ Incomplete env | ✅ Simple Go API | ❌ No | Custom subset |

### CLI Tool Design

The `celtest` CLI (`kubernetes-sigs/cel-test`, Phase 1b) delegates all CEL evaluation to the Go library.

#### Commands

**`celtest run [paths...]`** — Discover and run `*_test.cel` files.

| Flag | Type | Default | Description |
|---|---|---|---|
| `--version` | `string` | latest | K8s compatibility version |
| `--config` | `string` | auto | Config file (auto-discovers `.celtest.yaml`) |
| `--output` / `-o` | `string` | `text` | Output format: `text`, `json`, `junit` |
| `--verbose` / `-v` | `bool` | false | Show passing tests |
| `--fail-fast` | `bool` | false | Stop on first failure |
| `--cost-limit` | `int` | 0 | CEL cost budget |

**`celtest compile [files...]`** — Compile-check all expressions without evaluating.

| Flag | Type | Default | Description |
|---|---|---|---|
| `--version` | `string` | latest | K8s compatibility version |
| `--output` / `-o` | `string` | `text` | Output format: `text`, `json` |

#### Exit Codes

`0` = success, `1` = test failure, `2` = config error, `3` = compilation error.

#### Configuration File (`.celtest.yaml`)

```yaml
version: "1.31"
output: text
preamble:
  variables:
  - name: anyObject
    expression: 'has(request.operation) && request.operation == "DELETE" && object == null ? oldObject : object'
  - name: params
    expression: '!has(params.spec) ? null : !has(params.spec.parameters) ? null : params.spec.parameters'
```

Without a config file, the CLI runs in raw mode (`DiscoverAndRunTestsRaw`).

### Implementation Plan

#### Phase 1a: Core Go Library (MVP)
- `NewEvaluator` with admission-style env (MAP extension enabled by default via `HasPatchTypes: true`)
- `EvalAdmission`, `EvalExpression`, `EvalVariable`, `CompileCheck`
- `ParseVAPPolicy` / `ParseVAPPolicyFile` helpers
- `WithVersion`, `WithPreambleVariables`, `WithCostLimit`
- Declarative `*_test.cel` runner
- MAP expression compilation and evaluation supported; MAP mutation *application* (patching objects) deferred
- `HasAuthorizer` not enabled — `authorizer` references produce compile-time errors

#### Phase 1b: CLI Tool (`kubernetes-sigs/cel-test`)
Wraps the Go library. Installable via `go install sigs.k8s.io/cel-test/cmd/celtest@latest`.

> Additional CEL contexts (CRD, DRA, AuthN/AuthZ) and advanced features (mock authorizer) will be proposed in separate follow-up KEPs.

### Graduation Criteria

**Alpha:** Full admission evaluation (VAP/MAP/matchConditions), `celtest` CLI with `run`/`compile`, unit tests for version pinning and preamble ordering.

**Beta:** API stable, declarative runner shipped, adopted by 1+ external project, integration test for `ForInput()` equivalence.

**GA:** 2+ adopters, API stable for 2 releases, cost tracking matches production, docs on kubernetes.io.


## Test Plan

Unit tests cover: admission env construction, version gating (`WithVersion(1, 28)` blocks `ip()`), preamble variable ordering, `EvalAdmission` correctness, `CompileCheck` error reporting, declarative `*_test.cel` runner discovery/parsing/reporting, and runtime error propagation via `expect.error`. Integration testing deferred to adopting projects.

## Drawbacks

- **Heavy dependency**: `k8s.io/apiserver` is large, but acceptable for `*_test.go`-only imports.
- **Tied to K8s release cadence**: Must be updated with each release for new CEL libraries.

## Alternatives

**Why not improve gator?** gator is Gatekeeper-specific (requires ConstraintTemplate CRDs), tests whole policies only, and cannot test non-Gatekeeper VAPs. This proposal complements gator with expression-level testing.

**Why not standalone cel-go?** Manually constructing the CEL environment inevitably drifts from the real K8s environment. The `celeval` package in gatekeeper-library proved this — missing libraries caused false positives.

**Why not CLI-only?** A Go API enables table-driven tests, IDE integration, debugger support, and import by other tools. This KEP delivers both: Go library (Phase 1a) + CLI (Phase 1b).

---

*Status: Provisional*
*Date: February 2026*
