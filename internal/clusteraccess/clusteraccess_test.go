// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package clusteraccess_test

import (
	"strings"
	"testing"

	"code.local/k0s-dpu-bootstrapper/internal/clusteraccess"
	"code.local/k0s-dpu-bootstrapper/internal/tests"
)

func TestParseKubeconfig(t *testing.T) {
	restConfig, ca, err := clusteraccess.ParseKubeconfig(tests.Kubeconfig("test-ca", ""))
	if err != nil {
		t.Fatalf("clusteraccess.ParseKubeconfig: %v", err)
	}
	if restConfig.Host != "https://vip.example:6443" {
		t.Errorf("host = %q, want https://vip.example:6443", restConfig.Host)
	}
	if string(ca) != "test-ca" {
		t.Errorf("ca = %q, want test-ca", ca)
	}
}

func TestParseKubeconfigErrors(t *testing.T) {
	secondCluster := tests.ExtraCluster(t)

	cases := map[string]struct {
		want string
		raw  []byte
	}{
		// A join token embeds the CA, so a path reference is unusable.
		"no embedded ca":      {raw: tests.Kubeconfig("", ""), want: "certificate-authority-data"},
		"two cluster entries": {raw: tests.Kubeconfig("test-ca", secondCluster), want: "exactly one cluster entry"},
		"not a kubeconfig":    {raw: []byte("{{"), want: "loading kubeconfig"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := clusteraccess.ParseKubeconfig(tc.raw)
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
