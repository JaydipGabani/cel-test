package celtest

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// policyFile is the on-disk YAML structure for *.cel policy files.
type policyFile struct {
	Variables   []variableEntry   `yaml:"variables"`
	Validations []validationEntry `yaml:"validations"`
}

type variableEntry struct {
	Name       string `yaml:"name"`
	Expression string `yaml:"expression"`
}

type validationEntry struct {
	Expression        string `yaml:"expression"`
	Message           string `yaml:"message"`
	MessageExpression string `yaml:"messageExpression"`
}

func parseVAPPolicy(data []byte) (*VAPPolicy, error) {
	var pf policyFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("parsing policy YAML: %w", err)
	}

	policy := &VAPPolicy{}
	for _, v := range pf.Variables {
		if v.Name == "" {
			return nil, fmt.Errorf("variable missing name")
		}
		if v.Expression == "" {
			return nil, fmt.Errorf("variable %q missing expression", v.Name)
		}
		policy.Variables = append(policy.Variables, Variable{
			Name:       v.Name,
			Expression: v.Expression,
		})
	}
	for _, val := range pf.Validations {
		if val.Expression == "" {
			return nil, fmt.Errorf("validation missing expression")
		}
		policy.Validations = append(policy.Validations, Validation{
			Expression:        val.Expression,
			Message:           val.Message,
			MessageExpression: val.MessageExpression,
		})
	}
	return policy, nil
}

func parseVAPPolicyFile(path string) (*VAPPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy file %s: %w", path, err)
	}
	return parseVAPPolicy(data)
}
