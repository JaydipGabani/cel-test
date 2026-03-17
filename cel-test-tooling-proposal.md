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
- [Implementation History](#implementation-history)
<!-- /toc -->

## Summary

Kubernetes uses CEL (Common Expression Language) across 7 use cases spanning 5 distinct CEL environments, each with its own variables, and custom types. Currently there is **no official testing utility** that sets up the correct CEL environment for any of these features. This KEP proposes:

1. **A Go testing package** (`k8s.io/apiserver/pkg/cel/testing/celtest`) that wraps the existing K8s CEL infrastructure into a simple API for evaluating CEL expressions in Go tests — using the real K8s CEL environment, not a custom one.
2. **A standalone CLI tool** (`kubernetes-sigs/cel-test`) that discovers and runs declarative `*_test.cel` YAML test files so that policy authors who don't write Go can test CEL expressions locally.

**Scope of this KEP:** Phase 1 delivers admission-style CEL testing (VAP, MAP expression testing, matchConditions) via the Go library and CLI. CRD validation (Phase 2), DRA (Phase 3), AuthN/AuthZ (Phase 4), and advanced features (Phase 5) are described here for architectural context but are expected to be proposed as separate follow-up KEPs with their own design details and graduation criteria.

## Motivation

Users writing CEL expressions for Kubernetes must currently either deploy to a cluster (slow, no shift-left), use Gatekeeper-specific tooling like `gator` (covers Gatekeeper policies only, not other K8s CEL features), or roll their own evaluator (inevitably incomplete). The building blocks exist inside `k8s.io/apiserver`, they just aren't packaged for external testing use.

### Goals

- Provide a Go package that can evaluate CEL expressions in the real K8s CEL environment, starting with admission-style features (VAP, MAP, matchConditions) and designed to extend to other CEL contexts (CRD validation, DRA, AuthN, AuthZ) in follow-up KEPs.
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
*Mitigation:* The evaluator reuses upstream K8s code across the following layers:

| Layer | Upstream code reused | Fidelity | Notes |
|---|---|---|---|
| **Base environment** | `environment.MustBaseEnvSet(ver)` | ✅ Same code path | Identical versioned libraries, same version-gating mechanism |
| **Typed declarations** | `admissioncel.BuildRequestType()`, `BuildNamespaceType()` (in [compile.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go)) | ✅ Same code path | Uses the same `BuildRequestType()` and `BuildNamespaceType()` functions. The test env is assembled by `CreateTestEnv()` which lives in the same package as the unexported `createEnvForOpts()` and delegates to it directly — no mirroring needed. `StrictCostOpt` is also applied automatically by `createEnvForOpts()`. Production pre-builds 8 env combinations via `mustBuildEnvs()`; the test path builds exactly the one requested. |
| **MAP extension** | `hasPatchTypes` VersionedOptions (same `library.JSONPatch` + `mutation.DynamicTypeResolver`) | ✅ Same code path | `CreateTestEnv()` calls `createEnvForOpts()` which applies the unexported `hasPatchTypes` extension when `HasPatchTypes: true` is set. This enables compile-checking and evaluating MAP mutation expressions (`jsonPatch.escape()`, `Object.*` type resolution). MAP mutation *application* (applying patches to produce a mutated object) is out of scope — see Phase 1a notes. |
| **Variable composition** | Uses `NewCompositedCompilerForTypeChecking()` | ✅ Same code path | The test evaluator uses the already-exported `NewCompositedCompilerForTypeChecking()` function to create a `CompositedCompiler` with the `kubernetes.variables` typed map. Variables are compiled via `CompileAndStoreVariable()`, which calls `AddField()` to register each variable's output type — enabling subsequent variables to type-check references like `variables.containers`. A typo like `variables.contnainers` correctly fails to compile, matching production behavior. Cost estimation also benefits from concrete types rather than `DynType` defaults. The ordering semantics (preamble → variables → validations) are preserved. |
| **Namespace filtering** | Calls `admissioncel.CreateNamespaceObject()` directly | ✅ Same code path | `CreateNamespaceObject()` is already exported in `condition.go`. The test package calls it directly to filter namespace objects to safe fields, identical to the production path. |
| **Evaluation loop** | Custom loop mirroring `ForInput()` | ⚠️ Equivalent behavior | See below |

The evaluation loop is the one piece that is reimplemented rather than called directly. The upstream `ForInput()` method (in [condition.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go) and [composition.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go)) requires `admission.VersionedAttributes` — a type tied to the full K8s admission pipeline — which cannot be cleanly constructed from unstructured test input. The activation bindings are implemented in [activation.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go) (`evaluationActivation` struct). The custom loop follows the same preamble → variables → validations ordering and uses the same `StoredExpressionsEnv` for evaluation.

**This is why the package should live in `k8s.io/apiserver`**: inside the K8s tree, it can either call `ForInput()` directly (by constructing `VersionedAttributes` from internal types) or expose a simpler evaluation method that accepts unstructured inputs.

**Risk: DRA lives in a separate module (`k8s.io/dynamic-resource-allocation`).**
The DRA CEL environment cannot be reached from `k8s.io/apiserver` without a cross-module dependency.
*Mitigation:* Phase 1-2 covers only apiserver-resident features (VAP, MAP, CRD, matchConditions). DRA support (Phase 3) either adds a dependency or lives in `k8s.io/dynamic-resource-allocation/cel/testing`. A thin orchestrator in `sigs.k8s.io/cel-testing` can unify them.

**Risk: Version pinning backward compatibility.**
If a user pins `WithVersion(1, 28)` but the package ships with K8s 1.33, the base environment code may have changed behavior for how it handles older version gating.
*Mitigation:* `environment.MustBaseEnvSet(version)` is designed specifically for this. It accepts a compatibility version and only enables libraries and features available at that version. This is the same mechanism the K8s API server uses for rollback safety (`DefaultCompatibilityVersion()`). The test package delegates entirely to this function, so version pinning inherits the same backward compatibility guarantees as the API server itself.

> **Important caveat:** Version pinning controls which CEL libraries are *available* in the environment, not which *implementation* of those libraries runs. The library code comes from the Go binary you built against (e.g., K8s 1.33), not from K8s 1.28. If a library's behavior changed between 1.28 and 1.33 (e.g., a bug fix or semantic change), `WithVersion(1, 28)` running on a 1.33 binary will still use the 1.33 implementation of that library. This is the same guarantee the API server itself provides during rollback — no more, no less.

### Behavioral Details

**Thread safety and `t.Parallel()` support.**
The `Evaluator` is safe for concurrent use from multiple goroutines. Compilation results are cached in a `sync.Mutex`-protected `compilationCache` map, so parallel Go subtests (`t.Parallel()`) sharing the same `Evaluator` instance work correctly without external synchronization. The cache is keyed by expression text, so identical expressions compiled from different test cases hit the cache. Environment construction (`NewEvaluator`) is not concurrent — create the evaluator once and share it across parallel tests.

**Preamble variable compilation failures.**
Preamble variables are compiled lazily at first evaluation, not eagerly at `NewEvaluator()` construction time. If a preamble variable’s CEL expression fails to compile (e.g., syntax error), the failure surfaces as an error from `EvalAdmission()` or `EvalVariable()` at call time, not at evaluator construction. This is consistent with how the production admission pipeline handles compilation.

**Missing companion policy file.**
If a `*_test.cel` file has no `mode:` field (defaults to `mode: policy`) and no companion `*.cel` file exists in the same directory, the test runner calls `t.Fatalf()` with a descriptive error: `"loading companion policy <path>: open <path>: no such file or directory"`. The runner does not silently skip the file or fall back to expression mode.

**Parameter wrapping (`wrapParams`).**
The `DiscoverAndRunTestsWithEvaluator` function accepts a `wrapParams bool` parameter. When `true`, the runner automatically wraps `params` from test cases inside `{"spec": {"parameters": <params>}}` before evaluation — matching the Gatekeeper constraint CRD structure. This is a convenience for frameworks that expect parameters nested inside a CRD wrapper. When `false` (and in `DiscoverAndRunTestsRaw`), params are passed as-is.

## Design Details

### CEL Features and Environments

Kubernetes uses CEL across **7 use cases** spanning **5 distinct CEL environments** (VAP/MAP/matchConditions share the same admission env, AuthN has 2 sub-envs). Each has its own variables and custom types:

| # | Feature | Package | Variables | Custom Types/Libraries | Env |
|---|---------|---------|-----------|----------------------|---|
| 1 | **ValidatingAdmissionPolicy (VAP)** | `k8s.io/apiserver/pkg/admission/plugin/cel` | `object`, `oldObject`, `request`, `params`, `namespaceObject`, `authorizer`, `variables` | AdmissionRequest, Namespace, Authorizer types | Admission |
| 2 | **MutatingAdmissionPolicy (MAP)** | same as VAP + `mutation.go` | same as VAP | `library.JSONPatch` (adds `jsonPatch.escape()`), `mutation.DynamicTypeResolver` | Admission (extended) |
| 3 | **CRD Validation Rules** | `k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel` | `self`, `oldSelf` | Schema-derived types from OpenAPI | CRD |
| 4 | **Webhook matchConditions** | `k8s.io/apiserver/pkg/admission/plugin/cel` (same package as VAP via `ConditionCompiler`) | `object`, `oldObject`, `request` | AdmissionRequest type | Admission (subset) |
| 5 | **Dynamic Resource Allocation (DRA)** | `k8s.io/dynamic-resource-allocation/cel` | `device` (with `.driver`, `.attributes`, `.capacity`, `.allowMultipleAllocations` [1.34+, `ConsumableCapacity` gate]) | `DRADevice` typed object (versioned: `deviceTypeV131` without, `deviceTypeV134ConsumableCapacity` with `allowMultipleAllocations`), custom map-with-default, `ext.Bindings(ext.BindingsVersion(0))`. Note: `Semver` type is now in the base env since 1.33 via `library.SemverLib` | DRA |
| 6 | **Authentication (AuthN)** | `k8s.io/apiserver/pkg/authentication/cel` | `claims` OR `user` (two separate envs via `mustBuildEnvs()`) | `kubernetes.UserInfo` typed object (username, uid, groups, extra), claims as `map(string, any)` | AuthN (×2) |
| 7 | **Authorization (AuthZ)** | `k8s.io/apiserver/pkg/authorization/cel` | `request` (SubjectAccessReviewSpec) | `kubernetes.SubjectAccessReviewSpec`, `kubernetes.ResourceAttributes` (with optional `fieldSelector`/`labelSelector` behind `AuthorizeWithSelectors` feature gate), `kubernetes.NonResourceAttributes`, `kubernetes.SelectorRequirement` | AuthZ |

### How CEL Environments Are Built Today

All 7 features follow the same pattern:
```
MustBaseEnvSet(ver) → .Extend(feature-specific variables + types) → .Env(StoredExpressions) → Compile → Program → Eval
```

| Feature | Call site | Notes |
|---|---|---|
| VAP | `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go` | `mustBuildEnvs()` with `HasPatchTypes: false` |
| MAP | `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/mutating/plugin.go` | `mustBuildEnvs()` with `HasPatchTypes: true` |
| Webhook matchConditions | `staging/src/k8s.io/apiserver/pkg/admission/plugin/webhook/generic/webhook.go` | Same `compile.go` compiler via `ConditionCompiler` |
| CRD Validation | `staging/src/k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel/validation.go` | Separate pipeline via `prepareEnvSet()` |
| DRA | `staging/src/k8s.io/dynamic-resource-allocation/cel/compile.go` | Separate module |
| AuthN, AuthZ | `staging/src/k8s.io/apiserver/pkg/apis/apiserver/validation/validation.go` | AuthN: 2 separate envs via `mustBuildEnvs()`; AuthZ: 1 env via `mustBuildEnv()` (singular) |

The building blocks exist in `k8s.io/apiserver` — they just aren't packaged for external testing use.

### Proposed Required

This section consolidates all changes that must be contributed to the `kubernetes/kubernetes` repository (upstream). Everything else — the CLI tool, framework-specific preamble definitions, CI examples — lives downstream in `kubernetes-sigs/cel-test` or in adopting projects.

#### New package: `k8s.io/apiserver/pkg/cel/testing/celtest`

A new testing sub-package in apiserver staging. This is the core deliverable of Phase 1a.

| File | Contents | Approx LOC |
|---|---|---|
| `evaluator.go` | `Evaluator` struct, `NewEvaluator()`, `EvalAdmission()`, `EvalExpression()`, `EvalVariable()`, `CompileCheck()`, `WithVersion()`, `WithPreambleVariables()`, `WithCostLimit()` | ~300 |
| `parse.go` | `ParseVAPPolicy()`, `ParseVAPPolicyFile()`, `ParsePolicySource()` — YAML parsing for both the lightweight `variables:` / `validations:` format AND native K8s admission resource YAML (`ValidatingAdmissionPolicy`, `MutatingAdmissionPolicy` — auto-detected via `apiVersion`/`kind` fields) | ~180 |
| `runner.go` | `DiscoverAndRunTestsRaw()`, `DiscoverAndRunTestsWithEvaluator()`, `RunTestFileWithEvaluator()` — declarative `*_test.cel` test runner | ~350 |
| `types.go` | `AdmissionInput`, `AdmissionResult`, `Violation`, `VAPPolicy`, `Variable`, `Validation` | ~50 |

#### Modifications to existing package: `k8s.io/apiserver/pkg/admission/plugin/cel`

The test package needs helpers that mirror unexported internal functions. These are added to the production admission CEL package as exported test-support APIs.

| File | Change | Why |
|---|---|---|
| `testing_helpers.go` (new) | Export `CreateTestEnv(baseEnv, opts)` — a thin wrapper that delegates to the unexported `createEnvForOpts()` in the same package (not a mirror/reimplementation). Also exports `TestActivation` struct implementing `interpreter.Activation` for evaluating from unstructured inputs. | The production `createEnvForOpts()` is unexported and `evaluationActivation` requires `admission.VersionedAttributes` which can't be constructed from test input. Since `testing_helpers.go` lives in the same package, `CreateTestEnv()` calls `createEnvForOpts()` directly — full fidelity, no drift risk. The unexported `hasPatchTypes` extension is also applied via this delegation. A unit test (`TestCreateTestEnvEquivalence`) asserts that `CreateTestEnv()` produces an environment equivalent to the one produced by the production `mustBuildEnvs()` path, so any refactoring of internal functions is caught immediately. |

**`TestActivation.ResolveName()` conversion semantics:**

`TestActivation` replicates the exact conversion semantics of the production `evaluationActivation.ResolveName()` in [activation.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go). Since `TestActivation` lives in the same package, it calls the unexported `objectToResolveVal()` and `CreateNamespaceObject()` directly — **zero reimplementation, zero drift risk**. The full per-field conversion specification (nil behavior, non-nil conversion, production equivalence) is documented in `testing_helpers.go` code comments. A unit test (`TestActivationEquivalence`) asserts that `TestActivation.ResolveName()` produces identical `ref.Val` values as the production `evaluationActivation` for the same inputs.
| `compile.go` | No changes. `BuildRequestType()`, `BuildNamespaceType()`, and `OptionalVariableDeclarations` are already exported. The unexported `createEnvForOpts()` and `hasPatchTypes` are accessed from `testing_helpers.go` in the same package. | Already usable as-is. |

**Upstream export requirements:** `CreateTestEnv()` and `TestActivation` are the only new exports required in existing production packages. All other functions the test package needs (`BuildRequestType`, `BuildNamespaceType`, `MustBaseEnvSet`, `NewCompositedCompilerForTypeChecking`, `CreateNamespaceObject`, etc.) are already exported.

#### What is NOT upstream

Everything not listed above — CLI tool, framework preambles, output formatters, config schema, test examples — lives downstream. See [Downstream Requirements](#downstream-requirements) for the full breakdown of what downstream projects are expected to provide.

### Package Location

This design has two deliverables that follow established Kubernetes contribution patterns:

**1. Go library → `k8s.io/apiserver/pkg/cel/testing/celtest`**

The core Go API (`NewEvaluator`, `EvalExpression`, `CompileCheck`, `WithPreambleVariables`) lives in `k8s.io/apiserver` staging. This follows the precedent of other testing packages in the K8s tree:

| Existing package | What it provides |
|---|---|
| `k8s.io/client-go/testing` | Fake client, reactors for unit testing API interactions |
| `k8s.io/apiserver/pkg/storage/testing` | Store test suite functions for etcd |

Note: these precedent packages differ in scope — `k8s.io/client-go/testing` is a large (~4000 LOC) fake API server implementation, while this proposal is a thin evaluation wrapper. The comparison is about *location convention* (testing sub-packages in staging), not scope equivalence.

**Why `k8s.io/apiserver` and not a standalone module:** The test helper `CreateTestEnv()` lives in `k8s.io/apiserver/pkg/admission/plugin/cel` (in `testing_helpers.go`) alongside the unexported `createEnvForOpts()` and `hasPatchTypes`, which it calls directly. This is only possible because the test helper is in the same Go package. The core test package (`celtest`) also benefits from being in-tree: it uses already-exported functions like `NewCompositedCompilerForTypeChecking()`, `CreateNamespaceObject()`, `BuildRequestType()`, and `BuildNamespaceType()` without duplication. A standalone module outside the tree would have to either get these internals exported or duplicate the logic and risk drift. The trade-off is that the package inherits `k8s.io/apiserver`'s large dependency tree, but since it is imported only in `*_test.go` files, this does not affect production binaries.

The library ships with K8s releases, stays close to the CEL environment source code it wraps, and can use internal types directly. If DRA support (Phase 3) requires cross-module dependencies, the DRA-specific evaluator will live in `k8s.io/dynamic-resource-allocation/cel/testing`.

**2. CLI tool → `kubernetes-sigs/cel-test` (Phase 1b)**

A standalone CLI that discovers and runs `*_test.cel` files. The CLI delegates all CEL evaluation logic to the Go library; the CLI itself handles command/flag parsing (via `cobra`), file discovery, output formatting (text, JSON, JUnit XML), and `.celtest.yaml` config loading. It lives in a separate `kubernetes-sigs/cel-test` repo with its own release cycle, following the precedent of `kubernetes-sigs/kubectl-validate` and `kubernetes-sigs/kubetest2`.

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

Policy source files use the `*.cel` extension (lightweight format) or standard Kubernetes resource YAML (`*.yaml`/`*.yml`). Test files use the `*_test.cel` suffix convention — matching Go's `*_test.go` and OPA's `*_test.rego` patterns. A test file is paired with its policy file by base name: `foo.cel` (or `foo.yaml`) is tested by `foo_test.cel` in the same directory. A test file can also use `source:` to explicitly reference any policy file.

**File naming convention:**

| File | Role | Example |
|---|---|---|
| `*.cel` | Policy source — lightweight format (variables + validations) | `privileged.cel` |
| `*.yaml` / `*.yml` | Policy source — native K8s resource (VAP, Kyverno, etc.) | `policy.yaml` |
| `*_test.cel` | Test file for the same-named policy | `privileged_test.cel` |

**Discovery rules:**
1. Walk the directory tree for `*_test.cel` files
2. For each `foo_test.cel`, look for a companion policy in the same directory:
   - First check for `foo.cel` (lightweight format)
   - Then check for `foo.yaml` or `foo.yml` (native K8s resource format)
3. If a companion file exists → load it as the policy under test (format auto-detected, see below)
4. If no companion file exists → the test file must have `mode: expression` (standalone, no policy needed)
5. Alternatively, the test file can specify `source: path/to/policy.yaml` to explicitly reference a policy file (overrides auto-discovery)

**Policy source format auto-detection:**

The runner accepts policy source in **two formats**, auto-detected by inspecting the parsed YAML:

| Format | Detection | Extraction | Example file |
|---|---|---|---|
| **Lightweight `src.cel`** | No `apiVersion` or `kind` field; top-level `variables:` and/or `validations:` keys | Used as-is — `variables:` → `VAPPolicy.Variables`, `validations:` → `VAPPolicy.Validations` | `src.cel` |
| **Native K8s resource YAML** | Has `apiVersion` and `kind` fields | Variables and validations extracted based on resource type (see table below) | `policy.yaml` |

**Supported native K8s resource types (Phase 1):**

| `apiVersion` | `kind` | Variables path | Validations path |
|---|---|---|---|
| `admissionregistration.k8s.io/v1` or `v1beta1` or `v1alpha1` | `ValidatingAdmissionPolicy` | `spec.variables` | `spec.validations` |
| `admissionregistration.k8s.io/v1alpha1` or `v1beta1` | `MutatingAdmissionPolicy` | `spec.variables` | `spec.mutations` (MAP expressions) |


This follows OPA's model: `opa test` doesn't know about Conftest's policy structure or any downstream tool's format. Each tool handles its own format.

The lightweight `src.cel` format remains the recommended format for Gatekeeper libraries, Kyverno policies, and any non-VAP/MAP use case where the policy source is not a core K8s resource.

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
// One line discovers and runs all *_test.cel files (raw mode, no preamble)
func TestCELPolicies(t *testing.T) {
    celtest.DiscoverAndRunTestsRaw(t, "../../src")
}
```

**Assertion options:**

| Field | Applies to | Description |
|---|---|---|
| `expect.value` | variable, expression | Exact value match |
| `expect.size` | variable, expression | List/map/string length |
| `expect.contains` | variable, expression | If result is a string, checks substring containment (`strings.Contains`). If result is a list, checks element containment (`slices.Contains`). Other result types produce an error. |
| `expect.allowed` | whole-policy | Validation pass/fail |
| `expect.messageContains` | whole-policy | Violation message check |
| `expect.error` | all | Expect an evaluation error |
| `expect.errorContains` | all | Error message substring |

#### Formal YAML Schema

The complete schema for `*_test.cel` files:

```yaml
# TestFile schema
mode: string          # Optional. "policy" (default) or "expression".
                      # "policy": requires companion policy file (auto-discovered or via source:).
                      #   Supports variable, expression, and whole-policy tests.
                      # "expression": self-contained, each test evaluates its expression field directly.
                      #   - variable: tests are FORBIDDEN (no policy variables exist).
                      #   - Whole-policy tests (expect.allowed) are FORBIDDEN.
                      #   - No companion policy file is needed.
                      #   - object, oldObject, params, request inputs are all available.
source: string        # Optional. Explicit path to the policy source file (relative to test file).
                      # Overrides auto-discovery. Supports both lightweight .cel format and
                      # native K8s resource YAML (auto-detected via apiVersion/kind).
                      # Example: source: ../policy.yaml
                      # Example: source: my-vap.yaml
feature: string       # Optional. CEL context/environment to use. Default: "admission".
                      # Values: "admission" (VAP/MAP/matchConditions), "crd", "dra",
                      #   "authn-claims", "authn-user", "authz".
                      # In Phase 1, only "admission" is implemented; other values
                      # produce a "not yet supported" error.
                      # When "crd" is set (Phase 2+): object → self, oldObject → oldSelf.
tests:                # Required. Array of TestCase, minimum 1.
  - name: string      # Required. Unique within file. Used as Go subtest name.

    # --- Test type (mutually exclusive, pick at most one) ---
    variable: string   # Test a specific named variable from the policy. Only in mode: policy.
    expression: string # Test an arbitrary CEL expression. Works in both modes.
    # (neither):       # Test the whole policy (allowed/denied). Only in mode: policy.

    # --- Input ---
    object: map        # Optional. The object being admitted/validated.
    oldObject: map     # Optional. Previous version (for UPDATE/DELETE operations).
    params: map        # Optional. Policy parameters. Passed as-is to the CEL environment.
    request: map       # Optional. Request metadata. Merged onto DefaultAdmissionRequest
                       #   (which includes operation, kind, resource, name, namespace,
                       #   userInfo, dryRun, options — all at zero values with operation
                       #   defaulting to "CREATE"). User-specified fields take precedence.

    # --- Assertions ---
    expect:            # Required.
      # For variable and expression tests:
      value: any       # Optional. Exact value match (with numeric normalization).
      size: int        # Optional. Length of list, map, or string result.
      contains: string # Optional. Dispatch by result type:
                       #   - string result → substring match (strings.Contains).
                       #   - list result → element containment (slices.Contains).
                       #   - other types → error.

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

For standalone CEL expressions that don't use the variables/validations policy structure, set `mode: expression` (see schema above for semantics). Example:

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

### Core API

```go
package celtest

import (
    "k8s.io/apimachinery/pkg/util/version"
)

// Evaluator compiles and evaluates CEL expressions using the real K8s
// CEL environment (environment.MustBaseEnvSet). It supports version-pinning,
// preamble variables (for framework injection), and admission-style evaluation.
//
// Phase 1 focuses on admission-style CEL (VAP, MAP, matchConditions).
// Future phases will extend the evaluator or introduce feature-specific
// evaluator types for other CEL contexts:
//
//   - Evaluator (Phase 1): VAP, MAP, matchConditions
//   - CRDEvaluator (Phase 2, separate KEP): CRD x-kubernetes-validations
//   - DRAEvaluator (Phase 3, separate KEP): DRA device selectors
//   - AuthNEvaluator / AuthZEvaluator (Phase 4, separate KEP): AuthN/AuthZ
//
// All evaluators share environment.MustBaseEnvSet(ver) and WithVersion semantics.
// This KEP defines only Evaluator. Future evaluators will be designed
// in their respective KEPs with input from the teams that own those features.
type Evaluator struct {
    envSet           *environment.EnvSet
    version          *version.Version
    preambleVars     []Variable  // framework-injected variables evaluated before policy variables
}

// Option configures an Evaluator.
type Option func(*Evaluator)

// WithVersion sets the K8s compatibility version (default: latest).
func WithVersion(major, minor uint) Option { ... }

// WithoutPatchTypes disables the MAP extension (library.JSONPatch and
// mutation.DynamicTypeResolver). Use this when testing pure VAP expressions
// to ensure CompileCheck rejects accidental use of MAP-only features like
// jsonPatch.escape(). By default, the MAP extension is enabled.
func WithoutPatchTypes() Option { ... }

// NewEvaluator creates an evaluator for admission-style CEL expressions
// (VAP, MAP, matchConditions). Internally calls environment.MustBaseEnvSet() and
// extends with admission-style variables and types via CreateTestEnv(), which
// delegates to the unexported createEnvForOpts() in the same package.
//
// The environment includes the MAP extension by default (HasPatchTypes: true).
// Use WithoutPatchTypes() to restrict the environment to pure VAP/matchConditions.
// MAP mutation *application* (patching objects) is not supported.
func NewEvaluator(opts ...Option) (*Evaluator, error) { ... }

// ========== VAP / MAP / matchConditions ==========

// AdmissionInput represents input for admission-style CEL evaluation.
// Used by EvalAdmission for VAP, MAP, and matchConditions.
//
// Fields map to CEL variables following the same conventions as the K8s API server
// (see k8s.io/apiserver/pkg/admission/plugin/cel/activation.go evaluationActivation):
//
//   | AdmissionInput field | CEL variable            | CEL compile-time type              | Default if nil                                  |
//   |----------------------|-------------------------|------------------------------------|--------------------------------------------------|
//   | Object               | object                  | DynType                            | nil (null in CEL)                                |
//   | OldObject            | oldObject               | DynType                            | nil (null in CEL)                                |
//   | Params               | params                  | DynType                            | nil (null in CEL)                                |
//   | Request              | request                 | kubernetes.AdmissionRequest        | Minimal request with Operation="CREATE"           |
//   | Namespace            | namespaceObject         | kubernetes.Namespace               | nil (null in CEL)                                |
//
// Request and Namespace use typed Go structs (*admissionv1.AdmissionRequest and
// *corev1.Namespace respectively) rather than raw maps. The evaluator internally
// converts these to unstructured maps for CEL binding, matching the production
// API server's conversion path. This provides compile-time type safety for test
// authors while maintaining evaluation fidelity.
//
// If Request is nil, a minimal *admissionv1.AdmissionRequest with
// Operation="CREATE" is synthesized. If Namespace is nil, the namespaceObject
// CEL variable resolves to null.
//
// authorizer and authorizer.requestResource are not declared in Phase 1.
// Expressions referencing `authorizer` will fail at compile time with an
// "undeclared reference" error. Mock authorizer support will be added in
// Phase 5 via a `WithAuthorizer(mock)` option.
type AdmissionInput struct {
    Object    map[string]interface{}            // → CEL `object` (DynType)
    OldObject map[string]interface{}            // → CEL `oldObject` (DynType)
    Params    map[string]interface{}            // → CEL `params` (DynType)
    Request   *admissionv1.AdmissionRequest     // → CEL `request` (kubernetes.AdmissionRequest). Default: Operation="CREATE"
    Namespace *corev1.Namespace                 // → CEL `namespaceObject` (kubernetes.Namespace). Default: nil (null)
}

// VAPPolicy represents a parsed VAP-style policy (variables + validations).
type VAPPolicy struct {
    Variables   []Variable
    Validations []Validation
}

// AdmissionResult holds the evaluation outcome.
type AdmissionResult struct {
    Allowed    bool
    Violations []Violation
    Cost       int64  // total CEL evaluation cost in K8s cost units
}

type Violation struct {
    Expression string
    Message    string
    Error      error
}

> Cost tracking uses the same `cel.OptTrackCost` and `PerCallLimit` as the K8s API server. The `Cost` field reports the total CEL evaluation cost in K8s cost units. This helps policy authors detect expressions that may exceed the runtime cost budget before deploying to a cluster. The `AdmissionResult.Cost` field and `WithCostLimit` option are defined in Phase 1a; cost tracking implementation ships in Phase 1a with the default of no limit (cost is reported but never causes failures).

// WithCostLimit sets a **shared** CEL cost budget for the entire EvalAdmission call,
// matching the API server's runtimeCELCostBudget behavior in ForInput().
//
// The budget is consumed sequentially across the complete evaluation:
//   1. Preamble variable evaluation (cost deducted from budget)
//   2. Policy variable evaluation (cost deducted from remaining budget)
//   3. Validation expression evaluation (cost deducted from remaining budget)
//
// If the budget is exhausted mid-evaluation, remaining expressions receive a
// cost-exceeded error — identical to the API server's behavior where
// runtimeCELCostBudget is decremented across all expressions in a single
// admission evaluation.
//
// Default: no limit (cost is tracked and reported in AdmissionResult.Cost
// but never causes failures).
//
// Use celtest.PerCallLimit for the K8s production limit:
//   eval, _ := celtest.NewEvaluator(celtest.WithCostLimit(celtest.PerCallLimit))
//
// The K8s production limit is defined in k8s.io/apiserver/pkg/apis/cel/config.go.
func WithCostLimit(limit int64) Option { ... }

// PerCallLimit is the K8s API server's per-expression cost limit.
// Expressions exceeding this limit are rejected at runtime.
const PerCallLimit = celconfig.PerCallLimit  // currently 1,000,000 (from k8s.io/apiserver/pkg/apis/cel)

// EvalAdmission evaluates a VAP/MAP/matchCondition policy against admission input.
func (e *Evaluator) EvalAdmission(policy *VAPPolicy, input *AdmissionInput) (*AdmissionResult, error) { ... }

// ========== Future Feature-Specific Evaluators ==========
//
// Future CEL contexts (CRD, DRA, AuthN/AuthZ) will get their own evaluator types
// in separate KEPs — see Evaluator doc comment above for the planned list.
// In the meantime, EvalExpression() provides basic testing for any CEL expression
// using the admission-style environment with DynType variables.

// ========== Common / Cross-Feature ==========

// EvalExpression evaluates a single CEL expression in the evaluator's environment.
// Uses the same AdmissionInput as EvalAdmission — all CEL variables (object, oldObject,
// params, request, namespaceObject) are available. extraVars allows injecting additional
// variables into the activation (e.g., for testing sub-expressions that reference
// computed variables).
//
// extraVars are injected as top-level activation bindings, alongside the standard
// admission variables. They do NOT go into the `variables.*` namespace automatically.
// To simulate a computed variable that another expression references as
// `variables.containers`, inject it under the key `variables.containers`:
//
//   result, err := eval.EvalExpression(
//       `variables.containers.filter(c, c.name == "nginx")`,
//       &celtest.AdmissionInput{Object: myObj},
//       map[string]interface{}{
//           "variables.containers": []interface{}{...},
//       },
//   )
func (e *Evaluator) EvalExpression(expr string, input *AdmissionInput, extraVars map[string]interface{}) (interface{}, error) { ... }

// CompileCheck validates that a CEL expression compiles without errors.
// Uses the NewExpressions environment mode, which restricts available libraries
// to those present at the configured compatibility version — e.g., WithVersion(1, 28)
// makes ip() fail to compile because it was introduced in 1.30.
// Evaluation methods (EvalAdmission, EvalExpression) use the StoredExpressions
// environment mode, which permits libraries available in any version up to the
// binary's built-in version — matching the API server's behavior for
// already-persisted expressions (ensuring rollback safety).
// Returns a descriptive error including the CEL compiler's error message and position, or nil if valid.
//
// > **Important:** Because CompileCheck uses NewExpressions mode and EvalExpression
// > uses StoredExpressions mode, it is possible for an expression to *fail*
// > CompileCheck but *succeed* in EvalExpression with the same evaluator and version.
// > This happens when the expression uses a library introduced after the configured
// > compatibility version. For example, with WithVersion(1, 28):
// >
// >   eval.CompileCheck(`ip("10.0.0.1")`)       // ERROR: ip() not available at 1.28
// >   eval.EvalExpression(`ip("10.0.0.1")`, ...) // OK: StoredExpressions allows ip()
// >
// > This is intentional and matches the API server's dual-mode behavior: new
// > expressions are validated strictly, while already-stored expressions are
// > evaluated permissively for rollback safety.
func (e *Evaluator) CompileCheck(expr string) error { ... }

// ParseVAPPolicy parses a VAP policy from the lightweight YAML format
// (top-level variables: / validations: keys, no apiVersion/kind).
func ParseVAPPolicy(yamlContent string) (*VAPPolicy, error) { ... }

// ParseVAPPolicyFile reads and parses a lightweight VAP policy YAML file.
func ParseVAPPolicyFile(path string) (*VAPPolicy, error) { ... }

// ParsePolicySource auto-detects the format of a policy source file and extracts
// a VAPPolicy. Inspects the parsed YAML for apiVersion/kind fields:
//   - If present: extracts variables/validations from a core K8s admission resource
//     (ValidatingAdmissionPolicy, MutatingAdmissionPolicy)
//   - If absent: delegates to ParseVAPPolicy (lightweight format)
//
//
// Supported: ValidatingAdmissionPolicy (v1/v1beta1/v1alpha1),
//            MutatingAdmissionPolicy (v1alpha1/v1beta1).
// Unknown apiVersion/kind combinations return a descriptive error.
func ParsePolicySource(yamlContent string) (*VAPPolicy, error) { ... }

// ParsePolicySourceFile reads a file and delegates to ParsePolicySource.
func ParsePolicySourceFile(path string) (*VAPPolicy, error) { ... }

// EvalVariable evaluates a single named variable from a policy, given input.
// This evaluates preamble vars and all policy vars up to and including the target.
// This enables per-variable testing — the primary value add over whole-policy testing.
func (e *Evaluator) EvalVariable(policy *VAPPolicy, variableName string, input *AdmissionInput) (interface{}, error) { ... }

// ========== Declarative Test Runner ==========

// DiscoverAndRunTestsWithEvaluator walks srcRoot for *_test.cel files and runs
// them using the provided evaluator. wrapParams controls whether test case params
// are automatically wrapped in {"spec": {"parameters": <params>}} for frameworks
// like Gatekeeper that expect this structure.
func DiscoverAndRunTestsWithEvaluator(t *testing.T, eval *Evaluator, srcRoot string, wrapParams bool) { ... }

// DiscoverAndRunTestsRaw walks srcRoot for *_test.cel files and runs them
// without preamble variables.
func DiscoverAndRunTestsRaw(t *testing.T, srcRoot string) { ... }

// RunTestFileWithEvaluator runs a single test file with a custom evaluator.
// This enables custom framework integration to reuse the declarative test runner.
func RunTestFileWithEvaluator(t *testing.T, eval *Evaluator, testFilePath string, wrapParams bool) { ... }
```

### Framework Adaptation: Preamble Variables and Runner Variants

Policy frameworks like Gatekeeper and Kyverno inject runtime-computed variables and transform parameter structures before CEL evaluation. The test tooling must replicate this to produce accurate results.

**Preamble Variables** — CEL expressions evaluated before the policy's own variables, injecting framework-specific bindings. Configured via `WithPreambleVariables`.

Parameter wrapping (e.g., Gatekeeper wrapping user parameters inside `{spec: {parameters: <userParams>}}`) is a framework-specific concern handled downstream. The Gatekeeper preamble `params` expression (`params.spec.parameters`) already unwraps parameters in CEL, so test input just needs to include the full constraint structure. No Go-level wrapping is needed in the upstream API.

The upstream package (`k8s.io/apiserver/pkg/cel/testing/celtest`) provides two generic runners:

| Runner | Preamble | Use case |
|---|---|---|
| `DiscoverAndRunTestsWithEvaluator(t, eval, root)` | Custom (via evaluator) | Any framework |
| `DiscoverAndRunTestsRaw(t, root)` | None | Vanilla VAP, Kyverno, standalone |

For custom frameworks, use `RunTestFileWithEvaluator` with a custom evaluator that has the framework's preamble variables set via `WithPreambleVariables`. Framework-specific convenience wrappers (e.g., Gatekeeper's preamble injection and parameter wrapping) are the responsibility of downstream packages — see [Downstream Requirements](#downstream-requirements). Parameter wrapping, if needed, should also be handled downstream.

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

### Downstream Requirements

The upstream `k8s.io/apiserver/pkg/cel/testing/celtest` package provides the generic, framework-agnostic core. Downstream projects (such as `kubernetes-sigs/cel-test` for the CLI, or framework-specific test harnesses) are expected to provide the following on top of the upstream package:

- **Framework-specific preamble variable definitions.** For example, Gatekeeper requires `anyObject` (unified object/oldObject access for DELETE operations) and `params` (unwrapping the constraint CRD's `spec.parameters` structure). These preamble expressions are framework-specific CEL code that should not live in `k8s.io/apiserver`.
- **Convenience runner wrappers that pre-configure the evaluator with the correct preamble.** For example, a `DiscoverAndRunTests(t, root)` function that creates an evaluator with Gatekeeper preamble variables and delegates to the upstream `DiscoverAndRunTestsWithEvaluator`. Each framework (Gatekeeper, Kyverno, etc.) would provide its own such wrapper. Framework-specific parameter wrapping (e.g., Gatekeeper's `{spec: {parameters: ...}}` structure) should also be handled in downstream wrappers.
- **CLI tool implementation.** The `celtest` CLI binary wraps the upstream Go library with command-line argument parsing, output formatting (text, JSON, JUnit), configuration file loading (`.celtest.yaml`), and a `--config` flag for project configuration. The CLI lives in its own `kubernetes-sigs/cel-test` repository.
- **Framework-specific test examples and documentation.** Colocated `*_test.cel` files demonstrating how to test policies for each framework, along with instructions for CI integration (GitHub Actions, etc.).
- **Future framework-specific features.** If a framework needs additional test infrastructure beyond what `WithPreambleVariables` supports (e.g., custom resource resolution, external data injection, or framework-specific assertion helpers), those should be implemented downstream rather than added to the upstream core.

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
            want: 1, // expected length of the filtered list
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
            switch want := tt.want.(type) {
            case int:
                // EvalExpression returns a list — compare its length
                list, ok := result.([]interface{})
                if !ok {
                    t.Fatalf("expected list result, got %T", result)
                }
                if len(list) != want {
                    t.Errorf("got len %d, want %d", len(list), want)
                }
            default:
                if result != tt.want {
                    t.Errorf("got %v, want %v", result, tt.want)
                }
            }
        })
    }
}
```

### Comparison with Existing Tools

| Tool | Env Accuracy | API | Cluster | Scope |
|---|---|---|---|---|
| **This proposal** | ✅ Real K8s env | ✅ Simple Go API | ❌ No | Phase 1: Admission (VAP, MAP, matchConditions); Phases 2-4 planned: CRD, DRA, AuthN/AuthZ |
| gator CLI | ✅ Real K8s env | ⚠️ YAML suite files | ❌ No | Gatekeeper policies only (OPA + CEL templates) |
| kaptest | ⚠️ Third-party | ✅ Simple | ❌ No | VAP only |
| kubectl-validate (#130570) | ✅ Real K8s env | ⚠️ CLI tool | ❌ No | Schema validation |
| Custom `cel-go` (celeval) | ❌ Incomplete env | ✅ Simple Go API | ❌ No | Custom subset |

### CLI Tool Design

The `celtest` CLI (`kubernetes-sigs/cel-test`, Phase 1b) delegates all CEL evaluation to the Go library. The CLI handles command/flag parsing, file discovery, output formatting, and configuration. This section specifies its commands, flags, output format, and configuration.

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
| `--config` | `string` | none | Path to config file (see below). Auto-discovers `.celtest.yaml` in working directory if not specified. |
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
```

