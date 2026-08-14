// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package joinscript_test

import (
	"os/exec"
	"strings"
	"testing"

	"code.local/k0s-dpu-bootstrapper/internal/joinscript"
	"code.local/k0s-dpu-bootstrapper/internal/tests"
)

// TestExampleTemplateRenders keeps the shipped example honest. It must parse, carry the
// keys the resolver requires, and render with no placeholder left behind.
func TestExampleTemplateRenders(t *testing.T) {
	cm, got := tests.LoadExample(t)

	if cm.Labels[joinscript.TemplateLabel] != joinscript.TemplateLabelValue {
		t.Errorf("example is missing the %s=%s label", joinscript.TemplateLabel, joinscript.TemplateLabelValue)
	}

	if joinscript.ClusterRefFromAnnotations(cm.Annotations).IsZero() {
		t.Error("example is missing its cluster scoping annotations")
	}

	if strings.Contains(got, "{{") {
		t.Errorf("rendered example still contains a placeholder:\n%s", got)
	}

	for _, want := range []string{"ENCODED-TOKEN", "install worker", "--cri-socket"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered example is missing %q", want)
		}
	}
}

// TestExampleValuesRenderVerbatim is the guard on the multipart shape. Every value the
// example carries has to reach the script whole, which a stray indent would break.
func TestExampleValuesRenderVerbatim(t *testing.T) {
	cm, got := tests.LoadExample(t)

	for key, value := range joinscript.ValuesFromData(cm.Data) {
		if strings.TrimSpace(value) == "" {
			continue
		}

		// A short scalar can occur inside another, so only its presence is checked. A
		// value spanning lines is the one at risk from indentation, and must appear once.
		count := strings.Count(got, value)
		if strings.Contains(value, "\n") && count != 1 {
			t.Errorf("value %q appears %d times whole, want once.\nvalue:\n%s\nscript:\n%s",
				key, count, value, got)
		}

		if count == 0 {
			t.Errorf("value %q never reaches the rendered script.\nvalue:\n%s", key, value)
		}
	}
}

// TestExampleStepsRenderInOrder checks that the skeleton runs the steps in the documented
// order, since a step that lands before the one it depends on would fail on the node.
func TestExampleStepsRenderInOrder(t *testing.T) {
	cm, got := tests.LoadExample(t)
	values := joinscript.ValuesFromData(cm.Data)

	previous := -1

	for _, step := range tests.ExampleSteps {
		value, ok := values[step]
		if !ok {
			t.Fatalf("example has no %q key, so the skeleton and this test disagree", step)
		}

		if strings.TrimSpace(value) == "" {
			continue
		}

		at := strings.Index(got, value)
		if at < 0 {
			t.Fatalf("step %q is missing from the rendered script", step)
		}

		if at < previous {
			t.Errorf("step %q renders out of order", step)
		}

		previous = at
	}
}

// TestExamplePassesValidation keeps the shipped example on the right side of the check the
// controller applies, since a rejected one would never reach a DPU.
func TestExamplePassesValidation(t *testing.T) {
	_, got := tests.LoadExample(t)

	if err := joinscript.Validate(got, tests.ExampleFile); err != nil {
		t.Errorf("the shipped example does not pass validation: %v", err)
	}
}

// TestExampleRendersValidShell parses the rendered script with the real bash, which is what
// runs it and what the Go parser only stands in for.
func TestExampleRendersValidShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}

	_, got := tests.LoadExample(t)

	cmd := exec.CommandContext(t.Context(), "bash", "-n")
	cmd.Stdin = strings.NewReader(got)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("rendered script is not valid shell: %v\n%s\nscript:\n%s", err, out, got)
	}
}
