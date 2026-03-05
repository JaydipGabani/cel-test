package celtest_test

import (
	"testing"

	"github.com/JaydipGabani/cel-test/pkg/celtest"
)

func TestEvalExpression_Basic(t *testing.T) {
	eval, err := celtest.NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}

	tests := []struct {
		name   string
		expr   string
		object map[string]interface{}
		want   interface{}
	}{
		{
			name:   "has labels",
			expr:   `has(object.metadata.labels)`,
			object: map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "web"}}},
			want:   true,
		},
		{
			name:   "missing labels",
			expr:   `has(object.metadata.labels)`,
			object: map[string]interface{}{"metadata": map[string]interface{}{}},
			want:   false,
		},
		{
			name: "list size",
			expr: `size(object.spec.containers)`,
			object: map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "nginx"},
						map[string]interface{}{"name": "sidecar"},
					},
				},
			},
			want: int64(2),
		},
		{
			name:   "arithmetic",
			expr:   `object.spec.replicas > 3`,
			object: map[string]interface{}{"spec": map[string]interface{}{"replicas": int64(5)}},
			want:   true,
		},
		{
			name:   "string matches",
			expr:   `object.metadata.name.matches("^[a-z][a-z0-9-]*$")`,
			object: map[string]interface{}{"metadata": map[string]interface{}{"name": "my-app"}},
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvalExpression(tt.expr, &celtest.AdmissionInput{Object: tt.object}, nil)
			if err != nil {
				t.Fatalf("EvalExpression error: %v", err)
			}
			if result != tt.want {
				t.Errorf("got %v (%T), want %v (%T)", result, result, tt.want, tt.want)
			}
		})
	}
}

func TestEvalExpression_Error(t *testing.T) {
	eval, err := celtest.NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	_, err = eval.EvalExpression(
		`object.nonexistent.field`,
		&celtest.AdmissionInput{Object: map[string]interface{}{}},
		nil,
	)
	if err == nil {
		t.Error("expected error for accessing nonexistent field, got nil")
	}
}

func TestCompileCheck(t *testing.T) {
	eval, err := celtest.NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	if err := eval.CompileCheck(`object.metadata.name == "test"`); err != nil {
		t.Errorf("expected valid expression to compile, got: %v", err)
	}
	if err := eval.CompileCheck(`object.metadata.name ==`); err == nil {
		t.Error("expected compilation error for invalid syntax")
	}
}

func TestCompileCheck_VersionGating(t *testing.T) {
	eval128, err := celtest.NewEvaluator(celtest.WithVersion(1, 28))
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	if err := eval128.CompileCheck(`ip("192.168.0.1")`); err == nil {
		t.Error("expected ip() to fail compilation with version 1.28")
	}

	eval131, err := celtest.NewEvaluator(celtest.WithVersion(1, 31))
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	if err := eval131.CompileCheck(`ip("192.168.0.1")`); err != nil {
		t.Errorf("expected ip() to compile with version 1.31, got: %v", err)
	}
}

func TestEvalAdmission_SimplePolicy(t *testing.T) {
	eval, err := celtest.NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}

	policy := &celtest.VAPPolicy{
		Variables: []celtest.Variable{
			{Name: "replicas", Expression: `has(object.spec.replicas) ? object.spec.replicas : 1`},
			{Name: "maxReplicas", Expression: `params.maxReplicas`},
		},
		Validations: []celtest.Validation{
			{
				Expression:        `variables.replicas <= variables.maxReplicas`,
				MessageExpression: `"has " + string(variables.replicas) + " replicas, max is " + string(variables.maxReplicas)`,
			},
		},
	}

	result, err := eval.EvalAdmission(policy, &celtest.AdmissionInput{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{"replicas": int64(3)},
		},
		Params: map[string]interface{}{"maxReplicas": int64(5)},
	})
	if err != nil {
		t.Fatalf("EvalAdmission error: %v", err)
	}
	if !result.Allowed {
		t.Errorf("expected allowed, got denied: %s", result.FormatViolations())
	}

	result, err = eval.EvalAdmission(policy, &celtest.AdmissionInput{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{"replicas": int64(10)},
		},
		Params: map[string]interface{}{"maxReplicas": int64(5)},
	})
	if err != nil {
		t.Fatalf("EvalAdmission error: %v", err)
	}
	if result.Allowed {
		t.Error("expected denial, got allowed")
	}
	if len(result.Violations) == 0 {
		t.Error("expected violations")
	}
}

