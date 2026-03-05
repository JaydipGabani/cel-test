# Copilot Instructions for cel-test

## Project Overview

This repository implements a **CEL (Common Expression Language) test tooling framework for Kubernetes**. It provides:

1. A **Go testing library** (`celtest`) that wraps real Kubernetes CEL environments for unit-testing admission policies, CRD validation rules, DRA selectors, and auth expressions without a running cluster.
2. A **standalone CLI tool** (`celtest`) that discovers and runs declarative `*_test.cel` YAML test files so policy authors can test CEL locally without writing Go.

The project targets integration into `k8s.io/apiserver` (Go library) and `kubernetes-sigs/cel-test` (CLI wrapper).

## Repository Structure

```
cel-test-tooling-proposal.md   # KEP / design document
examples/                      # Example policies and tests by framework
  dra-gpu-selector/            # DRA device-selector expressions
  kyverno-disallow-latest/     # Kyverno CEL policy
  vanilla-vap-replica-limit/   # Pure Kubernetes VAP (no framework)
testdata/                      # Complex test data (e.g., Gatekeeper library)
```

## File Conventions

| Pattern | Purpose |
|---|---|
| `src.cel` | Policy source — YAML with `variables:` and `validations:` |
| `src_test.cel` | Declarative test file — YAML with `tests:` array |
| `policy.yaml` | Optional reference Kubernetes resource |

Discovery rule: `foo_test.cel` auto-pairs with `foo.cel` in the same directory. If no companion `.cel` exists, the test file must declare `mode: expression`.

## CEL Policy File Format (`src.cel`)

Policy files use YAML with two top-level keys:

- **`variables:`** — array of `{name, expression}` objects defining named CEL variables. Variables may reference `object`, `oldObject`, `params`, `request`, and other variables via `variables.<name>`.
- **`validations:`** — array of `{expression, messageExpression}` objects defining admission rules.

Gatekeeper policies also reference preamble variables like `variables.anyObject` and `variables.params`.

## CEL Test File Format (`src_test.cel`)

Test files use YAML with:

- **`mode:`** — `"policy"` (default) or `"expression"` (standalone CEL)
- **`tests:`** — array of test cases, each containing:
  - `name:` — unique descriptive test name
  - Test target (mutually exclusive): `variable:` (single named variable), `expression:` (arbitrary CEL), or omitted (whole-policy test)
  - Inputs: `object:`, `oldObject:`, `params:`, `request:` maps
  - `expect:` — assertions using: `value`, `size`, `contains`, `allowed`, `messageContains`, `error`, `errorContains`

### Three Testing Levels

| Level | When to use | Key assertions |
|---|---|---|
| **Per-variable** | Testing individual computed variables in isolation | `variable:` + `expect.value` / `expect.size` / `expect.contains` |
| **Whole-policy** | Testing full allow/deny outcomes | `expect.allowed` + `expect.messageContains` |
| **Per-expression** | Testing arbitrary standalone CEL expressions | `expression:` + `expect.value` |

## Go Library Usage

```go
import "k8s.io/apiserver/pkg/cel/testing/celtest"

// Gatekeeper-style (with preamble)
func TestGatekeeperPolicies(t *testing.T) {
    celtest.DiscoverAndRunTests(t, "src/")
}

// Vanilla VAP / Kyverno (no preamble)
func TestVAPPolicies(t *testing.T) {
    celtest.DiscoverAndRunTestsRaw(t, "src/")
}
```

## CLI Usage

```bash
celtest run src/...              # Discover and run all tests
celtest compile src/policy.cel   # Type-check without running
celtest eval 'expr' --input x.yaml  # Evaluate a single expression
```

## Coding Guidelines

- **Language:** Go, following standard Go conventions (`gofmt`, `go vet`).
- **Dependencies:** `k8s.io/apiserver`, `k8s.io/apimachinery`, `github.com/google/cel-go` (indirectly via apiserver).
- **Test naming:** Use descriptive strings (e.g., `"badContainers catches :latest"`, `"allows on UPDATE regardless"`).
- **Comments:** Include comment headers in `.cel` files describing origin and purpose. Use `# ---- Section ----` separators in test files.
- **Naming:** Follow Go `*_test.go` convention — test CEL files are always `*_test.cel`.
- **Runner flavors:** `DiscoverAndRunTests` (Gatekeeper), `DiscoverAndRunTestsRaw` (vanilla/Kyverno), `RunTestFileWithEvaluator` (custom evaluator).

## Kubernetes CEL Contexts Covered

The framework supports seven Kubernetes CEL use cases across implementation phases:

1. **ValidatingAdmissionPolicy (VAP)** — `object`, `oldObject`, `params`, `request`, `authorizer`
2. **MutatingAdmissionPolicy (MAP)** — same inputs plus mutation semantics
3. **CRD validation rules** — `self`, `oldSelf` transition rules
4. **matchConditions** — admission webhook match expressions
5. **DRA device selectors** — `device.attributes`, `device.driver`, custom Semver type
6. **Authentication (AuthN)** — user claim mappings
7. **Authorization (AuthZ)** — structured authorization expressions
