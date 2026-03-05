# Copilot Instructions for cel-test

## Project Overview

This is a **CEL (Common Expression Language) test tooling** project for Kubernetes. It provides:

1. A **Go package** (`celeval`) that wraps the upstream `k8s.io/apiserver/pkg/cel/testing/celtest` package for evaluating CEL expressions using the real Kubernetes CEL environment.
2. A **declarative test format** (`*_test.cel`) — YAML files colocated with policy source — so policy authors can test CEL expressions without writing Go.

The project supports testing CEL expressions used in ValidatingAdmissionPolicy (VAP), Gatekeeper, Kyverno, DRA device selectors, and other Kubernetes CEL features.

## Architecture

### Package Layout

- **`celeval/`** — Core Go package. Thin wrapper around the vendored upstream `celtest` package. Adds YAML parsing for policy files (`src.cel`) and declarative test files (`*_test.cel`).
  - `celeval.go` — Re-exports upstream types (`Evaluator`, `VAPPolicy`, `AdmissionInput`, etc.) and adds YAML parsing (`ParseVAPPolicy`, `ParseVAPPolicyFile`).
  - `testfile.go` — Declarative test file parser (`ParseTestFile`) and test runner (`RunTestFile`, `RunTestFileRaw`, `RunTestFileWithEvaluator`, `DiscoverAndRunTests`).
  - `celeval_test.go` — Go API tests demonstrating `EvalAdmission`, `EvalExpression`, `CompileCheck`, `WithVersion`, `WithPreambleVariables`.
  - `testfile_test.go` — Discovers and runs `*_test.cel` files under `testdata/gatekeeper/`.
  - `examples_test.go` — Discovers and runs `*_test.cel` files under `examples/`.
- **`examples/`** — Example policies with colocated `src.cel` + `src_test.cel` files (vanilla VAP, Kyverno, DRA).
- **`testdata/gatekeeper/`** — Gatekeeper-style policies with `src.cel` + `src_test.cel` (uses Gatekeeper preamble variables).
- **`vendor/k8s.io/apiserver/pkg/cel/testing/celtest/`** — Vendored upstream package (the "real" evaluator). This is the code that would eventually live in `k8s.io/apiserver`.

### Key Types (re-exported from upstream `celtest`)

- `Evaluator` — Compiles and evaluates CEL using the real K8s CEL env with `admissioncel.NewCompositedCompiler`.
- `VAPPolicy` — A parsed policy: `[]Variable` + `[]Validation`.
- `AdmissionInput` — Test input: `Object`, `OldObject`, `Params`, `Request`, `Namespace`.
- `AdmissionResult` — Evaluation result: `Allowed`, `Violations`, `Cost`.
- `Option` — Evaluator options: `WithVersion(major, minor)`, `WithPreambleVariables(...)`, `WithCostLimit(...)`.

### Evaluator Methods

- `EvalAdmission(policy, input)` — Evaluate a full policy (all variables + validations). Returns `AdmissionResult`.
- `EvalVariable(policy, varName, input)` — Evaluate a single named variable. Returns its value.
- `EvalExpression(expr, input, extraVars)` — Evaluate an arbitrary CEL expression. Returns its value.
- `CompileCheck(expr)` — Compile-only check (no evaluation). Returns compilation errors.

### Gatekeeper vs Vanilla VAP

- **Gatekeeper policies** use `WithPreambleVariables(GatekeeperPreamble()...)` which injects `anyObject` and `params` variables. Gatekeeper params are wrapped in `spec.parameters` structure. Use `RunTestFile` / `DiscoverAndRunTests`.
- **Vanilla VAP / Kyverno policies** use no preamble. Params are passed directly. Use `RunTestFileRaw` / `DiscoverAndRunTestsRaw`.

## Declarative Test Format (`*_test.cel`)

Test files are YAML with this structure:

```yaml
mode: expression  # optional; "policy" (default) or "expression"
tests:
- name: "test name"
  # What to test (pick one):
  variable: varName       # test a specific variable's output
  expression: "CEL expr"  # test an arbitrary expression
  # (neither)             # test the whole policy (allowed/denied)

  # Input:
  object: { ... }
  oldObject: { ... }
  params: { ... }
  request:
    operation: CREATE

  # Assertions:
  expect:
    # For whole-policy tests:
    allowed: true/false
    messageContains: "substring"
    # For variable/expression tests:
    value: <expected value>
    size: <expected length>
    contains: "substring"
    # For error testing:
    error: true
    errorContains: "substring"
```

### Conventions

- Policy files are named `src.cel`. Test files are named `src_test.cel`.
- The test runner derives the policy path from the test path: `foo_test.cel` → `foo.cel`.
- `mode: expression` means no policy file is needed — each test provides its own `expression`.
- Variable and expression fields are mutually exclusive.

## Development Guidelines

### Go Version & Dependencies

- Go 1.25.1+. Module: `github.com/JaydipGabani/cel-test`.
- Key deps: `k8s.io/apiserver` v0.35.2, `github.com/google/cel-go` v0.27.0, `gopkg.in/yaml.v3`.
- All dependencies are vendored under `vendor/`.

### Running Tests

```bash
# Run all tests
go test ./celeval/ -v

# Run only Go API tests
go test ./celeval/ -run TestEvalExpression

# Run only declarative test file discovery (Gatekeeper policies)
go test ./celeval/ -run TestCELPolicies

# Run only example policies (vanilla VAP, Kyverno, DRA)
go test ./celeval/ -run TestExamplePolicies
```

### Adding a New Policy Example

1. Create a directory under `examples/` (for vanilla/Kyverno) or `testdata/gatekeeper/` (for Gatekeeper).
2. Write `src.cel` with `variables:` and `validations:` in YAML.
3. Write `src_test.cel` with test cases covering variable-level and policy-level assertions.
4. Existing discovery tests (`TestExamplePolicies` or `TestCELPolicies`) will automatically pick up the new files.

### Code Style

- The `celeval` package is intentionally thin — it re-exports upstream types and adds only YAML parsing + declarative test running.
- Do NOT duplicate upstream logic. If something belongs in the evaluator, it goes in `vendor/k8s.io/apiserver/pkg/cel/testing/celtest/celtest.go` (the vendored upstream).
- Test files (`*_test.cel`) should test at multiple levels: individual variables first, then whole-policy pass/fail.
- Use `t.Helper()` in all test helper functions.
- Numeric comparisons in `deepEqual` handle `int` vs `int64` vs `float64` coercion from YAML parsing.

### Important Patterns

- `ParseVAPPolicy` / `ParseVAPPolicyFile` handle YAML → `VAPPolicy` conversion since the upstream package doesn't include YAML parsing.
- `buildAdmissionInputFromTestCase` converts YAML test case maps into typed `AdmissionInput`, including `admissionv1.AdmissionRequest` conversion.
- `wrapParams` wraps raw params in Gatekeeper's `spec.parameters` CRD structure for Gatekeeper-mode tests.
- `DiscoverAndRunTests` / `DiscoverAndRunTestsRaw` glob for `*_test.cel` at two directory depths and auto-derive policy paths.

### Design Doc

The full KEP proposal is in `cel-test-tooling-proposal.md`. It covers motivation, user stories, architecture (shared base + feature extensions + preamble variables), and the implementation plan for upstream contribution to `k8s.io/apiserver`.
