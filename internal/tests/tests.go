// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

// Package tests holds the builders and fakes shared by the test packages.
package tests

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"code.local/k0s-dpu-bootstrapper/internal/dpf"
	"code.local/k0s-dpu-bootstrapper/internal/joinscript"
)

const (
	// Namespace is the namespace every fixture is built in.
	Namespace = "dpf-operator-system"
	// DPUName is the name of the DPU most tests reconcile.
	DPUName = "dpu-1"
	// Flavor is the DPUFlavor the fixtures carry.
	Flavor = "bf3-default"
	// ConditionTrue is the status of a condition that holds.
	ConditionTrue = "True"
	// TypeKey is the object field naming a kind or a cluster manager.
	TypeKey = "type"
	// StatusKey is the condition field carrying its status.
	StatusKey = "status"
)

// Cluster returns a reference to a DPU cluster in the shared namespace.
func Cluster(name string) dpf.ClusterRef {
	return dpf.ClusterRef{Name: name, Namespace: Namespace}
}

// ClusterOne and ClusterTwo are the DPU clusters the suites reconcile against.
var (
	ClusterOne = Cluster("dpu-cluster-1")
	ClusterTwo = Cluster("dpu-cluster-2")
)

// Scheme registers the core types plus the DPF kinds as unstructured.
func Scheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}

	gv := dpf.DPUGVK.GroupVersion()
	for _, kind := range []string{dpf.DPUGVK.Kind, dpf.DPUClusterGVK.Kind} {
		s.AddKnownTypeWithName(gv.WithKind(kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gv.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}

	return s
}

// Client returns a fake host cluster client with the DPU index registered.
func Client(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(Scheme(t)).
		WithIndex(dpf.NewDPU(), dpf.ClusterIndexKey, dpf.ClusterIndexValue).
		WithObjects(objects...).
		Build()
}

// CoreClient returns a fake client that knows only the core types.
func CoreClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objects...).Build()
}

// DPU builds a DPU assigned to a cluster, joined ones carrying the agent condition.
func DPU(name string, cluster dpf.ClusterRef, joined bool) *unstructured.Unstructured {
	u := dpf.NewDPU()
	u.SetName(name)
	u.SetNamespace(Namespace)
	u.SetUID(types.UID(name + "-uid"))
	u.Object["spec"] = map[string]any{
		"cluster":   map[string]any{"name": cluster.Name, "namespace": cluster.Namespace},
		"dpuFlavor": Flavor,
	}

	if joined {
		u.Object["status"] = map[string]any{"agentStatus": map[string]any{"conditions": []any{
			map[string]any{TypeKey: dpf.KubeletConfiguredCondition, StatusKey: ConditionTrue},
		}}}
	}

	return u
}

// KubeconfigSecretName is the Secret a DPUCluster fixture points at.
func KubeconfigSecretName(cluster string) string {
	return cluster + "-admin"
}

// DPUCluster builds a static DPUCluster naming its own kubeconfig Secret.
func DPUCluster(ref dpf.ClusterRef) *unstructured.Unstructured {
	u := dpf.NewDPUCluster()
	u.SetName(ref.Name)
	u.SetNamespace(ref.Namespace)
	u.Object["spec"] = map[string]any{
		TypeKey:      dpf.StaticClusterType,
		"kubeconfig": KubeconfigSecretName(ref.Name),
	}

	return u
}

// JoinScriptConfigMap builds a labeled template ConfigMap scoped to a cluster.
func JoinScriptConfigMap(name string, cluster dpf.ClusterRef, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: Namespace,
			Labels:    map[string]string{joinscript.TemplateLabel: joinscript.TemplateLabelValue},
			Annotations: map[string]string{
				joinscript.ClusterNameAnnotation:      cluster.Name,
				joinscript.ClusterNamespaceAnnotation: cluster.Namespace,
			},
		},
		Data: data,
	}
}

// JoinScript builds a template ConfigMap carrying nothing but the script.
func JoinScript(name string, cluster dpf.ClusterRef, script string) *corev1.ConfigMap {
	return JoinScriptConfigMap(name, cluster, map[string]string{joinscript.ScriptKey: script})
}

// ScopedToFlavor narrows a template ConfigMap to one DPUFlavor.
func ScopedToFlavor(cm *corev1.ConfigMap, flavor string) *corev1.ConfigMap {
	cm.Annotations[joinscript.FlavorAnnotation] = flavor

	return cm
}

// KubeconfigSecret builds the Secret a DPUCluster resolves its kubeconfig from.
func KubeconfigSecret(t *testing.T, cluster string) *corev1.Secret {
	t.Helper()

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KubeconfigSecretName(cluster), Namespace: Namespace},
		Data:       map[string][]byte{dpf.KubeconfigSecretKey: Kubeconfig(string(CACertificate(t, cluster)), "")},
	}
}
