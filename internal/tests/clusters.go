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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"code.local/k0s-dpu-bootstrapper/internal/clusteraccess"
	"code.local/k0s-dpu-bootstrapper/internal/dpf"
)

// FakeCA is the authority the fake clusters hand out, which nothing parses.
const FakeCA = "BEGIN CERTIFICATE test END CERTIFICATE"

// FakeClusters stands in for the DPU clusters, one client per DPUCluster.
type FakeClusters struct {
	clients map[string]client.Client
	// AccessErr stands in for a DPU cluster that cannot be reached at all.
	AccessErr error
	// Intercept is applied to every cluster client, for failing token writes.
	Intercept interceptor.Funcs
	// APIServerURL overrides the address handed out, for a control plane that moves.
	APIServerURL string
	calls        int
}

// NewFakeClusters returns an empty set of fake DPU clusters.
func NewFakeClusters() *FakeClusters {
	return &FakeClusters{clients: map[string]client.Client{}}
}

// RefuseTokenWrites makes every cluster reject the Secret a bootstrap token is written as.
func (f *FakeClusters) RefuseTokenWrites(err error) {
	f.Intercept.Create = func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return err
	}
}

// RefuseTokenDeletes makes revocation fail, which leaves a live token behind.
func (f *FakeClusters) RefuseTokenDeletes(err error) {
	f.Intercept.Delete = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		return err
	}
}

// Access satisfies clusteraccess.Func, building a client on first use.
func (f *FakeClusters) Access(
	_ context.Context, _ client.Reader, cluster *unstructured.Unstructured,
) (*clusteraccess.Access, error) {
	f.calls++

	if f.AccessErr != nil {
		return nil, f.AccessErr
	}

	key := cluster.GetNamespace() + "/" + cluster.GetName()

	c, ok := f.clients[key]
	if !ok {
		c = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(f.Intercept).Build()
		f.clients[key] = c
	}

	url := f.APIServerURL
	if url == "" {
		url = "https://" + cluster.GetName() + ".example:6443"
	}

	return &clusteraccess.Access{Client: c, APIServerURL: url, CACert: []byte(FakeCA)}, nil
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
