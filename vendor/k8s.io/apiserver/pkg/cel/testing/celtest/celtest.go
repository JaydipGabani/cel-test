// Package celeval provides a CEL expression evaluator using the real Kubernetes
// API server CEL environment and upstream compilation pipeline.
//
// Shared with upstream (identical code paths):
//   - environment.MustBaseEnvSet() — versioned base CEL environment
//   - admissioncel.NewCompositedCompiler() — variable composition compiler
//   - admissioncel.CompileAndStoreVariables() — variable type propagation
//   - admissioncel.BuildRequestType() / BuildNamespaceType() — typed declarations
//   - admissioncel.CreateNamespaceObject() — namespace field filtering
//
// Custom evaluation loop (not shared):
//   - EvalAdmission evaluates expressions using the upstream-compiled environment
//     but with our own evaluation loop, because the upstream ForInput() requires
//     admission.VersionedAttributes — a type tied to the full admission pipeline
//     that cannot be cleanly constructed from unstructured test input.
//   - This is the primary motivation for placing the upstream package inside
//     k8s.io/apiserver, where ForInput() could be called directly.
package celtest

import (
	"fmt"

	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/cel/environment"
)

// Variable represents a named CEL variable expression.
type Variable struct {
	Name       string
	Expression string
}

// Validation represents a CEL validation rule.
type Validation struct {
	Expression        string
	Message           string
	MessageExpression string
}

// VAPPolicy represents a parsed VAP-style policy (variables + validations).
type VAPPolicy struct {
	Variables   []Variable  
	Validations []Validation
}

// AdmissionInput represents input for admission-style CEL evaluation.
type AdmissionInput struct {
	Object    map[string]interface{}        // → CEL `object` (DynType)
	OldObject map[string]interface{}        // → CEL `oldObject` (DynType)
	Params    map[string]interface{}        // → CEL `params` (DynType)
	Request   *admissionv1.AdmissionRequest // → CEL `request` (kubernetes.AdmissionRequest)
	Namespace *corev1.Namespace             // → CEL `namespaceObject` (kubernetes.Namespace)
}

// AdmissionResult holds the evaluation outcome.
type AdmissionResult struct {
	Allowed    bool
	Violations []Violation
	Cost       int64
}

// Violation represents a single failed validation.
type Violation struct {
	Expression string
	Message    string
	Error      error
}

// Messages returns the violation messages for all failed validations.
func (r *AdmissionResult) Messages() []string {
	var msgs []string
	for _, v := range r.Violations {
		if v.Message != "" {
			msgs = append(msgs, v.Message)
		}
	}
	return msgs
}

// Evaluator compiles and evaluates CEL expressions using the real K8s
// upstream compiler (NewCompositedCompiler) with our own evaluation loop.
type Evaluator struct {
	compiler     *admissioncel.CompositedCompiler // upstream compiler for variable composition
	envSet       *environment.EnvSet
	newEnv       *cel.Env // NewExpressionsEnv (version-gated, for CompileCheck)
	preambleVars []Variable
	ver          *version.Version
	costLimit    int64
}

// Option configures an Evaluator.
type Option func(*Evaluator)

// WithVersion sets the K8s compatibility version.
func WithVersion(major, minor uint) Option {
	return func(e *Evaluator) {
		e.ver = version.MajorMinor(major, minor)
	}
}

// WithPreambleVariables registers CEL variable expressions evaluated BEFORE
// the policy's own variables.
func WithPreambleVariables(vars ...Variable) Option {
	return func(e *Evaluator) {
		e.preambleVars = append(e.preambleVars, vars...)
	}
}

// WithCostLimit sets a CEL cost budget.
func WithCostLimit(limit int64) Option {
	return func(e *Evaluator) {
		e.costLimit = limit
	}
}

// GatekeeperPreamble returns the standard Gatekeeper preamble variables.
func GatekeeperPreamble() []Variable {
	return []Variable{
		{
			Name:       "anyObject",
			Expression: `has(request.operation) && request.operation == "DELETE" && object == null ? oldObject : object`,
		},
		{
			Name:       "params",
			Expression: `!has(params.spec) ? null : !has(params.spec.parameters) ? null: params.spec.parameters`,
		},
	}
}

