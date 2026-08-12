// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"code.local/k0s-dpu-bootstrapper/internal/clusteraccess"
	"code.local/k0s-dpu-bootstrapper/internal/dpf"
)

// FakeCA is the authority the fake clusters hand out, which nothing parses.
const FakeCA = "BEGIN CERTIFICATE test END CERTIFICATE"

// FakeClusters stands in for the DPU clusters, one client per DPUCluster.
type FakeClusters struct {
	clients map[string]client.Client
	calls   int
}

// NewFakeClusters returns an empty set of fake DPU clusters.
func NewFakeClusters() *FakeClusters {
	return &FakeClusters{clients: map[string]client.Client{}}
}

// Access satisfies clusteraccess.Func, building a client on first use.
func (f *FakeClusters) Access(
	_ context.Context, _ client.Reader, cluster *unstructured.Unstructured,
) (*clusteraccess.Access, error) {
	f.calls++

	key := cluster.GetNamespace() + "/" + cluster.GetName()

	c, ok := f.clients[key]
	if !ok {
		c = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
		f.clients[key] = c
	}

	return &clusteraccess.Access{
		Client:       c,
		APIServerURL: "https://" + cluster.GetName() + ".example:6443",
		CACert:       []byte(FakeCA),
	}, nil
}

// Client returns the client built for a cluster.
func (f *FakeClusters) Client(t *testing.T, ref dpf.ClusterRef) client.Client {
	t.Helper()

	c, ok := f.clients[ref.String()]
	if !ok {
		t.Fatalf("no client was built for cluster %s", ref)
	}

	return c
}

// Calls reports how many times access was built.
func (f *FakeClusters) Calls() int {
	return f.calls
}
