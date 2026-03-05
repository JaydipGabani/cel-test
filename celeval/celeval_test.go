package celeval_test

import (
	"testing"

	admissionv1 "k8s.io/api/admission/v1"

	"github.com/JaydipGabani/cel-test/celeval"
)

// ============================================================================
// Go API tests — demonstrates EvalAdmission, EvalExpression, CompileCheck,
// WithVersion, and WithPreambleVariables using the design doc's API.
// ============================================================================

func TestEvalExpression_Basic(t *testing.T) {
	eval, err := celeval.NewEvaluator()
	if err != nil {
		t.Fatal(err)
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
			name: "filter privileged containers",
			expr: `object.spec.containers.filter(c, has(c.securityContext) && c.securityContext.privileged == true).size()`,
			object: map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{"name": "safe", "securityContext": map[string]interface{}{"privileged": false}},
						map[string]interface{}{"name": "bad", "securityContext": map[string]interface{}{"privileged": true}},
					},
				},
			},
			want: int64(1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvalExpression(tt.expr, &celeval.AdmissionInput{Object: tt.object}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result != tt.want {
				t.Errorf("got %v (%T), want %v (%T)", result, result, tt.want, tt.want)
			}
		})
	}
}

func TestCompileCheck(t *testing.T) {
	eval, err := celeval.NewEvaluator()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("valid expression", func(t *testing.T) {
		if err := eval.CompileCheck(`object.metadata.name == "test"`); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("invalid expression", func(t *testing.T) {
		if err := eval.CompileCheck(`object.metadata.name ==`); err == nil {
			t.Error("expected error for invalid expression")
		}
	})
}

func TestWithVersion_GatesLibraries(t *testing.T) {
	// ip() was introduced in K8s 1.31
	t.Run("v1.32 has ip()", func(t *testing.T) {
		eval, _ := celeval.NewEvaluator(celeval.WithVersion(1, 32))
		if err := eval.CompileCheck(`ip("192.168.0.1").family() == 4`); err != nil {
			t.Errorf("expected ip() to be available at v1.32, got: %v", err)
		}
	})

	t.Run("v1.28 lacks ip()", func(t *testing.T) {
		eval, _ := celeval.NewEvaluator(celeval.WithVersion(1, 28))
		if err := eval.CompileCheck(`ip("192.168.0.1").family() == 4`); err == nil {
			t.Error("expected ip() to be unavailable at v1.28")
		}
	})
}

func TestEvalAdmission_PrivilegedContainers(t *testing.T) {
	eval, err := celeval.NewEvaluator(celeval.WithPreambleVariables(celeval.GatekeeperPreamble()...))
	if err != nil {
		t.Fatal(err)
	}

	policy, err := celeval.ParseVAPPolicyFile("../testdata/gatekeeper/pod-security-policy/privileged-containers/src.cel")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("denies privileged", func(t *testing.T) {
		result, err := eval.EvalAdmission(policy, &celeval.AdmissionInput{
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
			Params: map[string]interface{}{
				"spec": map[string]interface{}{"parameters": map[string]interface{}{}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Allowed {
			t.Error("expected denial for privileged container")
		}
	})

	t.Run("allows non-privileged", func(t *testing.T) {
		result, err := eval.EvalAdmission(policy, &celeval.AdmissionInput{
			Object: map[string]interface{}{
				"metadata": map[string]interface{}{"name": "test-pod"},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": "nginx", "image": "nginx",
							"securityContext": map[string]interface{}{"privileged": false},
						},
					},
				},
			},
			Params: map[string]interface{}{
				"spec": map[string]interface{}{"parameters": map[string]interface{}{}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Allowed {
			t.Error("expected allow for non-privileged container")
		}
	})

	t.Run("allows on UPDATE", func(t *testing.T) {
		result, err := eval.EvalAdmission(policy, &celeval.AdmissionInput{
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
			Request: &admissionv1.AdmissionRequest{Operation: admissionv1.Update},
			Params: map[string]interface{}{
				"spec": map[string]interface{}{"parameters": map[string]interface{}{}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Allowed {
			t.Error("expected allow on UPDATE")
		}
	})
}

func TestVanillaVAP_NoFramework(t *testing.T) {
	// Vanilla VAP expression — no preamble, no framework
	eval, err := celeval.NewEvaluator()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		expr    string
		object  map[string]interface{}
		allowed bool
	}{
		{
			name:    "name ends with k8s",
			expr:    `object.metadata.name.endsWith('k8s')`,
			object:  map[string]interface{}{"metadata": map[string]interface{}{"name": "my-namespace-k8s"}},
			allowed: true,
		},
		{
			name:    "name does not end with k8s",
			expr:    `object.metadata.name.endsWith('k8s')`,
			object:  map[string]interface{}{"metadata": map[string]interface{}{"name": "test-foobar"}},
			allowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := eval.EvalExpression(tt.expr, &celeval.AdmissionInput{Object: tt.object}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := result.(bool)
			if !ok {
				t.Fatalf("expected bool, got %T: %v", result, result)
			}
			if got != tt.allowed {
				t.Errorf("got %v, want %v", got, tt.allowed)
			}
		})
	}
}

func TestAdmissionRequest_TypedInput(t *testing.T) {
	// Verify that typed *admissionv1.AdmissionRequest is properly converted
	eval, err := celeval.NewEvaluator()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("operation from typed request", func(t *testing.T) {
		result, err := eval.EvalExpression(`request.operation`, &celeval.AdmissionInput{
			Request: &admissionv1.AdmissionRequest{Operation: admissionv1.Delete},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result != "DELETE" {
			t.Errorf("got %v, want DELETE", result)
		}
	})

	t.Run("default request has CREATE", func(t *testing.T) {
		result, err := eval.EvalExpression(`request.operation`, &celeval.AdmissionInput{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result != "CREATE" {
			t.Errorf("got %v, want CREATE", result)
		}
	})
}
