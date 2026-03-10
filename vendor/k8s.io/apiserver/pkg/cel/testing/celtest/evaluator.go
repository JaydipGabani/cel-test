// Package celtest provides a CEL expression evaluator for testing Kubernetes
// CEL expressions (VAP, CRD, matchConditions, etc.) using the real K8s CEL
// environment without requiring a running cluster.
package celtest

import (
"fmt"
"strings"
"sync"

"github.com/google/cel-go/cel"
admissionv1 "k8s.io/api/admission/v1"
corev1 "k8s.io/api/core/v1"
admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
celconfig "k8s.io/apiserver/pkg/apis/cel"
"k8s.io/apiserver/pkg/cel/environment"
"k8s.io/apimachinery/pkg/util/version"
)

// Evaluator compiles and evaluates CEL expressions using the real K8s
// CEL environment (environment.MustBaseEnvSet). It supports version-pinning,
// preamble variables (for framework injection), and admission-style evaluation.
type Evaluator struct {
envSet       *environment.EnvSet
version      *version.Version
preambleVars []Variable
costLimit    int64

mu               sync.Mutex
compilationCache map[string]compiledEntry
}

type compiledEntry struct {
program cel.Program
err     error
}

// Option configures an Evaluator.
type Option func(*Evaluator)

// WithVersion sets the K8s compatibility version (default: current).
// The base environment is versioned — some libraries are only available from
// certain K8s versions (e.g., ip() from 1.30+, semver() from 1.33+).
func WithVersion(major, minor uint) Option {
return func(e *Evaluator) {
e.version = version.MajorMinor(major, minor)
}
}

// WithPreambleVariables registers CEL variable expressions that are evaluated
// BEFORE the policy's own variables. This enables framework-specific variable
// injection (e.g., Gatekeeper's anyObject/params unwrapping).
func WithPreambleVariables(vars ...Variable) Option {
return func(e *Evaluator) {
e.preambleVars = append(e.preambleVars, vars...)
}
}

// WithCostLimit sets the CEL cost budget for evaluation.
// Default: 0 (no limit — cost is tracked and reported but never causes failures).
func WithCostLimit(limit int64) Option {
return func(e *Evaluator) {
e.costLimit = limit
}
}

// PerCallLimit is the K8s API server's per-expression cost limit (1,000,000).
const PerCallLimit = celconfig.PerCallLimit

// Variable represents a named CEL expression (used for policy variables and preamble).
type Variable struct {
Name       string
Expression string
}

// Validation represents a CEL validation expression with an optional message.
type Validation struct {
Expression        string
Message           string // static message on failure
MessageExpression string // CEL expression for dynamic message
}

// VAPPolicy represents a parsed VAP-style policy (variables + validations).
type VAPPolicy struct {
Variables   []Variable
Validations []Validation
}

// AdmissionInput represents input for admission-style CEL evaluation.
// Fields map to CEL variables following the same conventions as the K8s API server
// (see k8s.io/apiserver/pkg/admission/plugin/cel/activation.go).
//
// Request is typed as *admissionv1.AdmissionRequest and internally converted to
// unstructured for CEL binding — matching the API server's path. If nil, a minimal
// request with Operation="CREATE" is synthesized.
//
// Namespace is typed as *corev1.Namespace and internally filtered to safe fields
// then converted to unstructured. If nil, namespaceObject resolves to null in CEL.
type AdmissionInput struct {
Object    map[string]interface{}          // -> CEL "object" (DynType)
OldObject map[string]interface{}          // -> CEL "oldObject" (DynType)
Params    map[string]interface{}          // -> CEL "params" (DynType)
Request   *admissionv1.AdmissionRequest   // -> CEL "request" (kubernetes.AdmissionRequest)
Namespace *corev1.Namespace               // -> CEL "namespaceObject" (kubernetes.Namespace)
}

// AdmissionResult holds the evaluation outcome.
type AdmissionResult struct {
Allowed    bool
Violations []Violation
Cost       int64 // total CEL evaluation cost in K8s cost units
}

// Violation describes a single validation failure.
type Violation struct {
Expression string
Message    string
Error      error
}