func TestEvalVariable(t *testing.T) {
	eval, err := celtest.NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}

	policy := &celtest.VAPPolicy{
		Variables: []celtest.Variable{
			{Name: "containers", Expression: `has(object.spec.containers) ? object.spec.containers : []`},
			{Name: "containerCount", Expression: `size(variables.containers)`},
		},
	}

	result, err := eval.EvalVariable(policy, "containerCount", &celtest.AdmissionInput{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{"name": "nginx"},
					map[string]interface{}{"name": "sidecar"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("EvalVariable error: %v", err)
	}
	if result != int64(2) {
		t.Errorf("got %v, want 2", result)
	}
}

func TestPreambleVariables_Gatekeeper(t *testing.T) {
	eval, err := celtest.NewEvaluator(celtest.WithPreambleVariables(celtest.GatekeeperPreamble()...))
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}

	policy := &celtest.VAPPolicy{
		Variables: []celtest.Variable{
			{Name: "containers", Expression: `has(variables.anyObject.spec.containers) ? variables.anyObject.spec.containers : []`},
			{Name: "badContainers", Expression: "variables.containers.filter(c, has(c.securityContext) && has(c.securityContext.privileged) && c.securityContext.privileged).map(c, \"Privileged: \" + c.name)"},
		},
		Validations: []celtest.Validation{
			{
				Expression:        `size(variables.badContainers) == 0`,
				MessageExpression: `variables.badContainers.join(", ")`,
			},
		},
	}

	result, err := eval.EvalAdmission(policy, &celtest.AdmissionInput{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":            "nginx",
						"image":           "nginx",
						"securityContext": map[string]interface{}{"privileged": true},
					},
				},
			},
		},
		Params: map[string]interface{}{
			"spec": map[string]interface{}{
				"parameters": map[string]interface{}{},
			},
		},
	})
	if err != nil {
		t.Fatalf("EvalAdmission error: %v", err)
	}
	if result.Allowed {
		t.Error("expected denial for privileged container")
	}
	if len(result.Violations) == 0 || result.Violations[0].Message == "" {
		t.Error("expected violation with message")
	}
}

func TestParseVAPPolicy(t *testing.T) {
	yaml := "variables:\n- name: replicas\n  expression: 'object.spec.replicas'\n- name: maxReplicas\n  expression: 'params.maxReplicas'\nvalidations:\n- expression: 'variables.replicas <= variables.maxReplicas'\n  messageExpression: '\"too many replicas\"'\n"
	policy, err := celtest.ParseVAPPolicy(yaml)
	if err != nil {
		t.Fatalf("ParseVAPPolicy error: %v", err)
	}
	if len(policy.Variables) != 2 {
		t.Errorf("expected 2 variables, got %d", len(policy.Variables))
	}
	if len(policy.Validations) != 1 {
		t.Errorf("expected 1 validation, got %d", len(policy.Validations))
	}
}

func TestDiscoverAndRunTestsRaw_VanillaVAP(t *testing.T) {
	celtest.DiscoverAndRunTestsRaw(t, "../../examples/vanilla-vap-replica-limit")
}

func TestDiscoverAndRunTestsRaw_Kyverno(t *testing.T) {
	celtest.DiscoverAndRunTestsRaw(t, "../../examples/kyverno-disallow-latest")
}

func TestDiscoverAndRunTestsRaw_DRA(t *testing.T) {
	celtest.DiscoverAndRunTestsRaw(t, "../../examples/dra-gpu-selector")
}

func TestDiscoverAndRunTests_Gatekeeper(t *testing.T) {
	celtest.DiscoverAndRunTests(t, "../../testdata/gatekeeper/pod-security-policy/privileged-containers")
}
