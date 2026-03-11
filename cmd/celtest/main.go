package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	admissionv1 "k8s.io/api/admission/v1"
	celtest "k8s.io/apiserver/pkg/cel/testing/celtest"
)

func main() {
rootCmd := &cobra.Command{
Use:   "celtest",
Short: "CEL test tooling for Kubernetes policy expressions",
}
rootCmd.AddCommand(runCmd())
rootCmd.AddCommand(compileCmd())
rootCmd.AddCommand(evalCmd())
if err := rootCmd.Execute(); err != nil {
os.Exit(1)
}
}

type celTestConfig struct {
Version  string `yaml:"version"`
Output   string `yaml:"output"`
Preamble struct {
Variables []celtest.Variable `yaml:"variables"`
} `yaml:"preamble"`
}

func loadConfig(configPath string) (*celTestConfig, error) {
if configPath == "" {
if _, err := os.Stat(".celtest.yaml"); err == nil {
configPath = ".celtest.yaml"
} else {
return nil, nil
}
}
data, err := os.ReadFile(configPath)
if err != nil {
return nil, fmt.Errorf("reading config %s: %w", configPath, err)
}
var cfg celTestConfig
if err := yaml.Unmarshal(data, &cfg); err != nil {
return nil, fmt.Errorf("parsing config %s: %w", configPath, err)
}
return &cfg, nil
}

func buildEvaluator(versionStr string, config *celTestConfig) (*celtest.Evaluator, error) {
var opts []celtest.Option
ver := versionStr
if ver == "" && config != nil && config.Version != "" {
ver = config.Version
}
if ver != "" {
var major, minor uint
if _, err := fmt.Sscanf(ver, "%d.%d", &major, &minor); err != nil {
return nil, fmt.Errorf("invalid version %q: %w", ver, err)
}
opts = append(opts, celtest.WithVersion(major, minor))
}
if config != nil && len(config.Preamble.Variables) > 0 {
opts = append(opts, celtest.WithPreambleVariables(config.Preamble.Variables...))
}
return celtest.NewEvaluator(opts...)
}

func discoverTestFiles(root string) ([]string, error) {
var files []string
err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
if err != nil {
return err
}
if !info.IsDir() && strings.HasSuffix(path, "_test.cel") {
files = append(files, path)
}
return nil
})
return files, err
}

type testFileData struct {
Mode   string     `yaml:"mode"`
Source string     `yaml:"source"`
Tests  []testCase `yaml:"tests"`
}

type testCase struct {
Name       string                 `yaml:"name"`
Variable   string                 `yaml:"variable"`
Expression string                 `yaml:"expression"`
Object     map[string]interface{} `yaml:"object"`
OldObject  map[string]interface{} `yaml:"oldObject"`
Params     map[string]interface{} `yaml:"params"`
Request    map[string]interface{} `yaml:"request"`
Expect     struct {
Value           interface{} `yaml:"value"`
Size            *int        `yaml:"size"`
Contains        *string     `yaml:"contains"`
Allowed         *bool       `yaml:"allowed"`
MessageContains *string     `yaml:"messageContains"`
Error           *bool       `yaml:"error"`
ErrorContains   *string     `yaml:"errorContains"`
} `yaml:"expect"`
}

func runTestFileCLI(eval *celtest.Evaluator, testFilePath string, verbose bool) (passed, failed int) {
data, err := os.ReadFile(testFilePath)
if err != nil {
fmt.Fprintf(os.Stderr, "ERROR reading %s: %v\n", testFilePath, err)
return 0, 1
}
var tf testFileData
if err := yaml.Unmarshal(data, &tf); err != nil {
fmt.Fprintf(os.Stderr, "ERROR parsing %s: %v\n", testFilePath, err)
return 0, 1
}

mode := tf.Mode
if mode == "" {
mode = "policy"
}

var policy *celtest.VAPPolicy
if mode == "policy" {
policyPath := companionPath(testFilePath)
if policyPath == "" {
fmt.Fprintf(os.Stderr, "ERROR: no companion policy for %s\n", testFilePath)
return 0, 1
}
policy, err = celtest.ParseVAPPolicyFile(policyPath)
if err != nil {
fmt.Fprintf(os.Stderr, "ERROR loading policy for %s: %v\n", testFilePath, err)
return 0, 1
}
}

for _, tc := range tf.Tests {
input := &celtest.AdmissionInput{
Object:    tc.Object,
OldObject: tc.OldObject,
Params:    tc.Params,
}
if tc.Request != nil {
input.Request = requestMapToTyped(tc.Request)
}

ok, errMsg := runOneTest(eval, policy, mode, &tc, input)
if ok {
passed++
if verbose {
fmt.Printf("--- PASS: %s\n", tc.Name)
}
} else {
failed++
fmt.Printf("--- FAIL: %s\n", tc.Name)
fmt.Printf("        %s\n", errMsg)
}
}
return
}