// NewEvaluator creates a CEL evaluator using the real K8s CEL environment.
func NewEvaluator(opts ...Option) (*Evaluator, error) {
e := &Evaluator{
compilationCache: make(map[string]compiledEntry),
}
for _, opt := range opts {
opt(e)
}
if e.version == nil {
e.version = version.MajorMinor(1, 35)
}

baseEnvSet := environment.MustBaseEnvSet(e.version, true)
extended, err := admissioncel.CreateTestEnv(baseEnvSet, admissioncel.OptionalVariableDeclarations{
HasParams: true,
})
if err != nil {
return nil, fmt.Errorf("creating test env: %w", err)
}
e.envSet = extended
return e, nil
}

func (e *Evaluator) programOptions() []cel.ProgramOption {
opts := []cel.ProgramOption{
cel.EvalOptions(cel.OptOptimize, cel.OptTrackCost),
cel.InterruptCheckFrequency(celconfig.CheckFrequency),
}
if e.costLimit > 0 {
opts = append(opts, cel.CostLimit(uint64(e.costLimit)))
}
return opts
}

func (e *Evaluator) compile(expr string) (cel.Program, error) {
e.mu.Lock()
if cached, ok := e.compilationCache[expr]; ok {
e.mu.Unlock()
return cached.program, cached.err
}
e.mu.Unlock()

env := e.envSet.StoredExpressionsEnv()
ast, issues := env.Compile(expr)
if issues != nil && issues.Err() != nil {
err := fmt.Errorf("CEL compilation error: %s", issues.Err())
e.mu.Lock()
e.compilationCache[expr] = compiledEntry{err: err}
e.mu.Unlock()
return nil, err
}
prog, err := env.Program(ast, e.programOptions()...)
if err != nil {
err = fmt.Errorf("CEL program creation error: %w", err)
e.mu.Lock()
e.compilationCache[expr] = compiledEntry{err: err}
e.mu.Unlock()
return nil, err
}
e.mu.Lock()
e.compilationCache[expr] = compiledEntry{program: prog}
e.mu.Unlock()
return prog, nil
}

func (e *Evaluator) compileWithVariables(expr string) (cel.Program, error) {
cacheKey := "withvars:" + expr
e.mu.Lock()
if cached, ok := e.compilationCache[cacheKey]; ok {
e.mu.Unlock()
return cached.program, cached.err
}
e.mu.Unlock()

env := e.envSet.StoredExpressionsEnv()
extEnv, err := env.Extend(cel.Variable("variables", cel.DynType))
if err != nil {
return nil, fmt.Errorf("extending env with variables: %w", err)
}
ast, issues := extEnv.Compile(expr)
if issues != nil && issues.Err() != nil {
cErr := fmt.Errorf("CEL compilation error: %s", issues.Err())
e.mu.Lock()
e.compilationCache[cacheKey] = compiledEntry{err: cErr}
e.mu.Unlock()
return nil, cErr
}
prog, err := extEnv.Program(ast, e.programOptions()...)
if err != nil {
cErr := fmt.Errorf("CEL program creation error: %w", err)
e.mu.Lock()
e.compilationCache[cacheKey] = compiledEntry{err: cErr}
e.mu.Unlock()
return nil, cErr
}
e.mu.Lock()
e.compilationCache[cacheKey] = compiledEntry{program: prog}
e.mu.Unlock()
return prog, nil
}

// buildActivation builds a TestActivation from AdmissionInput.
// Request and Namespace are converted from typed Go structs to unstructured,
// matching the production API server's conversion path.
func buildActivation(input *AdmissionInput) *admissioncel.TestActivation {
var request interface{}
if input.Request != nil {
// Convert typed AdmissionRequest to unstructured map, matching the
// production path in condition.go: CreateAdmissionRequest -> unstructured
request = admissionRequestToUnstructured(input.Request)
} else {
request = map[string]interface{}{"operation": "CREATE"}
}
var namespace interface{}
if input.Namespace != nil {
namespace = namespaceToUnstructured(input.Namespace)
}
return &admissioncel.TestActivation{
Object:    input.Object,
OldObject: input.OldObject,
Params:    input.Params,
Request:   request,
Namespace: namespace,
}
}

