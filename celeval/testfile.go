package celeval

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	admissionv1 "k8s.io/api/admission/v1"
)

// TestCase represents a single test case in a *_test.cel file.
type TestCase struct {
	Name string `yaml:"name"`

	// What to test — pick one:
	// Variable: test a specific named variable's output
	// Expression: test an arbitrary CEL expression
	// (neither): test the whole policy (allowed/denied)
	Variable   string `yaml:"variable,omitempty"`
	Expression string `yaml:"expression,omitempty"`

	// Input
	Object    map[string]interface{} `yaml:"object,omitempty"`
	OldObject map[string]interface{} `yaml:"oldObject,omitempty"`
	Params    map[string]interface{} `yaml:"params,omitempty"`
	Request   map[string]interface{} `yaml:"request,omitempty"`

	// Assertions
	Expect Expect `yaml:"expect"`
}

// Expect holds assertions for a test case.
type Expect struct {
	// For whole-policy tests
	Allowed         *bool  `yaml:"allowed,omitempty"`
	MessageContains string `yaml:"messageContains,omitempty"`

	// For variable/expression tests
	Value    interface{} `yaml:"value,omitempty"`
	Size     *int        `yaml:"size,omitempty"`
	Contains string      `yaml:"contains,omitempty"`

	// For error testing
	Error         *bool  `yaml:"error,omitempty"`
	ErrorContains string `yaml:"errorContains,omitempty"`
}

// TestFile represents a parsed *_test.cel file.
type TestFile struct {
	Mode  string     `yaml:"mode,omitempty"` // "policy" (default) or "expression"
	Tests []TestCase `yaml:"tests"`
}

// IsExpressionMode returns true if the test file uses expression mode.
func (tf *TestFile) IsExpressionMode() bool {
	return tf.Mode == "expression"
}

// Validate checks that the test file is internally consistent.
func (tf *TestFile) Validate() error {
	if tf.Mode != "" && tf.Mode != "policy" && tf.Mode != "expression" {
		return fmt.Errorf("invalid mode %q: must be \"policy\" or \"expression\"", tf.Mode)
	}
	for _, tc := range tf.Tests {
		if tc.Variable != "" && tc.Expression != "" {
			return fmt.Errorf("test %q: variable and expression are mutually exclusive", tc.Name)
		}
		if tf.IsExpressionMode() {
			if tc.Variable != "" {
				return fmt.Errorf("test %q: variable tests are not allowed in mode: expression", tc.Name)
			}
			if tc.Expect.Allowed != nil {
				return fmt.Errorf("test %q: expect.allowed is not allowed in mode: expression", tc.Name)
			}
			if tc.Expression == "" {
				return fmt.Errorf("test %q: expression field is required in mode: expression", tc.Name)
			}
		}
	}
	return nil
}

// ParseTestFile reads and parses a *_test.cel YAML file.
func ParseTestFile(path string) (*TestFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading test file: %w", err)
	}
	var tf TestFile
	if err := yaml.Unmarshal(data, &tf); err != nil {
		return nil, fmt.Errorf("parsing test file YAML: %w", err)
	}
	if err := tf.Validate(); err != nil {
		return nil, fmt.Errorf("validating test file: %w", err)
	}
	return &tf, nil
}

// wrapParams wraps raw params in Gatekeeper's constraint CRD structure.
func wrapParams(params map[string]interface{}) map[string]interface{} {
	if len(params) == 0 {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"spec": map[string]interface{}{
			"parameters": params,
		},
	}
}

// buildAdmissionInputFromTestCase constructs an AdmissionInput from YAML test case data.
func buildAdmissionInputFromTestCase(tc TestCase, wrapGatekeeperParams bool) *AdmissionInput {
	params := tc.Params
	if wrapGatekeeperParams {
		params = wrapParams(tc.Params)
	}

	input := &AdmissionInput{
		Object:    tc.Object,
		OldObject: tc.OldObject,
		Params:    params,
	}

	// Convert request map to typed *admissionv1.AdmissionRequest
	if tc.Request != nil {
		req := &admissionv1.AdmissionRequest{}
		if op, ok := tc.Request["operation"].(string); ok {
			req.Operation = admissionv1.Operation(op)
		}
		if name, ok := tc.Request["name"].(string); ok {
			req.Name = name
		}
		if ns, ok := tc.Request["namespace"].(string); ok {
			req.Namespace = ns
		}
		input.Request = req
	}

	return input
}

