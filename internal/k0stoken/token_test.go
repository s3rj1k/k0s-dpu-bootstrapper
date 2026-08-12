// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package k0stoken_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"code.local/k0s-dpu-bootstrapper/internal/k0stoken"
	"code.local/k0s-dpu-bootstrapper/internal/tests"
)

func TestMintCreatesBootstrapSecret(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()

	token, err := k0stoken.Mint(t.Context(), c, tests.APIServerURL, []byte(tests.FakeCA), 2*time.Hour, tests.Now)
	if err != nil {
		t.Fatalf("k0stoken.Mint: %v", err)
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: "kube-system", Name: "bootstrap-token-" + token.ID}
	if err := c.Get(t.Context(), key, secret); err != nil {
		t.Fatalf("bootstrap token secret %s not created: %v", key, err)
	}
	if secret.Type != corev1.SecretTypeBootstrapToken {
		t.Errorf("secret type = %q, want %q", secret.Type, corev1.SecretTypeBootstrapToken)
	}
	if got := secret.StringData["token-id"]; got != token.ID {
		t.Errorf("token-id = %q, want %q", got, token.ID)
	}
	if got := secret.StringData["usage-bootstrap-authentication"]; got != "true" {
		t.Errorf("usage-bootstrap-authentication = %q, want \"true\"", got)
	}
	if got := secret.StringData["expiration"]; got != tests.Now.Add(2*time.Hour).Format(time.RFC3339) {
		t.Errorf("expiration = %q, want %q", got, tests.Now.Add(2*time.Hour).Format(time.RFC3339))
	}
	// A k0s cluster binds its bootstrap roles to the default group, so no extra groups
	// may be requested.
	if _, ok := secret.StringData["auth-extra-groups"]; ok {
		t.Error("auth-extra-groups must not be set for a k0s worker token")
	}
	if !token.ExpiresAt.Equal(tests.Now.Add(2 * time.Hour)) {
		t.Errorf("ExpiresAt = %s, want %s", token.ExpiresAt, tests.Now.Add(2*time.Hour))
	}
}

func TestMintEncodesJoinableKubeconfig(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()

	token, err := k0stoken.Mint(t.Context(), c, tests.APIServerURL, []byte(tests.FakeCA), time.Hour, tests.Now)
	if err != nil {
		t.Fatalf("k0stoken.Mint: %v", err)
	}

	cfg, err := k0stoken.Decode(token.Encoded)
	if err != nil {
		t.Fatalf("k0stoken.Decode: %v", err)
	}
	cluster, ok := cfg.Clusters["k0s"]
	if !ok {
		t.Fatalf("no k0s cluster in decoded token, got %v", cfg.Clusters)
	}
	if cluster.Server != tests.APIServerURL {
		t.Errorf("server = %q, want %q", cluster.Server, tests.APIServerURL)
	}
	if !bytes.Equal(cluster.CertificateAuthorityData, []byte(tests.FakeCA)) {
		t.Error("CA certificate was not embedded verbatim")
	}
	// A k0s worker install rejects a token whose authinfo carries any other name.
	authInfo, ok := cfg.AuthInfos["kubelet-bootstrap"]
	if !ok {
		t.Fatalf("no kubelet bootstrap authinfo in decoded token, got %v", cfg.AuthInfos)
	}
	if id, _, found := strings.Cut(authInfo.Token, "."); !found || id != token.ID {
		t.Errorf("token %q does not carry id %q", authInfo.Token, token.ID)
	}
}

func TestMintRejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		url string
		ca  []byte
		ttl time.Duration
	}{
		"no api server": {url: "", ca: []byte(tests.FakeCA), ttl: time.Hour},
		"no ca":         {url: tests.APIServerURL, ca: nil, ttl: time.Hour},
		"zero ttl":      {url: tests.APIServerURL, ca: []byte(tests.FakeCA), ttl: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
			if _, err := k0stoken.Mint(t.Context(), c, tc.url, tc.ca, tc.ttl, tests.Now); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := k0stoken.Decode("not base64 $$"); err == nil {
		t.Fatal("expected an error, got none")
	}
}
