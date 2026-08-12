// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"code.local/k0s-dpu-bootstrapper/internal/controller"
	"code.local/k0s-dpu-bootstrapper/internal/dpf"
	"code.local/k0s-dpu-bootstrapper/internal/joinscript"
	"code.local/k0s-dpu-bootstrapper/internal/tests"
)

const testScript = "#!/usr/bin/env bash\nk0s install worker # {{ .NodeName }} {{ .JoinToken }} {{ .APIServerURL }}"

func TestReconcileWritesJoinSecret(t *testing.T) {
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	res := tests.Reconcile(t, r, tests.DPUName)
	if res.RequeueAfter != 3*time.Hour {
		t.Errorf("RequeueAfter = %s, want 3h (ttl minus refresh window)", res.RequeueAfter)
	}

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret not written: %v", err)
	}
	script := string(secret.Data[dpf.JoinSecretKey])
	if !strings.Contains(script, "k0s install worker") || !strings.Contains(script, tests.DPUName) {
		t.Errorf("script was not rendered:\n%s", script)
	}
	if secret.Annotations[controller.ManagedByAnnotation] != controller.ManagedByValue {
		t.Errorf("managed by annotation = %q", secret.Annotations[controller.ManagedByAnnotation])
	}
	if secret.Annotations[controller.TokenExpiryAnnotation] != tests.Now.Add(4*time.Hour).Format(time.RFC3339) {
		t.Errorf("expiry annotation = %q", secret.Annotations[controller.TokenExpiryAnnotation])
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "DPU" {
		t.Errorf("owner references = %+v, want one DPU reference", secret.OwnerReferences)
	}

	// The bootstrap token belongs in the DPU cluster, not the host cluster.
	tokenSecret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: "kube-system", Name: "bootstrap-token-" + secret.Annotations[controller.TokenIDAnnotation]}
	if err := clusters.Client(t, tests.ClusterOne).Get(t.Context(), key, tokenSecret); err != nil {
		t.Errorf("bootstrap token %s not created in the DPU cluster: %v", key, err)
	}
}

func TestReconcileHandlesMultipleClusters(t *testing.T) {
	// Two DPUs in two clusters, each with its own template and control plane.
	second := tests.ClusterTwo
	r, hostClient, clusters := tests.NewReconciler(t,
		tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript),
		tests.DPU("dpu-2", second, false), tests.DPUCluster(second),
		tests.JoinScript("k0s-join-2", second, "echo second {{ .APIServerURL }}"),
	)

	tests.Reconcile(t, r, tests.DPUName)
	tests.Reconcile(t, r, "dpu-2")

	first, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret for dpu-1 missing: %v", err)
	}
	if !strings.Contains(string(first.Data[dpf.JoinSecretKey]), "dpu-cluster-1.example") {
		t.Errorf("dpu-1 was not pointed at its own control plane:\n%s", first.Data[dpf.JoinSecretKey])
	}

	other, err := tests.JoinSecret(t, hostClient, "dpu-2")
	if err != nil {
		t.Fatalf("join secret for dpu-2 missing: %v", err)
	}
	script := string(other.Data[dpf.JoinSecretKey])
	if !strings.Contains(script, "echo second") || !strings.Contains(script, "dpu-cluster-2.example") {
		t.Errorf("dpu-2 got the wrong template or control plane:\n%s", script)
	}

	// Each token must land in its own cluster and nowhere else.
	for ref, want := range map[dpf.ClusterRef]string{
		tests.ClusterOne: first.Annotations[controller.TokenIDAnnotation],
		second:           other.Annotations[controller.TokenIDAnnotation],
	} {
		tokens := &corev1.SecretList{}
		if err := clusters.Client(t, ref).List(t.Context(), tokens, client.InNamespace("kube-system")); err != nil {
			t.Fatalf("listing tokens in %s: %v", ref, err)
		}
		if len(tokens.Items) != 1 {
			t.Fatalf("cluster %s holds %d tokens, want 1", ref, len(tokens.Items))
		}
		if tokens.Items[0].Name != "bootstrap-token-"+want {
			t.Errorf("cluster %s holds %q, want the token of its own DPU", ref, tokens.Items[0].Name)
		}
	}
}

func TestReconcileOverwritesDPFsOwnSecret(t *testing.T) {
	// DPF creates this Secret itself with a kubeadm join command and never updates it
	// afterwards, so taking it over is safe.
	dpfSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: tests.Namespace, Name: dpf.JoinSecretName(tests.DPUName)},
		Data:       map[string][]byte{dpf.JoinSecretKey: []byte("kubeadm join 10.0.0.1:6443 --token abcdef")},
	}
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript), dpfSecret)

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if strings.Contains(string(secret.Data[dpf.JoinSecretKey]), "kubeadm join") {
		t.Error("kubeadm join command was not replaced")
	}
}