// RunTestFile loads a *_test.cel file alongside its policy file,
// runs all test cases using a Gatekeeper evaluator.
func RunTestFile(t *testing.T, policyPath, testPath string) {
	t.Helper()
	eval, err := NewEvaluator(WithPreambleVariables(GatekeeperPreamble()...))
	if err != nil {
		t.Fatalf("creating evaluator: %v", err)
	}
	RunTestFileWithEvaluator(t, eval, policyPath, testPath, true)
}

// RunTestFileRaw loads a *_test.cel file alongside its policy file,
// runs all test cases using a plain evaluator (no preamble, no param wrapping).
func RunTestFileRaw(t *testing.T, policyPath, testPath string) {
	t.Helper()
	eval, err := NewEvaluator()
	if err != nil {
		t.Fatalf("creating evaluator: %v", err)
	}
	RunTestFileWithEvaluator(t, eval, policyPath, testPath, false)
}

// RunTestFileWithEvaluator runs test cases with a given evaluator.
func RunTestFileWithEvaluator(t *testing.T, eval *Evaluator, policyPath, testPath string, wrapGatekeeperParams bool) {
	t.Helper()

	tf, err := ParseTestFile(testPath)
	if err != nil {
		t.Fatalf("parsing test file %s: %v", testPath, err)
	}

	// In expression mode, no policy file is needed.
	var policy *VAPPolicy
	if !tf.IsExpressionMode() {
		policy, err = ParseVAPPolicyFile(policyPath)
		if err != nil {
			t.Fatalf("parsing policy %s: %v", policyPath, err)
		}
	}

	for _, tc := range tf.Tests {
		t.Run(tc.Name, func(t *testing.T) {
			input := buildAdmissionInputFromTestCase(tc, wrapGatekeeperParams)

			switch {
			case tc.Variable != "":
				runVariableTest(t, eval, policy, tc, input)
			case tc.Expression != "":
				runExpressionTest(t, eval, tc, input)
			default:
				runPolicyTest(t, eval, policy, tc, input)
			}
		})
	}
}

// runVariableTest evaluates a specific named variable and checks assertions.
func runVariableTest(t *testing.T, eval *Evaluator, policy *VAPPolicy, tc TestCase, input *AdmissionInput) {
	t.Helper()

	val, err := eval.EvalVariable(policy, tc.Variable, input)

	if tc.Expect.Error != nil && *tc.Expect.Error {
		if err == nil {
			t.Error("expected error but got none")
		} else if tc.Expect.ErrorContains != "" && !strings.Contains(err.Error(), tc.Expect.ErrorContains) {
			t.Errorf("error %q does not contain %q", err.Error(), tc.Expect.ErrorContains)
		}
		return
	}
	if err != nil {
		t.Fatalf("evaluating variable %q: %v", tc.Variable, err)
	}

	checkAssertions(t, tc.Expect, val)
}

// runExpressionTest evaluates a single CEL expression.
func runExpressionTest(t *testing.T, eval *Evaluator, tc TestCase, input *AdmissionInput) {
	t.Helper()

	val, err := eval.EvalExpression(tc.Expression, input, nil)

	if tc.Expect.Error != nil && *tc.Expect.Error {
		if err == nil {
			t.Error("expected error but got none")
		} else if tc.Expect.ErrorContains != "" && !strings.Contains(err.Error(), tc.Expect.ErrorContains) {
			t.Errorf("error %q does not contain %q", err.Error(), tc.Expect.ErrorContains)
		}
		return
	}
	if err != nil {
		t.Fatalf("evaluating expression: %v", err)
	}

	checkAssertions(t, tc.Expect, val)
}

// runPolicyTest evaluates the whole policy.
func runPolicyTest(t *testing.T, eval *Evaluator, policy *VAPPolicy, tc TestCase, input *AdmissionInput) {
	t.Helper()

	result, err := eval.EvalAdmission(policy, input)

	if tc.Expect.Error != nil && *tc.Expect.Error {
		if err == nil {
			t.Error("expected error but got none")
		} else if tc.Expect.ErrorContains != "" && !strings.Contains(err.Error(), tc.Expect.ErrorContains) {
			t.Errorf("error %q does not contain %q", err.Error(), tc.Expect.ErrorContains)
		}
		return
	}
	if err != nil {
		t.Fatalf("evaluating policy: %v", err)
	}

	if tc.Expect.Allowed != nil {
		if result.Allowed != *tc.Expect.Allowed {
			t.Errorf("allowed: got %v, want %v; messages: %v", result.Allowed, *tc.Expect.Allowed, result.Messages())
		}
	}

	if tc.Expect.MessageContains != "" {
		msgs := strings.Join(result.Messages(), "; ")
		if !strings.Contains(msgs, tc.Expect.MessageContains) {
			t.Errorf("messages %q do not contain %q", msgs, tc.Expect.MessageContains)
		}
	}
}