// admissionRequestToUnstructured converts *admissionv1.AdmissionRequest to map[string]interface{}.
func admissionRequestToUnstructured(req *admissionv1.AdmissionRequest) map[string]interface{} {
m := map[string]interface{}{}
if req.Operation != "" {
m["operation"] = string(req.Operation)
}
if req.Name != "" {
m["name"] = req.Name
}
if req.Namespace != "" {
m["namespace"] = req.Namespace
}
if req.DryRun != nil {
m["dryRun"] = *req.DryRun
}
if req.SubResource != "" {
m["subResource"] = req.SubResource
}
if req.UID != "" {
m["uid"] = string(req.UID)
}
if req.Kind.Kind != "" || req.Kind.Group != "" || req.Kind.Version != "" {
m["kind"] = map[string]interface{}{
"group":   req.Kind.Group,
"version": req.Kind.Version,
"kind":    req.Kind.Kind,
}
}
if req.Resource.Resource != "" || req.Resource.Group != "" || req.Resource.Version != "" {
m["resource"] = map[string]interface{}{
"group":    req.Resource.Group,
"version":  req.Resource.Version,
"resource": req.Resource.Resource,
}
}
if req.RequestKind != nil {
m["requestKind"] = map[string]interface{}{
"group":   req.RequestKind.Group,
"version": req.RequestKind.Version,
"kind":    req.RequestKind.Kind,
}
}
if req.RequestResource != nil {
m["requestResource"] = map[string]interface{}{
"group":    req.RequestResource.Group,
"version":  req.RequestResource.Version,
"resource": req.RequestResource.Resource,
}
}
if req.RequestSubResource != "" {
m["requestSubResource"] = req.RequestSubResource
}
if req.UserInfo.Username != "" {
userInfo := map[string]interface{}{
"username": req.UserInfo.Username,
}
if req.UserInfo.UID != "" {
userInfo["uid"] = req.UserInfo.UID
}
if len(req.UserInfo.Groups) > 0 {
groups := make([]interface{}, len(req.UserInfo.Groups))
for i, g := range req.UserInfo.Groups {
groups[i] = g
}
userInfo["groups"] = groups
}
m["userInfo"] = userInfo
}
return m
}

// namespaceToUnstructured converts *corev1.Namespace to its safe-field unstructured representation.
func namespaceToUnstructured(ns *corev1.Namespace) map[string]interface{} {
metadata := map[string]interface{}{
"name": ns.Name,
}
if ns.Namespace != "" {
metadata["namespace"] = ns.Namespace
}
if len(ns.Labels) > 0 {
labels := map[string]interface{}{}
for k, v := range ns.Labels {
labels[k] = v
}
metadata["labels"] = labels
}
if len(ns.Annotations) > 0 {
annotations := map[string]interface{}{}
for k, v := range ns.Annotations {
annotations[k] = v
}
metadata["annotations"] = annotations
}
return map[string]interface{}{
"metadata": metadata,
}
}

// EvalExpression evaluates a single CEL expression against the given input.
// extraVars are merged into the activation for testing sub-expressions.
func (e *Evaluator) EvalExpression(expr string, input *AdmissionInput, extraVars map[string]interface{}) (interface{}, error) {
prog, err := e.compile(expr)
if err != nil {
return nil, err
}
act := buildActivation(input)
mapAct := map[string]interface{}{
"object":          act.Object,
"oldObject":       act.OldObject,
"params":          act.Params,
"request":         act.Request,
"namespaceObject": act.Namespace,
}
for k, v := range extraVars {
mapAct[k] = v
}
result, _, err := prog.Eval(mapAct)
if err != nil {
return nil, fmt.Errorf("CEL evaluation error: %w", err)
}
return result.Value(), nil
}

// CompileCheck validates that a CEL expression compiles without errors.
// Uses the version-gated environment (NewExpressionsEnv).
func (e *Evaluator) CompileCheck(expr string) error {
env := e.envSet.NewExpressionsEnv()
_, issues := env.Compile(expr)
if issues != nil && issues.Err() != nil {
return fmt.Errorf("CEL compilation error: %s", issues.Err())
}
return nil
}