func TestReconcileSkipsUnmanagedCluster(t *testing.T) {
	tmpl := tests.JoinScript("k0s-join", tests.ClusterOne, testScript)
	tmpl.Annotations[joinscript.ClusterNameAnnotation] = "someone-elses"
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tmpl)

	tests.Reconcile(t, r, tests.DPUName)

	if _, err := tests.JoinSecret(t, hostClient, tests.DPUName); !apierrors.IsNotFound(err) {
		t.Errorf("expected no join secret, got err = %v", err)
	}
}

func TestReconcileSkipsJoinedDPU(t *testing.T) {
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, true), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)

	if _, err := tests.JoinSecret(t, hostClient, tests.DPUName); !apierrors.IsNotFound(err) {
		t.Errorf("expected no join secret for an already joined DPU, got err = %v", err)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)
	first, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	res := tests.Reconcile(t, r, tests.DPUName)
	second, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	if first.Annotations[controller.TokenIDAnnotation] != second.Annotations[controller.TokenIDAnnotation] {
		t.Error("a second reconcile minted a new token even though the current one is still valid")
	}
	if res.RequeueAfter != 3*time.Hour {
		t.Errorf("RequeueAfter = %s, want 3h", res.RequeueAfter)
	}

	tokens := &corev1.SecretList{}
	if err := clusters.Client(t, tests.ClusterOne).List(t.Context(), tokens, client.InNamespace("kube-system")); err != nil {
		t.Fatalf("listing bootstrap tokens: %v", err)
	}
	if len(tokens.Items) != 1 {
		t.Errorf("bootstrap tokens in the DPU cluster = %d, want 1", len(tokens.Items))
	}
}

func TestReconcileRefreshesExpiringToken(t *testing.T) {
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)
	first, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	// Inside the refresh window the DPU still has not run the script, so a fresh token
	// has to replace the one about to expire.
	r.Clock = func() time.Time { return tests.Now.Add(3*time.Hour + time.Minute) }
	tests.Reconcile(t, r, tests.DPUName)

	second, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if first.Annotations[controller.TokenIDAnnotation] == second.Annotations[controller.TokenIDAnnotation] {
		t.Error("token was not re-minted inside the refresh window")
	}
}

func TestReconcileMissingDPUIsNotAnError(t *testing.T) {
	r, _, _ := tests.NewReconciler(t, tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	if res := tests.Reconcile(t, r, tests.DPUName); res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want 0", res.RequeueAfter)
	}
}

func TestDPUsForTemplate(t *testing.T) {
	second := tests.ClusterTwo
	r, _, _ := tests.NewReconciler(t,
		tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript),
		tests.DPU("dpu-2", second, false), tests.DPUCluster(second),
	)

	requests := r.DPUsForTemplate(t.Context(), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))
	if len(requests) != 1 || requests[0].Name != tests.DPUName {
		t.Errorf("requests = %+v, want only %s", requests, tests.DPUName)
	}

	unrelated := tests.JoinScript("k0s-join", tests.ClusterOne, testScript)
	unrelated.Annotations[joinscript.ClusterNameAnnotation] = "someone-elses"
	if got := r.DPUsForTemplate(t.Context(), unrelated); len(got) != 0 {
		t.Errorf("requests for an unrelated cluster = %+v, want none", got)
	}
}

func TestDPUsForDPUCluster(t *testing.T) {
	second := tests.ClusterTwo
	r, _, _ := tests.NewReconciler(t,
		tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript),
		tests.DPU("dpu-2", second, false), tests.DPU("dpu-3", second, false), tests.DPUCluster(second),
	)

	got := r.DPUsForDPUCluster(t.Context(), tests.DPUCluster(second))
	if len(got) != 2 {
		t.Fatalf("requests = %+v, want the two DPUs of that cluster", got)
	}
	for _, req := range got {
		if req.Name == tests.DPUName {
			t.Errorf("a DPU from another cluster was enqueued: %+v", got)
		}
	}
}

func TestReconcilePrefersFlavorScopedTemplate(t *testing.T) {
	// Two DPUs of one cluster can differ by flavor, each rendering its own script.
	narrowed := tests.JoinScript("k0s-join-flavor", tests.ClusterOne, "echo flavored {{ .NodeName }}")
	narrowed.Annotations[joinscript.FlavorAnnotation] = tests.Flavor
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript), narrowed)

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if !strings.Contains(string(secret.Data[dpf.JoinSecretKey]), "echo flavored") {
		t.Errorf("cluster wide template was used instead of the flavor scoped one:\n%s", secret.Data[dpf.JoinSecretKey])
	}
	if !strings.HasPrefix(secret.Annotations[controller.TemplateAnnotation], "k0s-join-flavor@") {
		t.Errorf("template annotation = %q", secret.Annotations[controller.TemplateAnnotation])
	}
}
