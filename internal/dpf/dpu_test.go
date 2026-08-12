// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package dpf_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"code.local/k0s-dpu-bootstrapper/internal/dpf"
	"code.local/k0s-dpu-bootstrapper/internal/tests"
)

func TestProjectDPU(t *testing.T) {
	u := dpf.NewDPU()
	u.SetName("dpu-1")
	u.SetNamespace("dpf-operator-system")
	u.Object["spec"] = map[string]any{
		"cluster":   map[string]any{"name": "dpu-cluster-1", "namespace": "dpf-operator-system"},
		"bfb":       "bf-bundle",
		"dpuFlavor": "k0s",
		// A field this controller does not know about must not break the projection.
		"nodeEffect": map[string]any{"drain": map[string]any{"automaticNodeReboot": true}},
	}

	dpu, err := dpf.ProjectDPU(u)
	if err != nil {
		t.Fatalf("dpf.ProjectDPU: %v", err)
	}
	if dpu.Spec.Cluster.Name != "dpu-cluster-1" || dpu.Spec.Cluster.Namespace != "dpf-operator-system" {
		t.Errorf("cluster = %+v, want dpf-operator-system/dpu-cluster-1", dpu.Spec.Cluster)
	}
	if dpu.Spec.BFB != "bf-bundle" || dpu.Spec.DPUFlavor != "k0s" {
		t.Errorf("bfb/flavor = %q/%q, want bf-bundle/k0s", dpu.Spec.BFB, dpu.Spec.DPUFlavor)
	}
}

// tests.ConditionTrue is the status value of a condition that holds.
func TestKubeletConfigured(t *testing.T) {
	cases := map[string]struct {
		conditions []any
		want       bool
	}{
		"no agent status": {conditions: nil, want: false},
		"other condition only": {
			conditions: []any{map[string]any{tests.TypeKey: "KubeletStarted", tests.StatusKey: tests.ConditionTrue}},
			want:       false,
		},
		"configured false": {
			conditions: []any{map[string]any{tests.TypeKey: dpf.KubeletConfiguredCondition, tests.StatusKey: "False"}},
			want:       false,
		},
		"configured true": {
			conditions: []any{
				map[string]any{tests.TypeKey: "KubeletStarted", tests.StatusKey: tests.ConditionTrue},
				map[string]any{tests.TypeKey: dpf.KubeletConfiguredCondition, tests.StatusKey: tests.ConditionTrue},
			},
			want: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			u := dpf.NewDPU()
			u.SetName("dpu-1")
			if tc.conditions != nil {
				if err := unstructured.SetNestedSlice(u.Object, tc.conditions, "status", "agentStatus", "conditions"); err != nil {
					t.Fatalf("SetNestedSlice: %v", err)
				}
			}
			dpu, err := dpf.ProjectDPU(u)
			if err != nil {
				t.Fatalf("dpf.ProjectDPU: %v", err)
			}
			if got := dpu.KubeletConfigured(); got != tc.want {
				t.Errorf("KubeletConfigured() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProjectDPUCluster(t *testing.T) {
	u := dpf.NewDPUCluster()
	u.SetName("dpu-cluster-1")
	u.Object["spec"] = map[string]any{tests.TypeKey: dpf.StaticClusterType, "kubeconfig": "k0s-admin"}

	dc, err := dpf.ProjectDPUCluster(u)
	if err != nil {
		t.Fatalf("dpf.ProjectDPUCluster: %v", err)
	}
	if dc.Spec.Type != dpf.StaticClusterType || dc.Spec.Kubeconfig != "k0s-admin" {
		t.Errorf("spec = %+v, want static/k0s-admin", dc.Spec)
	}
}

func TestClusterRef(t *testing.T) {
	if !(dpf.ClusterRef{}).IsZero() {
		t.Error("empty dpf.ClusterRef should be zero")
	}
	if !(dpf.ClusterRef{Name: "a"}).IsZero() {
		t.Error("dpf.ClusterRef without a namespace should be zero")
	}
	ref := dpf.ClusterRef{Name: "c1", Namespace: "ns"}
	if ref.IsZero() {
		t.Error("complete dpf.ClusterRef should not be zero")
	}
	if got := ref.ObjectKey(); got.Name != "c1" || got.Namespace != "ns" {
		t.Errorf("ObjectKey() = %v, want ns/c1", got)
	}
}

func TestJoinSecretName(t *testing.T) {
	// Must match the name DPF uses, since in zero trust mode the agent may only read a
	// Secret called exactly this.
	if got := dpf.JoinSecretName("dpu-1"); got != "dpu-1-kubeadm-join" {
		t.Errorf("dpf.JoinSecretName = %q, want dpu-1-kubeadm-join", got)
	}
}

func TestOwnerReference(t *testing.T) {
	u := dpf.NewDPU()
	u.SetName("dpu-1")
	u.SetUID("uid-1")

	ref := dpf.OwnerReference(u)
	if ref.Kind != "DPU" || ref.Name != "dpu-1" || string(ref.UID) != "uid-1" {
		t.Errorf("owner reference = %+v", ref)
	}
	if ref.APIVersion != dpf.GroupName+"/"+dpf.Version {
		t.Errorf("apiVersion = %q, want %q", ref.APIVersion, dpf.GroupName+"/"+dpf.Version)
	}
}
