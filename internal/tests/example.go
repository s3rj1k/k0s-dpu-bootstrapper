// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"code.local/k0s-dpu-bootstrapper/internal/joinscript"
)

// ExampleFile is the join script template shipped with the project.
const ExampleFile = "../../examples/join-script.yaml"

// ExampleSteps are the step keys of the example, in the order the skeleton runs them.
var ExampleSteps = []string{
	"stepNeutraliseKubelet",
	"stepKubeletConfig",
	"stepInstallK0s",
	"preJoin",
	"stepWriteToken",
	"stepWorkerService",
	"stepJoin",
}

// LoadExample parses the shipped example and renders it the way the reconciler would.
func LoadExample(t *testing.T) (*corev1.ConfigMap, string) {
	t.Helper()

	raw, err := os.ReadFile(ExampleFile)
	if err != nil {
		t.Fatalf("reading %s: %v", ExampleFile, err)
	}

	cm := &corev1.ConfigMap{}
	if err := yaml.Unmarshal(raw, cm); err != nil {
		t.Fatalf("parsing %s: %v", ExampleFile, err)
	}

	values := joinscript.ValuesFromData(cm.Data)
	tmpl := &joinscript.Template{Name: cm.Name, Script: cm.Data[joinscript.ScriptKey], Values: values}

	if tmpl.Script == "" {
		t.Fatalf("example is missing the %s key", joinscript.ScriptKey)
	}

	got, err := joinscript.Render(tmpl, &joinscript.Data{
		JoinToken:      "ENCODED-TOKEN",
		TokenExpiresAt: "2026-08-12T12:00:00Z",
		APIServerURL:   APIServerURL,
		NodeName:       DPUName,
		DPUName:        DPUName,
		DPUNamespace:   Namespace,
		Values:         values,
	})
	if err != nil {
		t.Fatalf("rendering the example: %v", err)
	}

	return cm, got
}
