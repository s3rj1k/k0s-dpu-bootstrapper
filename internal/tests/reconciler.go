// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"code.local/k0s-dpu-bootstrapper/internal/controller"
	"code.local/k0s-dpu-bootstrapper/internal/dpf"
)

const (
	// TokenTTL is the token lifetime the reconciler under test mints with.
	TokenTTL = 4 * time.Hour
	// RefreshBefore is how much validity is left when it mints again.
	RefreshBefore = time.Hour
)

// Now is the fixed instant the tests reckon token expiry from.
var Now = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

// NewReconciler wires a reconciler against a fake host cluster and fake DPU clusters.
func NewReconciler(t *testing.T, objects ...client.Object) (*controller.DPUReconciler, client.Client, *FakeClusters) {
	t.Helper()

	hostClient := Client(t, objects...)
	clusters := NewFakeClusters()

	return &controller.DPUReconciler{
		Client:            hostClient,
		Recorder:          &Recorder{},
		TemplateNamespace: Namespace,
		TokenTTL:          TokenTTL,
		RefreshBefore:     RefreshBefore,
		Clock:             func() time.Time { return Now },
		NewClusterAccess:  clusters.Access,
	}, hostClient, clusters
}

// Request addresses one DPU of the shared namespace.
func Request(dpuName string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: Namespace, Name: dpuName}}
}

// Reconcile runs one reconcile for a DPU and fails the test on error.
func Reconcile(t *testing.T, r *controller.DPUReconciler, dpuName string) ctrl.Result {
	t.Helper()

	res, err := r.Reconcile(t.Context(), Request(dpuName))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	return res
}

// ReconcileFails runs one reconcile that has to fail and returns what it reported. The
// message rather than the error, so that a caller with nothing to assert can ignore it.
func ReconcileFails(t *testing.T, r *controller.DPUReconciler, dpuName string) string {
	t.Helper()

	_, err := r.Reconcile(t.Context(), Request(dpuName))
	if err == nil {
		t.Fatal("expected an error, got none")
	}

	return err.Error()
}

// Tokens lists the bootstrap tokens standing in one DPU cluster.
func Tokens(t *testing.T, clusters *FakeClusters, ref dpf.ClusterRef) []corev1.Secret {
	t.Helper()

	list := &corev1.SecretList{}
	if err := clusters.Client(t, ref).List(t.Context(), list, client.InNamespace("kube-system")); err != nil {
		t.Fatalf("listing bootstrap tokens in %s: %v", ref, err)
	}

	return list.Items
}

// ExpectNoTokens fails the test if a token was minted and left behind.
func ExpectNoTokens(t *testing.T, clusters *FakeClusters, ref dpf.ClusterRef) {
	t.Helper()

	if tokens := Tokens(t, clusters, ref); len(tokens) != 0 {
		t.Errorf("bootstrap tokens left in %s = %d, want none", ref, len(tokens))
	}
}

// TokenSecretHalf reads the secret half of a bootstrap token Secret, from whichever field
// carries it. A real API server folds StringData into Data, the fake client leaves it.
func TokenSecretHalf(secret *corev1.Secret) string {
	if half := string(secret.Data["token-secret"]); half != "" {
		return half
	}

	return secret.StringData["token-secret"]
}

// TemplateRef reads back the revision of a template ConfigMap, the way the resolver names it.
func TemplateRef(t *testing.T, c client.Client, name string) string {
	t.Helper()

	cm := &corev1.ConfigMap{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: Namespace, Name: name}, cm); err != nil {
		t.Fatalf("getting template %s: %v", name, err)
	}

	return cm.Name + "@" + cm.ResourceVersion
}

// PreviousJoinSecret builds the join Secret a build older than the hash annotations left
// behind, carrying the ones that existed then and neither of the two that decide currency.
func PreviousJoinSecret(dpuName, templateRef, script string, expiresAt time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: Namespace,
			Name:      dpf.JoinSecretName(dpuName),
			Annotations: map[string]string{
				controller.ManagedByAnnotation:   controller.ManagedByValue,
				controller.TemplateAnnotation:    templateRef,
				controller.TokenExpiryAnnotation: expiresAt.Format(time.RFC3339),
				controller.TokenIDAnnotation:     "aaaaaa",
			},
		},
		Data: map[string][]byte{dpf.JoinSecretKey: []byte(script)},
	}
}

// JoinSecret reads the join Secret a DPU would have been given.
func JoinSecret(t *testing.T, c client.Client, dpuName string) (*corev1.Secret, error) {
	t.Helper()

	secret := &corev1.Secret{}
	err := c.Get(t.Context(), types.NamespacedName{
		Namespace: Namespace,
		Name:      dpf.JoinSecretName(dpuName),
	}, secret)

	return secret, err
}