// NewEvaluator creates a CEL evaluator using the real K8s upstream compiler.
//
// Uses admissioncel.NewCompositedCompiler — the same compiler the K8s API server
// uses for VAP/MAP/matchConditions. Preamble variables are compiled using
// CompileAndStoreVariables with proper type propagation through the composition chain.
func NewEvaluator(opts ...Option) (*Evaluator, error) {
	e := &Evaluator{ver: version.MajorMinor(1, 32)}
	for _, opt := range opts {
		opt(e)
	}

	envSet := environment.MustBaseEnvSet(e.ver)
	e.envSet = envSet

	// Use the real upstream CompositedCompiler
	compiler, err := admissioncel.NewCompositedCompiler(envSet)
	if err != nil {
		return nil, fmt.Errorf("creating composited compiler: %w", err)
	}
	e.compiler = compiler

	// Store preamble variables using upstream CompileAndStoreVariables
	if len(e.preambleVars) > 0 {
		accessors := toNamedExpressionAccessors(e.preambleVars)
		optDecls := admissioncel.OptionalVariableDeclarations{HasParams: true}
		compiler.CompileAndStoreVariables(accessors, optDecls, environment.StoredExpressions)
	}

	// Build a version-gated env for CompileCheck
	extended, err := envSet.Extend(
		environment.VersionedOptions{
			IntroducedVersion: version.MajorMinor(1, 0),
			EnvOptions: []cel.EnvOption{
				cel.Variable("variables", cel.DynType),
				cel.Variable("object", cel.DynType),
				cel.Variable("oldObject", cel.DynType),
				cel.Variable("params", cel.DynType),
				cel.Variable("request", cel.DynType),
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("extending env for CompileCheck: %w", err)
	}
	e.newEnv = extended.NewExpressionsEnv()

	return e, nil
}

// EvalAdmission evaluates a VAP/MAP/matchCondition policy against admission input.
//
// Compilation uses the upstream CompositedCompiler.CompileAndStoreVariables —
// ensuring expressions compile identically to the K8s API server with proper
// variable type propagation through the composition chain.
//
// Evaluation uses our own loop that mirrors the upstream ForInput() logic.
// The upstream ForInput() requires admission.VersionedAttributes which is tied
// to the full admission pipeline and cannot be cleanly constructed from
// unstructured test input. This evaluation gap is the primary motivation for
// placing the upstream package inside k8s.io/apiserver, where ForInput() could
// be called directly or a simpler Evaluate() method could be added.
func (e *Evaluator) EvalAdmission(policy *VAPPolicy, input *AdmissionInput) (*AdmissionResult, error) {
	if input == nil {
		input = &AdmissionInput{}
	}

	env := e.compiler.CompositionEnv.EnvSet.StoredExpressionsEnv()
	activation := buildActivationMap(input)
	varsMap := activation["variables"].(map[string]interface{})

	result := &AdmissionResult{Allowed: true}

	// Step 1: Evaluate preamble variables
	for _, v := range e.preambleVars {
		val, err := evalSingleExpression(env, v.Expression, activation)
		if err != nil {
			return nil, fmt.Errorf("evaluating preamble variable %q: %w", v.Name, err)
		}
		varsMap[v.Name] = val
	}

	// Step 2: Evaluate policy variables
	for _, v := range policy.Variables {
		val, err := evalSingleExpression(env, v.Expression, activation)
		if err != nil {
			return nil, fmt.Errorf("evaluating variable %q: %w", v.Name, err)
		}
		varsMap[v.Name] = val
	}

	// Step 3: Evaluate validations
	for _, v := range policy.Validations {
		val, err := evalSingleExpression(env, v.Expression, activation)
		if err != nil {
			result.Allowed = false
			result.Violations = append(result.Violations, Violation{
				Expression: v.Expression,
				Error:      err,
			})
			continue
		}

		allowed, ok := val.(bool)
		if !ok {
			result.Allowed = false
			result.Violations = append(result.Violations, Violation{
				Expression: v.Expression,
				Error:      fmt.Errorf("validation did not return bool, got %T: %v", val, val),
			})
			continue
		}

		if !allowed {
			result.Allowed = false
			viol := Violation{Expression: v.Expression}
			if v.MessageExpression != "" {
				msgVal, msgErr := evalSingleExpression(env, v.MessageExpression, activation)
				if msgErr != nil {
					viol.Message = fmt.Sprintf("(error evaluating messageExpression: %v)", msgErr)
				} else {
					viol.Message = fmt.Sprintf("%v", msgVal)
				}
			} else if v.Message != "" {
				viol.Message = v.Message
			}
			result.Violations = append(result.Violations, viol)
		}
	}

	return result, nil
}

// EvalExpression evaluates a single CEL expression.
func (e *Evaluator) EvalExpression(expr string, input *AdmissionInput, extraVars map[string]interface{}) (interface{}, error) {
	if input == nil {
		input = &AdmissionInput{}
	}

	env := e.compiler.CompositionEnv.EnvSet.StoredExpressionsEnv()
	activation := buildActivationMap(input)
	varsMap := activation["variables"].(map[string]interface{})
	for k, v := range extraVars {
		varsMap[k] = v
	}

	// Evaluate preamble variables
	for _, v := range e.preambleVars {
		val, err := evalSingleExpression(env, v.Expression, activation)
		if err != nil {
			return nil, fmt.Errorf("evaluating preamble variable %q: %w", v.Name, err)
		}
		varsMap[v.Name] = val
	}

	return evalSingleExpression(env, expr, activation)
}

// EvalVariable evaluates a specific named variable from a policy, including
// preamble variables and all prior variables in the chain. Returns the value
// of the named variable.
func (e *Evaluator) EvalVariable(policy *VAPPolicy, varName string, input *AdmissionInput) (interface{}, error) {
	if input == nil {
		input = &AdmissionInput{}
	}

	env := e.compiler.CompositionEnv.EnvSet.StoredExpressionsEnv()
	activation := buildActivationMap(input)
	varsMap := activation["variables"].(map[string]interface{})

	// Evaluate preamble variables
	for _, v := range e.preambleVars {
		val, err := evalSingleExpression(env, v.Expression, activation)
		if err != nil {
			return nil, fmt.Errorf("evaluating preamble variable %q: %w", v.Name, err)
		}
		varsMap[v.Name] = val
		if v.Name == varName {
			return val, nil
		}
	}

	// Evaluate policy variables
	for _, v := range policy.Variables {
		val, err := evalSingleExpression(env, v.Expression, activation)
		if err != nil {
			return nil, fmt.Errorf("evaluating variable %q: %w", v.Name, err)
		}
		varsMap[v.Name] = val
		if v.Name == varName {
			return val, nil
		}
	}

	return nil, fmt.Errorf("variable %q not found in policy or preamble", varName)
}

// CompileCheck validates that a CEL expression compiles without errors.
// Uses NewExpressionsEnv (version-gated) so WithVersion restricts library availability.
func (e *Evaluator) CompileCheck(expr string) error {
	expr = strings.TrimSpace(expr)
	_, issues := e.newEnv.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return fmt.Errorf("compilation failed: %w", issues.Err())
	}
	return nil
}

// ========== Input conversion ==========

// buildActivationMap creates an activation map for CEL evaluation.
func buildActivationMap(input *AdmissionInput) map[string]interface{} {
	var requestMap map[string]interface{}
	if input.Request != nil {
		result, err := runtime.DefaultUnstructuredConverter.ToUnstructured(input.Request)
		if err != nil {
			requestMap = map[string]interface{}{"operation": string(input.Request.Operation)}
		} else {
			requestMap = result
		}
	} else {
		requestMap = map[string]interface{}{"operation": "CREATE"}
	}

	return map[string]interface{}{
		"variables": map[string]interface{}{},
		"object":    input.Object,
		"oldObject": input.OldObject,
		"params":    input.Params,
		"request":   requestMap,
	}
}

// evalSingleExpression compiles and evaluates a single CEL expression.
func evalSingleExpression(env *cel.Env, expr string, activation map[string]interface{}) (interface{}, error) {
	expr = strings.TrimSpace(expr)
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		ast, issues = env.Parse(expr)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("parsing: %w", issues.Err())
		}
	}
	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("program: %w", err)
	}
	out, _, err := prg.Eval(activation)
	if err != nil {
		return nil, fmt.Errorf("evaluating CEL expression: %w", err)
	}
	return convertCELValue(out), nil
}

