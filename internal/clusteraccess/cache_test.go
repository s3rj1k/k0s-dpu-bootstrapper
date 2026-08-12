// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package clusteraccess_test

import (
	"bytes"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"code.local/k0s-dpu-bootstrapper/internal/clusteraccess"
	"code.local/k0s-dpu-bootstrapper/internal/dpf"
	"code.local/k0s-dpu-bootstrapper/internal/tests"
)

func TestClusterAccessCacheReusesAndSeparates(t *testing.T) {
	first := tests.DPUCluster(tests.Cluster("cluster-a"))
	second := tests.DPUCluster(tests.Cluster("cluster-b"))
	hostClient := tests.CoreClient(t, tests.KubeconfigSecret(t, "cluster-a"), tests.KubeconfigSecret(t, "cluster-b"))

	cache := clusteraccess.NewCache()
	one, err := cache.Get(t.Context(), hostClient, first)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	again, err := cache.Get(t.Context(), hostClient, first)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if one != again {
		t.Error("a second Get for the same cluster rebuilt the client")
	}

	// A second dpf.DPUCluster gets its own entry rather than reusing the first.
	other, err := cache.Get(t.Context(), hostClient, second)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if other == one {
		t.Error("two DPUClusters shared one access entry")
	}
	if cache.Len() != 2 {
		t.Errorf("cached clusters = %d, want 2", cache.Len())
	}
}

func TestClusterAccessCacheInvalidatesOnKubeconfigChange(t *testing.T) {
	cluster := tests.DPUCluster(tests.Cluster("cluster-a"))
	secret := tests.KubeconfigSecret(t, "cluster-a")
	hostClient := tests.CoreClient(t, secret)

	cache := clusteraccess.NewCache()
	before, err := cache.Get(t.Context(), hostClient, cluster)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Rotating the credential must produce a fresh client.
	rotated := tests.CACertificate(t, "rotated")
	secret.Data[dpf.KubeconfigSecretKey] = tests.Kubeconfig(string(rotated), "")
	if err := hostClient.Update(t.Context(), secret); err != nil {
		t.Fatalf("updating kubeconfig secret: %v", err)
	}
	after, getErr := cache.Get(t.Context(), hostClient, cluster)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if before == after {
		t.Error("cache did not notice the rotated kubeconfig")
	}
	if !bytes.Equal(after.CACert, rotated) {
		t.Error("cached access still carries the previous certificate authority")
	}
}

func TestClusterAccessErrors(t *testing.T) {
	cases := map[string]struct {
		cluster *unstructured.Unstructured
		objects []client.Object
	}{
		"no kubeconfig field": {cluster: tests.DPUClusterWithoutKubeconfig()},
		"missing secret":      {cluster: tests.DPUCluster(tests.Cluster("cluster-a"))},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hostClient := tests.CoreClient(t, tc.objects...)
			if _, err := clusteraccess.New(t.Context(), hostClient, tc.cluster); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestClusterAccessRejectsSecretWithoutExpectedKey(t *testing.T) {
	cluster := tests.DPUCluster(tests.Cluster("cluster-a"))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tests.KubeconfigSecretName("cluster-a"), Namespace: tests.Namespace},
		Data:       map[string][]byte{"admin.conf": tests.Kubeconfig(string(tests.CACertificate(t, "a")), "")},
	}
	hostClient := tests.CoreClient(t, secret)

	if _, err := clusteraccess.New(t.Context(), hostClient, cluster); err == nil {
		t.Fatal("expected an error, got none")
	}
}
