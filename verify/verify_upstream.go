package verify

import (
	_ "k8s.io/apiserver/pkg/cel/environment"
	_ "k8s.io/apiserver/pkg/admission/plugin/cel"
	_ "k8s.io/apiserver/pkg/apis/cel"
	_ "k8s.io/apiserver/pkg/authentication/cel"
	_ "k8s.io/apiserver/pkg/authorization/cel"
	_ "k8s.io/apiserver/pkg/cel/library"
)
