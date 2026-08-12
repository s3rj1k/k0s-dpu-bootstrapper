// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

// Package clusteraccess builds and caches clients for the DPU clusters a DPUCluster
// object names.
package clusteraccess

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"code.local/k0s-dpu-bootstrapper/internal/dpf"
)

// Access is everything needed to talk to a DPU cluster and to build a join token for it.
type Access struct {
	// Client talks to the DPU cluster, uncached since only token Secrets are created.
	Client client.Client
	// APIServerURL is the endpoint DPUs reach the control plane on.
	APIServerURL string
	// CACert is the cluster CA in PEM form, embedded into the join token.
	CACert []byte
}

// Func builds access to a DPU cluster, so tests can substitute a fake.
type Func func(ctx context.Context, hostClient client.Reader, cluster *unstructured.Unstructured) (*Access, error)

// ParseKubeconfig turns an admin kubeconfig into a rest config and the cluster CA, which
// must be embedded and belong to a single cluster entry.
func ParseKubeconfig(raw []byte) (*rest.Config, []byte, error) {
	clientConfig, err := clientcmd.NewClientConfigFromBytes(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("building rest config: %w", err)
	}

	if restConfig.Host == "" {
		return nil, nil, errors.New("kubeconfig has no server address")
	}

	rawConfig, err := clientConfig.RawConfig()
	if err != nil {
		return nil, nil, err
	}

	if len(rawConfig.Clusters) != 1 {
		return nil, nil, fmt.Errorf("expected exactly one cluster entry, got %d", len(rawConfig.Clusters))
	}

	for _, cluster := range rawConfig.Clusters {
		if len(cluster.CertificateAuthorityData) == 0 {
			return nil, nil, errors.New("no certificate-authority-data; the CA must be embedded, not referenced by path")
		}

		return restConfig, cluster.CertificateAuthorityData, nil
	}

	return nil, nil, errors.New("no cluster entry")
}

// FromSecret turns a kubeconfig Secret into cluster access.
func FromSecret(secret *corev1.Secret) (*Access, error) {
	key := types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}

	raw, ok := secret.Data[dpf.KubeconfigSecretKey]
	if !ok {
		return nil, fmt.Errorf("kubeconfig secret %s has no %q key", key, dpf.KubeconfigSecretKey)
	}

	restConfig, caCert, err := ParseKubeconfig(raw)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig secret %s: %w", key, err)
	}

	c, err := client.New(restConfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("building client from kubeconfig secret %s: %w", key, err)
	}

	return &Access{Client: c, APIServerURL: restConfig.Host, CACert: caCert}, nil
}

// KubeconfigSecret resolves the Secret named by a DPUCluster's spec.kubeconfig.
func KubeconfigSecret(ctx context.Context, hostClient client.Reader, cluster *unstructured.Unstructured) (*corev1.Secret, error) {
	dc, err := dpf.ProjectDPUCluster(cluster)
	if err != nil {
		return nil, err
	}

	if dc.Spec.Kubeconfig == "" {
		return nil, fmt.Errorf("DPUCluster %s/%s has no spec.kubeconfig", cluster.GetNamespace(), cluster.GetName())
	}

	secret := &corev1.Secret{}

	key := types.NamespacedName{Namespace: cluster.GetNamespace(), Name: dc.Spec.Kubeconfig}
	if err := hostClient.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("getting kubeconfig secret %s: %w", key, err)
	}

	return secret, nil
}

// New reads a DPUCluster's kubeconfig Secret and builds access to it.
func New(ctx context.Context, hostClient client.Reader, cluster *unstructured.Unstructured) (*Access, error) {
	secret, err := KubeconfigSecret(ctx, hostClient, cluster)
	if err != nil {
		return nil, err
	}

	return FromSecret(secret)
}
