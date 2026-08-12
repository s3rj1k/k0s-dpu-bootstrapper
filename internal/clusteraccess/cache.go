// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package clusteraccess

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type entry struct {
	access        *Access
	secretVersion string
}

// Cache memoises access per DPUCluster, dropping an entry when its kubeconfig Secret
// changes. Nothing in the controller runtime libraries keys clients this way.
type Cache struct {
	entries map[types.NamespacedName]entry
	mu      sync.Mutex
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{entries: map[types.NamespacedName]entry{}}
}

// Get returns access to a DPU cluster, reusing the client while its kubeconfig holds.
func (c *Cache) Get(
	ctx context.Context, hostClient client.Reader, cluster *unstructured.Unstructured,
) (*Access, error) {
	secret, err := KubeconfigSecret(ctx, hostClient, cluster)
	if err != nil {
		return nil, err
	}

	key := types.NamespacedName{Namespace: cluster.GetNamespace(), Name: cluster.GetName()}

	c.mu.Lock()
	cached, ok := c.entries[key]
	c.mu.Unlock()

	if ok && cached.secretVersion == secret.ResourceVersion {
		return cached.access, nil
	}

	access, err := FromSecret(secret)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.entries[key] = entry{secretVersion: secret.ResourceVersion, access: access}
	c.mu.Unlock()

	return access, nil
}

// Len reports how many clusters are currently cached.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.entries)
}
