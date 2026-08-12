// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package joinscript_test

import (
	"strings"
	"testing"

	"code.local/k0s-dpu-bootstrapper/internal/dpf"
	"code.local/k0s-dpu-bootstrapper/internal/joinscript"
	"code.local/k0s-dpu-bootstrapper/internal/tests"
)

const (
	testExtraArgs = "--labels dpu=true"
	testCRISocket = "remote:unix:///run/containerd/containerd.sock"
	testScript    = "echo hi"
)

var testTarget = joinscript.Target{Cluster: tests.ClusterOne, Flavor: tests.Flavor}

func TestResolve(t *testing.T) {
	c := tests.Client(t, tests.JoinScriptConfigMap("k0s-join", tests.ClusterOne, map[string]string{
		joinscript.ScriptKey: "echo {{ .DPUName }}",
		"k0sVersion":         "v1.34.9",
		"extraArgs":          "--labels dpu=true",
	}))

	tmpl, err := joinscript.Resolve(t.Context(), c, tests.Namespace, testTarget)
	if err != nil {
		t.Fatalf("joinscript.Resolve: %v", err)
	}
	if tmpl == nil {
		t.Fatal("expected a template, got nil")
	}
	if tmpl.Script != "echo {{ .DPUName }}" {
		t.Errorf("script = %q", tmpl.Script)
	}
	if tmpl.Values["k0sVersion"] != "v1.34.9" || tmpl.Values["extraArgs"] != testExtraArgs {
		t.Errorf("values = %v, want every key except the script", tmpl.Values)
	}
	if _, ok := tmpl.Values[joinscript.ScriptKey]; ok {
		t.Error("the script key must not be exposed as a value")
	}
	if !strings.HasPrefix(tmpl.Ref(), "k0s-join@") {
		t.Errorf("Ref() = %q, want it to start with k0s-join@", tmpl.Ref())
	}
}

func TestResolveNoMatchIsNotAnError(t *testing.T) {
	// A DPU in a cluster this controller does not manage must be left alone, with DPF's
	// own kubeadm join Secret untouched.
	other := dpf.ClusterRef{Name: "someone-elses", Namespace: tests.Namespace}
	c := tests.Client(t, tests.JoinScriptConfigMap("k0s-join", other, map[string]string{joinscript.ScriptKey: testScript}))

	tmpl, err := joinscript.Resolve(t.Context(), c, tests.Namespace, testTarget)
	if err != nil {
		t.Fatalf("joinscript.Resolve: %v", err)
	}
	if tmpl != nil {
		t.Errorf("expected no template, got %q", tmpl.Name)
	}
}

func TestResolveAmbiguousIsAnError(t *testing.T) {
	c := tests.Client(t,
		tests.JoinScriptConfigMap("first", tests.ClusterOne, map[string]string{joinscript.ScriptKey: "echo one"}),
		tests.JoinScriptConfigMap("second", tests.ClusterOne, map[string]string{joinscript.ScriptKey: "echo two"}),
	)

	_, err := joinscript.Resolve(t.Context(), c, tests.Namespace, testTarget)
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "exactly one is required") {
		t.Errorf("error = %q", err)
	}
}

func TestResolveMissingScriptKey(t *testing.T) {
	c := tests.Client(t, tests.JoinScriptConfigMap("k0s-join", tests.ClusterOne, map[string]string{"wrong-key": testScript}))

	if _, err := joinscript.Resolve(t.Context(), c, tests.Namespace, testTarget); err == nil {
		t.Fatal("expected an error, got none")
	}
}

