// SPDX-FileCopyrightText: 2020 k0s authors
// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

// Package k0stoken mints k0s worker join tokens, derived from k0s pkg/token.
package k0stoken

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	tokenutil "k8s.io/cluster-bootstrap/token/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Authinfo name a k0s worker install requires.
	workerTokenAuthName = "kubelet-bootstrap"

	// Context name k0s writes into its own join tokens.
	contextName = "k0s"
)

// Token is a minted k0s worker join token.
type Token struct {
	// ExpiresAt is when the token stops authenticating.
	ExpiresAt time.Time
	// ID is the part before the dot, which also names the token Secret.
	ID string
	// Encoded is the bootstrap kubeconfig, gzipped and base64 encoded.
	Encoded string
}

// Generate returns a random bootstrap token split into its id and secret halves.
func Generate() (id, secret string, err error) {
	token, err := tokenutil.GenerateBootstrapToken()
	if err != nil {
		return "", "", fmt.Errorf("generating bootstrap token: %w", err)
	}
	id, secret, ok := strings.Cut(token, ".")
	if !ok {
		return "", "", errors.New("malformed bootstrap token")
	}
	return id, secret, nil
}

// BootstrapSecret builds the Secret that makes the token authenticate.
func BootstrapSecret(id, secret string, expiresAt time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrapapi.BootstrapTokenSecretPrefix + id,
			Namespace: metav1.NamespaceSystem,
		},
		Type: corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			bootstrapapi.BootstrapTokenIDKey:               id,
			bootstrapapi.BootstrapTokenSecretKey:           secret,
			bootstrapapi.BootstrapTokenExpirationKey:       expiresAt.Format(time.RFC3339),
			bootstrapapi.BootstrapTokenUsageAuthentication: "true",
			bootstrapapi.BootstrapTokenDescriptionKey:      "Worker bootstrap token for a DPF-provisioned DPU",
		},
	}
}

// Encode renders the bootstrap kubeconfig and packs it the way k0s expects.
func Encode(apiServerURL string, caCert []byte, token string) (string, error) {
	kubeconfig, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{contextName: {
			Server:                   apiServerURL,
			CertificateAuthorityData: caCert,
		}},
		Contexts: map[string]*clientcmdapi.Context{contextName: {
			Cluster:  contextName,
			AuthInfo: workerTokenAuthName,
		}},
		CurrentContext: contextName,
		AuthInfos: map[string]*clientcmdapi.AuthInfo{workerTokenAuthName: {
			Token: token,
		}},
	})
	if err != nil {
		return "", fmt.Errorf("writing bootstrap kubeconfig: %w", err)
	}

	var out bytes.Buffer
	gz, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(gz, bytes.NewReader(kubeconfig)); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// Mint creates a worker bootstrap token in the target cluster and returns the encoded
// join token, which points at the API server the DPU can reach.
func Mint(ctx context.Context, c client.Client, apiServerURL string, caCert []byte, ttl time.Duration, now time.Time) (*Token, error) {
	if apiServerURL == "" {
		return nil, errors.New("api server URL is empty")
	}
	if len(caCert) == 0 {
		return nil, errors.New("cluster CA certificate is empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("token ttl must be positive, got %s", ttl)
	}

	id, secretPart, err := Generate()
	if err != nil {
		return nil, err
	}
	expiresAt := now.Add(ttl).UTC().Truncate(time.Second)

	if createErr := c.Create(ctx, BootstrapSecret(id, secretPart, expiresAt)); createErr != nil {
		return nil, fmt.Errorf("creating bootstrap token secret: %w", createErr)
	}

	encoded, err := Encode(apiServerURL, caCert, id+"."+secretPart)
	if err != nil {
		return nil, err
	}

	return &Token{ID: id, Encoded: encoded, ExpiresAt: expiresAt}, nil
}

// Revoke deletes the bootstrap token Secret of a minted token, for a token that never
// reached a DPU. An already absent Secret is not an error.
func Revoke(ctx context.Context, c client.Client, id string) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      bootstrapapi.BootstrapTokenSecretPrefix + id,
		Namespace: metav1.NamespaceSystem,
	}}

	if err := client.IgnoreNotFound(c.Delete(ctx, secret)); err != nil {
		return fmt.Errorf("deleting bootstrap token secret %s: %w", secret.Name, err)
	}

	return nil
}

// Decode reverses the encoding, for tests and for reading a rendered Secret.
func Decode(encoded string) (*clientcmdapi.Config, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64 decoding join token: %w", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("gunzipping join token: %w", err)
	}
	defer gz.Close()
	kubeconfig, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}
	return clientcmd.Load(kubeconfig)
}