The `--config` flag can also point to this file explicitly:
```bash
celtest run src/ --config .celtest.yaml
```

Without `--config` or a `.celtest.yaml` in the working directory, the CLI runs in raw mode (equivalent to `DiscoverAndRunTestsRaw`).

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
- `NewEvaluator` with admission-style environment including MAP extension (`HasPatchTypes: true` by default — enables `library.JSONPatch` and `mutation.DynamicTypeResolver`)
- `EvalAdmission` for full VAP/MAP policy evaluation (validations returning allowed/denied)
- `EvalExpression` for single expression testing (including MAP mutation expressions — returns raw CEL value)
- `CompileCheck` for compilation validation (including MAP expressions like `jsonPatch.escape()`)
- `ParseVAPPolicy` / `ParseVAPPolicyFile` helpers
- `WithVersion` for K8s version pinning
- `WithPreambleVariables` for framework injection
- Declarative `*_test.cel` runner (`DiscoverAndRunTestsRaw`, `DiscoverAndRunTestsWithEvaluator`)
- **MAP support scope**: MAP expression compilation, variable testing, and validation evaluation are supported (see MAP extension row in Risks table). MAP mutation *application* (applying patches to produce a mutated object) is deferred to a future phase.
- **Note**: VAP, MAP, and matchConditions share the same base admission env (`k8s.io/apiserver/pkg/admission/plugin/cel/compile.go`) with different `OptionalVariableDeclarations` flags (`HasParams`, `HasAuthorizer`, `HasPatchTypes`). The evaluator enables `HasParams` and `HasPatchTypes` by default. `HasAuthorizer` is **not** enabled in Phase 1 — expressions referencing `authorizer` will produce a compile-time "undeclared reference" error, which is clearer than a runtime null dereference. Mock authorizer support will be added in Phase 5 via a `WithAuthorizer(mock)` option.