func runOneTest(eval *celtest.Evaluator, policy *celtest.VAPPolicy, mode string, tc *testCase, input *celtest.AdmissionInput) (bool, string) {
if tc.Variable != "" {
result, err := eval.EvalVariable(policy, tc.Variable, input)
if err != nil {
if tc.Expect.Error != nil && *tc.Expect.Error {
return true, ""
}
return false, fmt.Sprintf("evaluating variable %q: %v", tc.Variable, err)
}
return checkExpect(result, tc)
} else if tc.Expression != "" {
result, err := eval.EvalExpression(tc.Expression, input, nil)
if err != nil {
if tc.Expect.Error != nil && *tc.Expect.Error {
return true, ""
}
return false, fmt.Sprintf("evaluating expression: %v", err)
}
return checkExpect(result, tc)
} else {
result, err := eval.EvalAdmission(policy, input)
if err != nil {
return false, fmt.Sprintf("evaluating policy: %v", err)
}
if tc.Expect.Allowed != nil && result.Allowed != *tc.Expect.Allowed {
msgs := make([]string, len(result.Violations))
for i, v := range result.Violations {
msgs[i] = v.Message
}
return false, fmt.Sprintf("expected allowed=%v, got allowed=%v (violations: %s)", *tc.Expect.Allowed, result.Allowed, strings.Join(msgs, "; "))
}
if tc.Expect.MessageContains != nil {
msgs := make([]string, len(result.Violations))
for i, v := range result.Violations {
msgs[i] = v.Message
}
if !strings.Contains(strings.Join(msgs, "; "), *tc.Expect.MessageContains) {
return false, fmt.Sprintf("expected message containing %q", *tc.Expect.MessageContains)
}
}
return true, ""
}
}

func checkExpect(result interface{}, tc *testCase) (bool, string) {
if tc.Expect.Value != nil {
if fmt.Sprintf("%v", result) != fmt.Sprintf("%v", tc.Expect.Value) {
return false, fmt.Sprintf("expected value %v, got %v", tc.Expect.Value, result)
}
}
if tc.Expect.Size != nil {
s := valueSize(result)
if s != *tc.Expect.Size {
return false, fmt.Sprintf("expected size %d, got %d", *tc.Expect.Size, s)
}
}
if tc.Expect.Contains != nil {
if !valueContains(result, *tc.Expect.Contains) {
return false, fmt.Sprintf("expected contains %q", *tc.Expect.Contains)
}
}
return true, ""
}

func valueSize(v interface{}) int {
if v == nil {
return 0
}
switch val := v.(type) {
case []interface{}:
return len(val)
case string:
return len(val)
default:
// Reflection for CEL types
rv := reflect.ValueOf(v)
if rv.Kind() == reflect.Slice {
return rv.Len()
}
return 0
}
}

func valueContains(result interface{}, substr string) bool {
if s, ok := result.(string); ok {
return strings.Contains(s, substr)
}
// Check list elements
rv := reflect.ValueOf(result)
if rv.Kind() == reflect.Slice {
for i := 0; i < rv.Len(); i++ {
elem := rv.Index(i).Interface()
if fmt.Sprintf("%v", elem) == substr || strings.Contains(fmt.Sprintf("%v", elem), substr) {
return true
}
}
}
return false
}

func companionPath(testPath string) string {
dir := filepath.Dir(testPath)
base := filepath.Base(testPath)
name := strings.TrimSuffix(base, "_test.cel")
for _, ext := range []string{".cel", ".yaml", ".yml"} {
p := filepath.Join(dir, name+ext)
if _, err := os.Stat(p); err == nil {
return p
}
}
return ""
}

func requestMapToTyped(m map[string]interface{}) *admissionv1.AdmissionRequest {
// Minimal conversion for the fields tests typically set
req := &admissionv1.AdmissionRequest{}
if op, ok := m["operation"].(string); ok {
req.Operation = admissionv1.Operation(op)
}
return req
}

