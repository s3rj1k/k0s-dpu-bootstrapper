// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// APIServerURL is the endpoint the generated kubeconfigs point at.
const APIServerURL = "https://vip.example:6443"

// CACertificate returns a self signed certificate in PEM form, which building a client
// parses immediately.
func CACertificate(t *testing.T, commonName string) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// Kubeconfig renders an admin kubeconfig. An empty authority leaves the certificate out
// and extraCluster appends a second entry, both of which the parser rejects.
func Kubeconfig(authority, extraCluster string) []byte {
	embedded := ""
	if authority != "" {
		embedded = "    certificate-authority-data: " + base64.StdEncoding.EncodeToString([]byte(authority)) + "\n"
	}

	return []byte(`apiVersion: v1
kind: Config
clusters:
- name: k0s
  cluster:
    server: ` + APIServerURL + `
` + embedded + extraCluster + `contexts:
- name: k0s
  context:
    cluster: k0s
    user: admin
current-context: k0s
users:
- name: admin
  user:
    token: secret
`)
}

// ExtraCluster is a second cluster entry, which a kubeconfig may not carry.
func ExtraCluster(t *testing.T) string {
	t.Helper()

	return "- name: other\n  cluster:\n    server: https://other.example:6443\n" +
		"    certificate-authority-data: " +
		base64.StdEncoding.EncodeToString(CACertificate(t, "other")) + "\n"
}
