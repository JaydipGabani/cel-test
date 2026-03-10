package celtest

import (
"fmt"
"os"
"path/filepath"
"reflect"
"strings"
"testing"

"github.com/google/cel-go/common/types/ref"
admissionv1 "k8s.io/api/admission/v1"
	"gopkg.in/yaml.v3"
)

type testFile struct {
Mode  string     `yaml:"mode"`
Tests []testCase `yaml:"tests"`
}

type testCase struct {
Name       string                 `yaml:"name"`
Variable   string                 `yaml:"variable"`
Expression string                 `yaml:"expression"`
Object     map[string]interface{} `yaml:"object"`
OldObject  map[string]interface{} `yaml:"oldObject"`
Params     map[string]interface{} `yaml:"params"`
Request    map[string]interface{} `yaml:"request"`
Expect     expectation            `yaml:"expect"`
}

type expectation struct {
Value           interface{} `yaml:"value"`
Size            *int        `yaml:"size"`
Contains        string      `yaml:"contains"`
Allowed         *bool       `yaml:"allowed"`
MessageContains string      `yaml:"messageContains"`
Error           *bool       `yaml:"error"`
ErrorContains   string      `yaml:"errorContains"`
}

// DiscoverAndRunTestsWithEvaluator walks srcRoot for *_test.cel files and runs
// them using the provided evaluator with optional parameter wrapping.
func DiscoverAndRunTestsWithEvaluator(t *testing.T, eval *Evaluator, srcRoot string, wrapParams bool) {
discoverAndRun(t, eval, srcRoot, wrapParams)
}

// DiscoverAndRunTestsRaw walks srcRoot for *_test.cel files and runs them
// without preamble variables or parameter wrapping.
func DiscoverAndRunTestsRaw(t *testing.T, srcRoot string) {
eval, err := NewEvaluator()
if err != nil {
t.Fatalf("creating evaluator: %v", err)
}
discoverAndRun(t, eval, srcRoot, false)
}

func discoverAndRun(t *testing.T, eval *Evaluator, srcRoot string, wrapParams bool) {
t.Helper()
var testFiles []string
err := filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
if err != nil {
return err
}
if !info.IsDir() && strings.HasSuffix(info.Name(), "_test.cel") {
testFiles = append(testFiles, path)
}
return nil
})
if err != nil {
t.Fatalf("walking %s: %v", srcRoot, err)
}
if len(testFiles) == 0 {
t.Fatalf("no *_test.cel files found in %s", srcRoot)
}
for _, tf := range testFiles {
tf := tf
relPath, _ := filepath.Rel(srcRoot, tf)
t.Run(relPath, func(t *testing.T) {
runTestFile(t, eval, tf, wrapParams)
})
}
}

func runTestFile(t *testing.T, eval *Evaluator, testFilePath string, wrapParams bool) {
t.Helper()
data, err := os.ReadFile(testFilePath)
if err != nil {
t.Fatalf("reading test file: %v", err)
}
var tf testFile
if err := yaml.Unmarshal(data, &tf); err != nil {
t.Fatalf("parsing test file: %v", err)
}
if len(tf.Tests) == 0 {
t.Fatal("test file has no tests")
}
mode := tf.Mode
if mode == "" {
mode = "policy"
}
var policy *VAPPolicy
if mode == "policy" {
policyPath := companionPolicyPath(testFilePath)
policy, err = ParseVAPPolicyFile(policyPath)
if err != nil {
t.Fatalf("loading companion policy %s: %v", policyPath, err)
}
}
for _, tc := range tf.Tests {
tc := tc
t.Run(tc.Name, func(t *testing.T) {
runSingleTest(t, eval, policy, &tc, mode, wrapParams)
})
}
}

func runSingleTest(t *testing.T, eval *Evaluator, policy *VAPPolicy, tc *testCase, mode string, wrapParams bool) {
t.Helper()
if tc.Variable != "" && tc.Expression != "" {
t.Fatal("variable and expression are mutually exclusive")
}
if mode == "expression" && tc.Variable != "" {
t.Fatal("variable tests are not allowed in expression mode")
}
if mode == "expression" && tc.Expect.Allowed != nil {
t.Fatal("expect.allowed is not allowed in expression mode")
}
input := &AdmissionInput{
Object:    tc.Object,
OldObject: tc.OldObject,
Params:    tc.Params,
}
if tc.Request != nil {
input.Request = requestMapToTyped(tc.Request)
}
if wrapParams && input.Params != nil {
input.Params = map[string]interface{}{
"spec": map[string]interface{}{
"parameters": input.Params,
},
}
}
switch {
case tc.Variable != "":
runVariableTest(t, eval, policy, tc, input)
case tc.Expression != "":
runExpressionTest(t, eval, tc, input)
default:
if policy == nil {
t.Fatal("whole-policy test requires a companion .cel policy file")
}
runPolicyTest(t, eval, policy, tc, input)
}
}

// requestMapToTyped converts a YAML-parsed map to a minimal *admissionv1.AdmissionRequest.
func requestMapToTyped(m map[string]interface{}) *admissionv1.AdmissionRequest {
req := &admissionv1.AdmissionRequest{}
if op, ok := m["operation"].(string); ok {
req.Operation = admissionv1.Operation(op)
}
if name, ok := m["name"].(string); ok {
req.Name = name
}
if ns, ok := m["namespace"].(string); ok {
req.Namespace = ns
}
return req
}