func TestRender(t *testing.T) {
	tmpl := &joinscript.Template{
		Name: "k0s-join",
		Script: "k0s install worker --cri-socket {{ .Values.criSocket }} " +
			"{{ .Values.extraArgs }} # {{ .NodeName }} {{ .JoinToken }}",
		Values: map[string]string{"extraArgs": testExtraArgs, "criSocket": testCRISocket},
	}

	got, err := joinscript.Render(tmpl, &joinscript.Data{
		JoinToken: "ENCODED",
		NodeName:  tests.DPUName,
		Values:    tmpl.Values,
	})
	if err != nil {
		t.Fatalf("joinscript.Render: %v", err)
	}
	for _, want := range []string{"ENCODED", tests.DPUName, testExtraArgs, testCRISocket} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered script is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderMissingValueIsAnError(t *testing.T) {
	// The rendered script runs as root on the DPU, so a typo must fail loudly instead of
	// substituting an empty string.
	tmpl := &joinscript.Template{Name: "k0s-join", Script: "curl -o k0s {{ .Values.k0sURL }}", Values: map[string]string{}}

	if _, err := joinscript.Render(tmpl, &joinscript.Data{Values: tmpl.Values}); err == nil {
		t.Fatal("expected an error, got none")
	}
}

func TestClusterRefFromAnnotations(t *testing.T) {
	got := joinscript.ClusterRefFromAnnotations(map[string]string{
		joinscript.ClusterNameAnnotation:      "c1",
		joinscript.ClusterNamespaceAnnotation: "ns",
	})
	if got != (dpf.ClusterRef{Name: "c1", Namespace: "ns"}) {
		t.Errorf("joinscript.ClusterRefFromAnnotations = %+v", got)
	}
	if !joinscript.ClusterRefFromAnnotations(nil).IsZero() {
		t.Error("missing annotations should give a zero ClusterRef")
	}
}

func TestResolvePrefersFlavorScopedTemplate(t *testing.T) {
	c := tests.Client(t,
		tests.JoinScriptConfigMap("cluster-wide", tests.ClusterOne, map[string]string{joinscript.ScriptKey: "echo wide"}),
		tests.ScopedToFlavor(tests.JoinScriptConfigMap("for-flavor", tests.ClusterOne,
			map[string]string{joinscript.ScriptKey: "echo narrow"}), testTarget.Flavor),
	)

	tmpl, err := joinscript.Resolve(t.Context(), c, tests.Namespace, testTarget)
	if err != nil {
		t.Fatalf("joinscript.Resolve: %v", err)
	}
	if tmpl == nil || tmpl.Name != "for-flavor" {
		t.Fatalf("resolved %v, want the flavor scoped template", tmpl)
	}
}

func TestResolveFallsBackWhenFlavorDoesNotMatch(t *testing.T) {
	c := tests.Client(t,
		tests.JoinScriptConfigMap("cluster-wide", tests.ClusterOne, map[string]string{joinscript.ScriptKey: "echo wide"}),
		tests.ScopedToFlavor(tests.JoinScriptConfigMap("other-flavor", tests.ClusterOne, map[string]string{joinscript.ScriptKey: "echo other"}), "bf3-storage"),
	)

	tmpl, err := joinscript.Resolve(t.Context(), c, tests.Namespace, testTarget)
	if err != nil {
		t.Fatalf("joinscript.Resolve: %v", err)
	}
	if tmpl == nil || tmpl.Name != "cluster-wide" {
		t.Fatalf("resolved %v, want the cluster wide template", tmpl)
	}
}

func TestResolveSkipsWhenOnlyAnotherFlavorMatches(t *testing.T) {
	// A cluster served only by templates for other flavors is not ours to touch.
	c := tests.Client(t, tests.ScopedToFlavor(tests.JoinScriptConfigMap("other-flavor", tests.ClusterOne,
		map[string]string{joinscript.ScriptKey: "echo other"}), "bf3-storage"))

	tmpl, err := joinscript.Resolve(t.Context(), c, tests.Namespace, testTarget)
	if err != nil {
		t.Fatalf("joinscript.Resolve: %v", err)
	}
	if tmpl != nil {
		t.Errorf("resolved %q, want nothing", tmpl.Name)
	}
}

func TestResolveAmbiguousFlavorScopeIsAnError(t *testing.T) {
	c := tests.Client(t,
		tests.ScopedToFlavor(tests.JoinScriptConfigMap("first", tests.ClusterOne, map[string]string{joinscript.ScriptKey: "echo one"}), testTarget.Flavor),
		tests.ScopedToFlavor(tests.JoinScriptConfigMap("second", tests.ClusterOne, map[string]string{joinscript.ScriptKey: "echo two"}), testTarget.Flavor),
	)

	_, err := joinscript.Resolve(t.Context(), c, tests.Namespace, testTarget)
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "exactly one is required") {
		t.Errorf("error = %q", err)
	}
}

func TestValuesFromData(t *testing.T) {
	values := joinscript.ValuesFromData(map[string]string{
		joinscript.ScriptKey: testScript,
		"k0sURL":             "https://artifacts.internal/k0s",
	})
	if _, ok := values[joinscript.ScriptKey]; ok {
		t.Error("the script key must not be exposed as a value")
	}
	if values["k0sURL"] != "https://artifacts.internal/k0s" {
		t.Errorf("values = %v", values)
	}

	// A ConfigMap with no data must not panic the clone.
	if got := joinscript.ValuesFromData(nil); len(got) != 0 {
		t.Errorf("ValuesFromData(nil) = %v, want empty", got)
	}
}