// checkAssertions verifies value/size/contains assertions.
func checkAssertions(t *testing.T, expect Expect, val interface{}) {
	t.Helper()

	if expect.Value != nil {
		if !deepEqual(val, expect.Value) {
			t.Errorf("value: got %v (%T), want %v (%T)", val, val, expect.Value, expect.Value)
		}
	}

	if expect.Size != nil {
		size := valueSize(val)
		if size != *expect.Size {
			t.Errorf("size: got %d, want %d (value: %v)", size, *expect.Size, val)
		}
	}

	if expect.Contains != "" {
		s := fmt.Sprintf("%v", val)
		if !strings.Contains(s, expect.Contains) {
			t.Errorf("value %v does not contain %q", val, expect.Contains)
		}
	}
}

func valueSize(val interface{}) int {
	rv := reflect.ValueOf(val)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.String:
		return rv.Len()
	default:
		return -1
	}
}

func deepEqual(got, want interface{}) bool {
	gotN, gotIsNum := toFloat64(got)
	wantN, wantIsNum := toFloat64(want)
	if gotIsNum && wantIsNum {
		return gotN == wantN
	}
	gotV := reflect.ValueOf(got)
	wantV := reflect.ValueOf(want)
	if gotV.Kind() == reflect.Slice && wantV.Kind() == reflect.Slice {
		if gotV.Len() == 0 && wantV.Len() == 0 {
			return true
		}
	}
	return reflect.DeepEqual(got, want)
}

func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// DiscoverAndRunTests finds all *_test.cel files under a root directory
// and runs them with Gatekeeper preamble variables.
func DiscoverAndRunTests(t *testing.T, srcRoot string) {
	t.Helper()
	discoverAndRun(t, srcRoot, RunTestFile)
}

// DiscoverAndRunTestsRaw finds all *_test.cel files under a root directory
// and runs them without preamble variables (vanilla VAP / Kyverno style).
func DiscoverAndRunTestsRaw(t *testing.T, srcRoot string) {
	t.Helper()
	discoverAndRun(t, srcRoot, RunTestFileRaw)
}

func discoverAndRun(t *testing.T, srcRoot string, runFn func(*testing.T, string, string)) {
	t.Helper()

	// Discover *_test.cel files at two directory depths.
	matches, err := filepath.Glob(filepath.Join(srcRoot, "*", "*", "*_test.cel"))
	if err != nil {
		t.Fatalf("globbing for test files: %v", err)
	}
	matches2, _ := filepath.Glob(filepath.Join(srcRoot, "*", "*_test.cel"))
	matches = append(matches, matches2...)

	if len(matches) == 0 {
		t.Log("no *_test.cel files found")
		return
	}

	for _, testPath := range matches {
		dir := filepath.Dir(testPath)
		base := filepath.Base(testPath)

		// Derive policy path: foo_test.cel → foo.cel
		policyBase := strings.TrimSuffix(base, "_test.cel") + ".cel"
		policyPath := filepath.Join(dir, policyBase)

		// Check if expression-mode (no policy file needed)
		if _, err := os.Stat(policyPath); os.IsNotExist(err) {
			tf, parseErr := ParseTestFile(testPath)
			if parseErr != nil {
				t.Errorf("%s found but failed to parse: %v", testPath, parseErr)
				continue
			}
			if !tf.IsExpressionMode() {
				t.Errorf("%s found but no %s exists (add mode: expression for standalone tests)", testPath, policyBase)
				continue
			}
			policyPath = ""
		}

		rel, _ := filepath.Rel(srcRoot, dir)
		testName := rel
		if policyBase != "src.cel" {
			testName = filepath.Join(rel, strings.TrimSuffix(base, "_test.cel"))
		}
		t.Run(testName, func(t *testing.T) {
			runFn(t, policyPath, testPath)
		})
	}
}