func runVariableTest(t *testing.T, eval *Evaluator, policy *VAPPolicy, tc *testCase, input *AdmissionInput) {
t.Helper()
result, err := eval.EvalVariable(policy, tc.Variable, input)
if tc.Expect.Error != nil && *tc.Expect.Error {
if err == nil {
t.Error("expected error but got none")
} else if tc.Expect.ErrorContains != "" && !strings.Contains(err.Error(), tc.Expect.ErrorContains) {
t.Errorf("expected error containing %q, got: %v", tc.Expect.ErrorContains, err)
}
return
}
if err != nil {
t.Fatalf("unexpected error: %v", err)
}
checkValueExpectations(t, result, tc.Expect)
}

func runExpressionTest(t *testing.T, eval *Evaluator, tc *testCase, input *AdmissionInput) {
t.Helper()
result, err := eval.EvalExpression(tc.Expression, input, nil)
if tc.Expect.Error != nil && *tc.Expect.Error {
if err == nil {
t.Error("expected error but got none")
} else if tc.Expect.ErrorContains != "" && !strings.Contains(err.Error(), tc.Expect.ErrorContains) {
t.Errorf("expected error containing %q, got: %v", tc.Expect.ErrorContains, err)
}
return
}
if err != nil {
t.Fatalf("unexpected error: %v", err)
}
checkValueExpectations(t, result, tc.Expect)
}

func runPolicyTest(t *testing.T, eval *Evaluator, policy *VAPPolicy, tc *testCase, input *AdmissionInput) {
t.Helper()
result, err := eval.EvalAdmission(policy, input)
if tc.Expect.Error != nil && *tc.Expect.Error {
if err == nil {
t.Error("expected error but got none")
} else if tc.Expect.ErrorContains != "" && !strings.Contains(err.Error(), tc.Expect.ErrorContains) {
t.Errorf("expected error containing %q, got: %v", tc.Expect.ErrorContains, err)
}
return
}
if err != nil {
t.Fatalf("unexpected error: %v", err)
}
if tc.Expect.Allowed != nil {
if result.Allowed != *tc.Expect.Allowed {
t.Errorf("expected allowed=%v, got allowed=%v (violations: %s)",
*tc.Expect.Allowed, result.Allowed, result.FormatViolations())
}
}
if tc.Expect.MessageContains != "" {
msg := result.FormatViolations()
if !strings.Contains(msg, tc.Expect.MessageContains) {
t.Errorf("expected violations containing %q, got: %q", tc.Expect.MessageContains, msg)
}
}
}

func checkValueExpectations(t *testing.T, result interface{}, expect expectation) {
t.Helper()
if expect.Value != nil {
if !valueEqual(result, expect.Value) {
t.Errorf("expected value %v (%T), got %v (%T)", expect.Value, expect.Value, result, result)
}
}
if expect.Size != nil {
if size := valueSize(result); size != *expect.Size {
t.Errorf("expected size %d, got %d (value: %v)", *expect.Size, size, result)
}
}
if expect.Contains != "" {
if !valueContains(result, expect.Contains) {
t.Errorf("expected result to contain %q, got: %v", expect.Contains, result)
}
}
}

func valueEqual(got, want interface{}) bool {
gotN, gotIsNum := toFloat64(got)
wantN, wantIsNum := toFloat64(want)
if gotIsNum && wantIsNum {
return gotN == wantN
}
if want == nil && got == nil {
return true
}
wantSlice, wantIsSlice := toSlice(want)
gotSlice, gotIsSlice := toSlice(got)
if wantIsSlice && gotIsSlice {
if len(wantSlice) == 0 && len(gotSlice) == 0 {
return true
}
return reflect.DeepEqual(gotSlice, wantSlice)
}
if gotBool, ok := got.(bool); ok {
if wantBool, ok2 := want.(bool); ok2 {
return gotBool == wantBool
}
}
if gotStr, ok := got.(string); ok {
if wantStr, ok2 := want.(string); ok2 {
return gotStr == wantStr
}
}
return reflect.DeepEqual(got, want)
}

func toFloat64(v interface{}) (float64, bool) {
switch n := v.(type) {
case int:
return float64(n), true
case int32:
return float64(n), true
case int64:
return float64(n), true
case float32:
return float64(n), true
case float64:
return n, true
}
return 0, false
}

func toSlice(v interface{}) ([]interface{}, bool) {
if v == nil {
return nil, true
}
rv := reflect.ValueOf(v)
if rv.Kind() == reflect.Slice {
result := make([]interface{}, rv.Len())
for i := 0; i < rv.Len(); i++ {
elem := rv.Index(i).Interface()
if refVal, ok := elem.(ref.Val); ok {
elem = refVal.Value()
}
result[i] = elem
}
return result, true
}
return nil, false
}

func valueSize(v interface{}) int {
if v == nil {
return 0
}
rv := reflect.ValueOf(v)
switch rv.Kind() {
case reflect.Slice, reflect.Map, reflect.String:
return rv.Len()
}
return -1
}

func valueContains(result interface{}, substr string) bool {
if s, ok := result.(string); ok {
return strings.Contains(s, substr)
}
rv := reflect.ValueOf(result)
if rv.Kind() == reflect.Slice {
for i := 0; i < rv.Len(); i++ {
elem := rv.Index(i).Interface()
if s, ok := elem.(string); ok && strings.Contains(s, substr) {
return true
}
if fmt.Sprint(elem) == substr || strings.Contains(fmt.Sprint(elem), substr) {
return true
}
}
}
return strings.Contains(fmt.Sprint(result), substr)
}

func companionPolicyPath(testPath string) string {
base := strings.TrimSuffix(testPath, "_test.cel")
return base + ".cel"
}

// RunTestFileWithEvaluator runs a single test file with a custom evaluator.
func RunTestFileWithEvaluator(t *testing.T, eval *Evaluator, testFilePath string, wrapParams bool) {
runTestFile(t, eval, testFilePath, wrapParams)
}
