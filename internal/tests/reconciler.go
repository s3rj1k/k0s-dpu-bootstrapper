// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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
	// EventBuffer is how many events the recorder holds before it blocks.
	EventBuffer = 20
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
		Recorder:          record.NewFakeRecorder(EventBuffer),
		TemplateNamespace: Namespace,
		TokenTTL:          TokenTTL,
		RefreshBefore:     RefreshBefore,
		Clock:             func() time.Time { return Now },
		NewClusterAccess:  clusters.Access,
	}, hostClient, clusters
}

// Reconcile runs one reconcile for a DPU and fails the test on error.
func Reconcile(t *testing.T, r *controller.DPUReconciler, dpuName string) ctrl.Result {
	t.Helper()

	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: Namespace, Name: dpuName},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	return res
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