func runCmd() *cobra.Command {
var versionFlag, configFlag, outputFlag string
var verboseFlag, failFastFlag bool
cmd := &cobra.Command{
Use:   "run [paths...]",
Short: "Discover and run *_test.cel files",
Args:  cobra.MinimumNArgs(1),
RunE: func(cmd *cobra.Command, args []string) error {
config, err := loadConfig(configFlag)
if err != nil {
return err
}
eval, err := buildEvaluator(versionFlag, config)
if err != nil {
return err
}
var allFiles []string
for _, path := range args {
files, err := discoverTestFiles(path)
if err != nil {
return err
}
allFiles = append(allFiles, files...)
}
if len(allFiles) == 0 {
return fmt.Errorf("no *_test.cel files found")
}
totalPassed, totalFailed := 0, 0
for _, f := range allFiles {
fmt.Printf("=== RUN   %s\n", f)
p, fl := runTestFileCLI(eval, f, verboseFlag)
totalPassed += p
totalFailed += fl
s := "PASS"
if fl > 0 {
s = "FAIL"
}
fmt.Printf("=== %s: %s (%d passed, %d failed)\n\n", s, f, p, fl)
if failFastFlag && fl > 0 {
break
}
}
s := "ok"
if totalFailed > 0 {
s = "FAIL"
}
fmt.Printf("%s\t%d files, %d passed, %d failed\n", s, len(allFiles), totalPassed, totalFailed)
if totalFailed > 0 {
os.Exit(1)
}
return nil
},
}
cmd.Flags().StringVar(&versionFlag, "version", "", "K8s version")
cmd.Flags().StringVar(&configFlag, "config", "", "Config file")
cmd.Flags().StringVarP(&outputFlag, "output", "o", "text", "Output format")
cmd.Flags().BoolVarP(&verboseFlag, "verbose", "v", false, "Verbose")
cmd.Flags().BoolVar(&failFastFlag, "fail-fast", false, "Stop on first failure")
return cmd
}

func compileCmd() *cobra.Command {
var versionFlag string
cmd := &cobra.Command{
Use:   "compile [files...]",
Short: "Compile-check expressions",
Args:  cobra.MinimumNArgs(1),
RunE: func(cmd *cobra.Command, args []string) error {
eval, err := buildEvaluator(versionFlag, nil)
if err != nil {
return err
}
hasError := false
for _, path := range args {
policy, err := celtest.ParseVAPPolicyFile(path)
if err != nil {
fmt.Fprintf(os.Stderr, "ERROR: %s: %v\n", path, err)
hasError = true
continue
}
for _, v := range policy.Variables {
if err := eval.CompileCheck(v.Expression); err != nil {
fmt.Fprintf(os.Stderr, "ERROR: %s variable %q: %v\n", path, v.Name, err)
hasError = true
}
}
for _, v := range policy.Validations {
if err := eval.CompileCheck(v.Expression); err != nil {
fmt.Fprintf(os.Stderr, "ERROR: %s validation: %v\n", path, err)
hasError = true
}
}
if !hasError {
fmt.Printf("OK: %s\n", path)
}
}
if hasError {
os.Exit(3)
}
return nil
},
}
cmd.Flags().StringVar(&versionFlag, "version", "", "K8s version")
return cmd
}

func evalCmd() *cobra.Command {
var versionFlag, objectFlag, paramsFlag string
cmd := &cobra.Command{
Use:   "eval '<expression>'",
Short: "Evaluate a CEL expression",
Args:  cobra.ExactArgs(1),
RunE: func(cmd *cobra.Command, args []string) error {
eval, err := buildEvaluator(versionFlag, nil)
if err != nil {
return err
}
input := &celtest.AdmissionInput{}
if objectFlag != "" {
obj, err := loadYAMLInput(objectFlag)
if err != nil {
return err
}
input.Object = obj
}
if paramsFlag != "" {
params, err := loadYAMLInput(paramsFlag)
if err != nil {
return err
}
input.Params = params
}
result, err := eval.EvalExpression(args[0], input, nil)
if err != nil {
fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
os.Exit(3)
}
fmt.Printf("%v\n", result)
return nil
},
}
cmd.Flags().StringVar(&versionFlag, "version", "", "K8s version")
cmd.Flags().StringVar(&objectFlag, "object", "", "Object YAML/JSON")
cmd.Flags().StringVar(&paramsFlag, "params", "", "Params YAML/JSON")
return cmd
}

func loadYAMLInput(input string) (map[string]interface{}, error) {
var data []byte
if _, err := os.Stat(input); err == nil {
data, err = os.ReadFile(input)
if err != nil {
return nil, err
}
} else {
data = []byte(input)
}
var result map[string]interface{}
if err := yaml.Unmarshal(data, &result); err != nil {
return nil, err
}
return result, nil
}