#### Phase 1b: CLI Tool (`kubernetes-sigs/cel-test`)

Wraps the upstream Go library in a standalone CLI. See [CLI Tool Design](#cli-tool-design) for commands, flags, output formats, and configuration. Installable via `go install sigs.k8s.io/cel-test/cmd/celtest@latest`.

#### Phase 2: CRD Validation Rules (separate follow-up KEP)
- `CRDEvaluator` with `EvalRule()` method, `self` / `oldSelf` variables, required OpenAPI schema input
- Schema-aware type checking and transition rule support
- CRD env is built in `k8s.io/apiextensions-apiserver` via `prepareEnvSet()` — whether the evaluator takes a dependency on that module or reimplements will be resolved in the Phase 2 KEP

#### Phase 3: DRA Device Selectors (separate follow-up KEP)
- `DRAEvaluator` with `EvalSelector()` method, typed `DRADevice` variable
- Lives in `k8s.io/dynamic-resource-allocation/cel/testing` due to cross-module boundary

#### Phase 4: Authentication & Authorization (separate follow-up KEP)
- `AuthNClaimsEvaluator`, `AuthNUserEvaluator`, `AuthZEvaluator` — separate evaluator types following the per-feature pattern

#### Phase 5: Advanced Features
- Authorizer mock support, `testing.T` assertion helpers, benchmark support for cost comparison

### Graduation Criteria

#### Alpha (Phase 1a-1b)
- `EvalAdmission` fully functional for VAP/MAP/matchConditions validation evaluation (MAP mutation application deferred — see Phase 1a MAP support scope).
- `EvalExpression` and `CompileCheck` for expression-level testing
- `WithPreambleVariables` supports framework injection (Gatekeeper validated)
- Package located in `k8s.io/apiserver/pkg/cel/testing/celtest`
- `celtest` CLI available via `kubernetes-sigs/cel-test` with `run`, `compile`, `eval` commands
- Unit tests cover admission evaluation, version pinning, and preamble variable ordering

#### Beta
- `Evaluator` API stable for VAP/MAP/matchConditions
- Declarative `*_test.cel` format with `DiscoverAndRunTestsWithEvaluator` / `DiscoverAndRunTestsRaw` runners
- CLI supports admission-style evaluation with all output formats (text, JSON, JUnit)
- Adopted by at least 1 external project (e.g., gatekeeper-library) for CEL policy testing
- Integration test validating evaluation loop equivalence with upstream `ForInput()` path

#### GA (Stable)
- Adopted by 2+ projects outside of gatekeeper-library
- `Evaluator` API stable with no breaking changes for 2 K8s releases
- Cost tracking matches production behavior
- Documentation published on kubernetes.io

> **Note:** Phases 2–4 (CRD, DRA, AuthN/AuthZ) will be proposed as separate follow-up KEPs with independent graduation criteria.

### Open Questions for sig-api-machinery


1. **Should AuthN's dual-env pattern (claims vs user) be modeled as two methods on one evaluator or two separate evaluator types?**
   **Proposed: Two separate evaluator types** (`AuthNClaimsEvaluator`, `AuthNUserEvaluator`) following the per-feature evaluator pattern. To be designed in the Phase 4 KEP.

2. **How should CRD schema-typed `self`/`oldSelf` be handled — require OpenAPI schema input, or support untyped `DynType` for simpler testing?**
   **Proposed: Require OpenAPI schema.** Schema-typed `self` is the primary value of CRD validation testing — without it, type errors (the most common CRD validation bugs) cannot be caught. CRD authors already have their schema in their CRD YAML. For quick untyped testing, `EvalExpression()` with the admission environment is sufficient.

3. **Should this package provide assertion helpers (`celtest.RequireAllowed(t, result)`) or keep it minimal?**
   **Proposed: Keep the core API minimal.** Assertion helpers (`celtest.RequireAllowed`) can be added in Phase 5 or by downstream projects.

4. **What's the right level of cost tracking granularity to expose in test results?**
   **Proposed: Expose total cost as `int64` on result types.** Per-expression cost breakdown is future work.

5. **How should feature gates that affect CEL environment construction be handled in the test package?**
   Some CEL contexts include types or variables gated by K8s feature gates — e.g., AuthZ's `fieldSelector`/`labelSelector` types are only present when `AuthorizeWithSelectors` is enabled, and DRA's `allowMultipleAllocations` field is gated by `ConsumableCapacity`. These gates are checked at environment construction time (in `mustBuildEnv()`/`newCompiler()`), not at evaluation time, so `WithVersion()` alone cannot toggle them.
   **Proposed: Note as a known limitation for Phase 1.** The admission evaluator (Phase 1) is not affected — admission `OptionalVariableDeclarations` flags (`HasParams`, `HasPatchTypes`) are controlled by the evaluator, not by feature gates. `HasAuthorizer` is not enabled in Phase 1 (see Phase 1a note). The Phase 4 KEP (AuthN/AuthZ) and Phase 3 KEP (DRA) will need to address this — likely by enabling the superset environment by default (matching the pattern of `HasPatchTypes: true` in Phase 1) or by adding a `WithFeatureGates()` option.

## Test Plan

Unit tests for the `celtest` package itself will cover:

- **Admission environment construction**: `NewEvaluator` creates the correct admission CEL environment with the expected variables and types available.
- **Version gating**: `WithVersion(1, 28)` correctly makes `ip()` unavailable (introduced in 1.30); `WithVersion(1, 34)` makes all current libraries available.
- **Preamble variable ordering**: Preamble variables are evaluated before policy variables; policy variables can reference preamble results via `variables.*`.
- **EvalAdmission correctness**: Known-good and known-bad policies produce the expected `Allowed`/`Violations` results.
- **CompileCheck**: Valid expressions return no errors; invalid expressions return descriptive compile errors.
- **Declarative runner**: `DiscoverAndRunTestsRaw` correctly discovers `*_test.cel` files, parses YAML test cases, and reports pass/fail per test case.
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