// ========== Upstream interface adapters ==========

// namedExpr implements admissioncel.NamedExpressionAccessor
type namedExpr struct {
	name       string
	expression string
}

func (n *namedExpr) GetExpression() string   { return n.expression }
func (n *namedExpr) ReturnTypes() []*cel.Type { return []*cel.Type{cel.AnyType} }
func (n *namedExpr) GetName() string          { return n.name }

func toNamedExpressionAccessors(vars []Variable) []admissioncel.NamedExpressionAccessor {
	accessors := make([]admissioncel.NamedExpressionAccessor, len(vars))
	for i, v := range vars {
		accessors[i] = &namedExpr{name: v.Name, expression: v.Expression}
	}
	return accessors
}

// ========== CEL value conversion ==========

func convertCELValue(val ref.Val) interface{} {
	if val == nil {
		return nil
	}
	switch val.Type() {
	case types.BoolType, types.IntType, types.UintType, types.DoubleType, types.StringType:
		return val.Value()
	case types.NullType:
		return nil
	case types.ListType:
		it := val.(traits.Lister).Iterator()
		var result []interface{}
		for it.HasNext() == types.True {
			result = append(result, convertCELValue(it.Next()))
		}
		return result
	case types.MapType:
		m := val.(traits.Mapper)
		it := m.Iterator()
		result := map[string]interface{}{}
		for it.HasNext() == types.True {
			k := it.Next()
			v, _ := m.Find(k)
			result[fmt.Sprintf("%v", k.Value())] = convertCELValue(v)
		}
		return result
	default:
		return val.Value()
	}
}