// EvalAdmission evaluates a full VAP-style policy (preamble vars -> policy vars -> validations).
func (e *Evaluator) EvalAdmission(policy *VAPPolicy, input *AdmissionInput) (*AdmissionResult, error) {
act := buildActivation(input)
variablesMap := map[string]interface{}{}
var totalCost int64

// Phase 1: Preamble variables
for _, v := range e.preambleVars {
val, cost, err := e.evalInActivation(v.Expression, act, variablesMap)
totalCost += cost
if err != nil {
return nil, fmt.Errorf("preamble variable %q: %w", v.Name, err)
}
variablesMap[v.Name] = val
if v.Name == "params" {
act.Params = val
}
}

// Phase 2: Policy variables
for _, v := range policy.Variables {
val, cost, err := e.evalInActivation(v.Expression, act, variablesMap)
totalCost += cost
if err != nil {
return nil, fmt.Errorf("variable %q: %w", v.Name, err)
}
variablesMap[v.Name] = val
}

// Phase 3: Validations
result := &AdmissionResult{Allowed: true}
for _, val := range policy.Validations {
evalResult, cost, err := e.evalInActivation(val.Expression, act, variablesMap)
totalCost += cost
if err != nil {
result.Allowed = false
result.Violations = append(result.Violations, Violation{Expression: val.Expression, Error: err})
continue
}
allowed, ok := evalResult.(bool)
if !ok {
result.Allowed = false
result.Violations = append(result.Violations, Violation{
Expression: val.Expression,
Error:      fmt.Errorf("validation must return bool, got %T", evalResult),
})
continue
}
if !allowed {
result.Allowed = false
msg := val.Message
if val.MessageExpression != "" {
if msgVal, msgCost, err := e.evalInActivation(val.MessageExpression, act, variablesMap); err == nil {
totalCost += msgCost
if s, ok := msgVal.(string); ok {
msg = s
}
}
}
result.Violations = append(result.Violations, Violation{Expression: val.Expression, Message: msg})
}
}
result.Cost = totalCost
return result, nil
}

// EvalVariable evaluates a single named variable from a policy, given input.
// This evaluates preamble vars and all policy vars up to and including the target.
func (e *Evaluator) EvalVariable(policy *VAPPolicy, variableName string, input *AdmissionInput) (interface{}, error) {
act := buildActivation(input)
variablesMap := map[string]interface{}{}

for _, v := range e.preambleVars {
val, _, err := e.evalInActivation(v.Expression, act, variablesMap)
if err != nil {
return nil, fmt.Errorf("preamble variable %q: %w", v.Name, err)
}
variablesMap[v.Name] = val
if v.Name == "params" {
act.Params = val
}
}
for _, v := range policy.Variables {
val, _, err := e.evalInActivation(v.Expression, act, variablesMap)
if err != nil {
return nil, fmt.Errorf("variable %q: %w", v.Name, err)
}
variablesMap[v.Name] = val
if v.Name == variableName {
return val, nil
}
}
return nil, fmt.Errorf("variable %q not found in policy", variableName)
}

// evalInActivation compiles and evaluates an expression with the TestActivation
// and a variables map. Returns (value, costUnits, error).
func (e *Evaluator) evalInActivation(expr string, act *admissioncel.TestActivation, variablesMap map[string]interface{}) (interface{}, int64, error) {
prog, err := e.compileWithVariables(expr)
if err != nil {
return nil, 0, err
}
evalAct := &admissioncel.TestActivation{
Object:    act.Object,
OldObject: act.OldObject,
Params:    act.Params,
Request:   act.Request,
Namespace: act.Namespace,
Variables: variablesMap,
}
result, details, err := prog.Eval(evalAct)
if err != nil {
return nil, 0, fmt.Errorf("CEL evaluation error: %w", err)
}
var cost int64
if details != nil {
cost = int64(*details.ActualCost())
}
return result.Value(), cost, nil
}

// ParseVAPPolicy parses a VAP policy from YAML content.
func ParseVAPPolicy(yamlContent string) (*VAPPolicy, error) {
return parseVAPPolicy([]byte(yamlContent))
}

// ParseVAPPolicyFile reads and parses a VAP policy from a file.
func ParseVAPPolicyFile(path string) (*VAPPolicy, error) {
return parseVAPPolicyFile(path)
}

// FormatViolations returns a human-readable string of all violations.
func (r *AdmissionResult) FormatViolations() string {
if len(r.Violations) == 0 {
return ""
}
var msgs []string
for _, v := range r.Violations {
if v.Error != nil {
msgs = append(msgs, fmt.Sprintf("error evaluating %q: %v", v.Expression, v.Error))
} else {
msgs = append(msgs, v.Message)
}
}
return strings.Join(msgs, ", ")
}
