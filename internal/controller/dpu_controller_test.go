// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"code.local/k0s-dpu-bootstrapper/internal/controller"
	"code.local/k0s-dpu-bootstrapper/internal/dpf"
	"code.local/k0s-dpu-bootstrapper/internal/joinscript"
	"code.local/k0s-dpu-bootstrapper/internal/k0stoken"
	"code.local/k0s-dpu-bootstrapper/internal/tests"
)

const testScript = "#!/usr/bin/env bash\nk0s install worker # {{ .NodeName }} {{ .JoinToken }} {{ .APIServerURL }}"

func TestReconcileWritesJoinSecret(t *testing.T) {
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	res := tests.Reconcile(t, r, tests.DPUName)
	if res.RequeueAfter != 3*time.Hour {
		t.Errorf("RequeueAfter = %s, want 3h (ttl minus refresh window)", res.RequeueAfter)
	}

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret not written: %v", err)
	}
	script := string(secret.Data[dpf.JoinSecretKey])
	if !strings.Contains(script, "k0s install worker") || !strings.Contains(script, tests.DPUName) {
		t.Errorf("script was not rendered:\n%s", script)
	}
	if secret.Annotations[controller.ManagedByAnnotation] != controller.ManagedByValue {
		t.Errorf("managed by annotation = %q", secret.Annotations[controller.ManagedByAnnotation])
	}
	if secret.Annotations[controller.TokenExpiryAnnotation] != tests.Now.Add(4*time.Hour).Format(time.RFC3339) {
		t.Errorf("expiry annotation = %q", secret.Annotations[controller.TokenExpiryAnnotation])
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "DPU" {
		t.Errorf("owner references = %+v, want one DPU reference", secret.OwnerReferences)
	}

	// The bootstrap token belongs in the DPU cluster, not the host cluster.
	tokenSecret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: "kube-system", Name: "bootstrap-token-" + secret.Annotations[controller.TokenIDAnnotation]}
	if err := clusters.Client(t, tests.ClusterOne).Get(t.Context(), key, tokenSecret); err != nil {
		t.Errorf("bootstrap token %s not created in the DPU cluster: %v", key, err)
	}
}

func TestReconcileStoresATokenTheDPUCanJoinWith(t *testing.T) {
	// Everything above checks the script as text. This one takes the token out of it and
	// follows it back to the cluster, which is the only thing the DPU actually needs.
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))
	clusters.APIServerURL = tests.APIServerURL

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	// Taken out of the rendered script, so what is checked is what the DPU is handed and
	// not something the controller kept alongside it.
	fields := strings.Fields(string(secret.Data[dpf.JoinSecretKey]))
	encoded := fields[len(fields)-2]

	kubeconfig, err := k0stoken.Decode(encoded)
	if err != nil {
		t.Fatalf("the script does not carry a decodable join token: %v", err)
	}

	cluster, ok := kubeconfig.Clusters["k0s"]
	if !ok {
		t.Fatalf("the token names no cluster, got %v", kubeconfig.Clusters)
	}
	if cluster.Server != tests.APIServerURL {
		t.Errorf("the token points at %q, want the cluster this DPU belongs to", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != tests.FakeCA {
		t.Errorf("the token carries the wrong authority: %q", cluster.CertificateAuthorityData)
	}

	// Both halves, since a token whose secret half does not match the Secret in the DPU
	// cluster authenticates as nobody and the DPU never joins.
	auth, ok := kubeconfig.AuthInfos["kubelet-bootstrap"]
	if !ok {
		t.Fatalf("the token names no user, got %v", kubeconfig.AuthInfos)
	}
	id, secretHalf, _ := strings.Cut(auth.Token, ".")

	stored := &corev1.Secret{}
	key := types.NamespacedName{Namespace: "kube-system", Name: "bootstrap-token-" + id}
	if err := clusters.Client(t, tests.ClusterOne).Get(t.Context(), key, stored); err != nil {
		t.Fatalf("the token in the script names no bootstrap token in the DPU cluster: %v", err)
	}
	if held := tests.TokenSecretHalf(stored); held != secretHalf {
		t.Errorf("the two halves disagree, script has %q and the cluster has %q", secretHalf, held)
	}
	if secret.Annotations[controller.TokenIDAnnotation] != id {
		t.Errorf("the token id annotation is %q, but the script carries %q",
			secret.Annotations[controller.TokenIDAnnotation], id)
	}
}

func TestReconcileHandlesMultipleClusters(t *testing.T) {
	// Two DPUs in two clusters, each with its own template and control plane.
	second := tests.ClusterTwo
	r, hostClient, clusters := tests.NewReconciler(t,
		tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript),
		tests.DPU("dpu-2", second, false), tests.DPUCluster(second),
		tests.JoinScript("k0s-join-2", second, "echo second {{ .APIServerURL }}"),
	)

	tests.Reconcile(t, r, tests.DPUName)
	tests.Reconcile(t, r, "dpu-2")

	first, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret for dpu-1 missing: %v", err)
	}
	if !strings.Contains(string(first.Data[dpf.JoinSecretKey]), "dpu-cluster-1.example") {
		t.Errorf("dpu-1 was not pointed at its own control plane:\n%s", first.Data[dpf.JoinSecretKey])
	}

	other, err := tests.JoinSecret(t, hostClient, "dpu-2")
	if err != nil {
		t.Fatalf("join secret for dpu-2 missing: %v", err)
	}
	script := string(other.Data[dpf.JoinSecretKey])
	if !strings.Contains(script, "echo second") || !strings.Contains(script, "dpu-cluster-2.example") {
		t.Errorf("dpu-2 got the wrong template or control plane:\n%s", script)
	}

	// Each token must land in its own cluster and nowhere else.
	for ref, want := range map[dpf.ClusterRef]string{
		tests.ClusterOne: first.Annotations[controller.TokenIDAnnotation],
		second:           other.Annotations[controller.TokenIDAnnotation],
	} {
		tokens := &corev1.SecretList{}
		if err := clusters.Client(t, ref).List(t.Context(), tokens, client.InNamespace("kube-system")); err != nil {
			t.Fatalf("listing tokens in %s: %v", ref, err)
		}
		if len(tokens.Items) != 1 {
			t.Fatalf("cluster %s holds %d tokens, want 1", ref, len(tokens.Items))
		}
		if tokens.Items[0].Name != "bootstrap-token-"+want {
			t.Errorf("cluster %s holds %q, want the token of its own DPU", ref, tokens.Items[0].Name)
		}
	}
}

func TestReconcileOverwritesDPFsOwnSecret(t *testing.T) {
	// DPF creates this Secret itself with a kubeadm join command and never updates it
	// afterwards, so taking it over is safe.
	dpfSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: tests.Namespace, Name: dpf.JoinSecretName(tests.DPUName)},
		Data:       map[string][]byte{dpf.JoinSecretKey: []byte("kubeadm join 10.0.0.1:6443 --token abcdef")},
	}
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript), dpfSecret)

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if strings.Contains(string(secret.Data[dpf.JoinSecretKey]), "kubeadm join") {
		t.Error("kubeadm join command was not replaced")
	}
}

func TestReconcileReportsClusterOutsideNamespace(t *testing.T) {
	// DPF's allocator picks a DPUCluster from any namespace. Reaching one this controller
	// does not watch used to surface as a cache error that retried forever.
	elsewhere := dpf.ClusterRef{Name: "dpu-cluster-1", Namespace: "dpu-cplane-tenant1"}
	r, hostClient, _ := tests.NewReconciler(t,
		tests.DPU(tests.DPUName, elsewhere, false),
		tests.JoinScript("k0s-join", elsewhere, testScript),
	)

	tests.Reconcile(t, r, tests.DPUName)

	tests.ExpectEvents(t, r, "JoinClusterOutOfScope", tests.DPUName)
	if _, err := tests.JoinSecret(t, hostClient, tests.DPUName); !apierrors.IsNotFound(err) {
		t.Errorf("expected no join secret, got err = %v", err)
	}
}

func TestReconcileSkipsUnmanagedCluster(t *testing.T) {
	tmpl := tests.JoinScript("k0s-join", tests.ClusterOne, testScript)
	tmpl.Annotations[joinscript.ClusterNameAnnotation] = "someone-elses"
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tmpl)

	tests.Reconcile(t, r, tests.DPUName)

	if _, err := tests.JoinSecret(t, hostClient, tests.DPUName); !apierrors.IsNotFound(err) {
		t.Errorf("expected no join secret, got err = %v", err)
	}
}

func TestReconcileSkipsJoinedDPU(t *testing.T) {
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, true), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)

	if _, err := tests.JoinSecret(t, hostClient, tests.DPUName); !apierrors.IsNotFound(err) {
		t.Errorf("expected no join secret for an already joined DPU, got err = %v", err)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)
	first, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	res := tests.Reconcile(t, r, tests.DPUName)
	second, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	if first.Annotations[controller.TokenIDAnnotation] != second.Annotations[controller.TokenIDAnnotation] {
		t.Error("a second reconcile minted a new token even though the current one is still valid")
	}
	if res.RequeueAfter != 3*time.Hour {
		t.Errorf("RequeueAfter = %s, want 3h", res.RequeueAfter)
	}

	tokens := &corev1.SecretList{}
	if err := clusters.Client(t, tests.ClusterOne).List(t.Context(), tokens, client.InNamespace("kube-system")); err != nil {
		t.Fatalf("listing bootstrap tokens: %v", err)
	}
	if len(tokens.Items) != 1 {
		t.Errorf("bootstrap tokens in the DPU cluster = %d, want 1", len(tokens.Items))
	}
}

func TestReconcileDoesNotChurnAsTheClockMoves(t *testing.T) {
	// Nothing that goes into the fingerprint may come from the clock. The stand in expiry
	// is fixed for exactly this reason, and a real deadline there would re-mint every time.
	dated := "#!/usr/bin/env bash\nTOKEN_EXPIRES_AT='{{ .TokenExpiresAt }}'\nk0s install worker # {{ .JoinToken }}"
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, dated))

	tests.Reconcile(t, r, tests.DPUName)

	before, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	// Later, but still well inside the life of the token it already has.
	r.Clock = func() time.Time { return tests.Now.Add(time.Minute) }
	tests.Reconcile(t, r, tests.DPUName)

	after, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if after.Annotations[controller.TokenIDAnnotation] != before.Annotations[controller.TokenIDAnnotation] {
		t.Error("a minute passing was enough to mint a new token")
	}
	if tokens := tests.Tokens(t, clusters, tests.ClusterOne); len(tokens) != 1 {
		t.Errorf("tokens in the DPU cluster = %d, want one", len(tokens))
	}
}

func TestReconcileRefreshesExpiringToken(t *testing.T) {
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)
	first, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	// Inside the refresh window the DPU still has not run the script, so a fresh token
	// has to replace the one about to expire.
	r.Clock = func() time.Time { return tests.Now.Add(3*time.Hour + time.Minute) }
	tests.Reconcile(t, r, tests.DPUName)

	second, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if first.Annotations[controller.TokenIDAnnotation] == second.Annotations[controller.TokenIDAnnotation] {
		t.Error("token was not re-minted inside the refresh window")
	}
}

func TestReconcileRevokesTheTokenItReplaces(t *testing.T) {
	// A DPU that has not joined can be re-rendered many times over. Leaving each superseded
	// token to age out would pile up live join credentials for one machine.
	tmpl := tests.JoinScript("k0s-join", tests.ClusterOne, testScript)
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tmpl)

	tests.Reconcile(t, r, tests.DPUName)

	for i := range 3 {
		tmpl.Data[joinscript.ScriptKey] = testScript + "\n# edit " + strconv.Itoa(i)
		if err := hostClient.Update(t.Context(), tmpl); err != nil {
			t.Fatalf("editing the template: %v", err)
		}

		tests.Reconcile(t, r, tests.DPUName)
	}

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	tokens := tests.Tokens(t, clusters, tests.ClusterOne)
	if len(tokens) != 1 {
		t.Fatalf("four renders left %d tokens in the DPU cluster, want only the current one", len(tokens))
	}
	if want := "bootstrap-token-" + secret.Annotations[controller.TokenIDAnnotation]; tokens[0].Name != want {
		t.Errorf("the surviving token is %q, want %q", tokens[0].Name, want)
	}
}

func TestReconcileRevokesTheTokenOfADeletedDPU(t *testing.T) {
	// The Secret goes with the DPU through its owner reference. The token is in another
	// cluster, where nothing owns it and nothing would collect it before it aged out.
	dpuObj := tests.DPU(tests.DPUName, tests.ClusterOne, false)
	dpuObj.SetFinalizers([]string{"dpu.nvidia.com/dpu-protection"})
	r, hostClient, clusters := tests.NewReconciler(t, dpuObj, tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)

	if len(tests.Tokens(t, clusters, tests.ClusterOne)) != 1 {
		t.Fatal("no token was minted to begin with")
	}

	// DPF holds the DPU with its own finalizer, so the object is still there to be seen.
	if err := hostClient.Delete(t.Context(), dpuObj); err != nil {
		t.Fatalf("deleting the DPU: %v", err)
	}

	tests.Reconcile(t, r, tests.DPUName)

	tests.ExpectNoTokens(t, clusters, tests.ClusterOne)
}

func TestReconcileLeavesTokensNamedBySecretsItDoesNotOwn(t *testing.T) {
	// Revoking on deletion reads a token id out of a Secret. Acting on one this controller
	// did not write would take away a credential belonging to something else.
	dpuObj := tests.DPU(tests.DPUName, tests.ClusterOne, false)
	dpuObj.SetFinalizers([]string{"dpu.nvidia.com/dpu-protection"})
	r, hostClient, clusters := tests.NewReconciler(t, dpuObj, tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	secret.Annotations[controller.ManagedByAnnotation] = "someone-else"
	if err := hostClient.Update(t.Context(), secret); err != nil {
		t.Fatalf("disowning the secret: %v", err)
	}

	if err := hostClient.Delete(t.Context(), dpuObj); err != nil {
		t.Fatalf("deleting the DPU: %v", err)
	}

	tests.Reconcile(t, r, tests.DPUName)

	if tokens := tests.Tokens(t, clusters, tests.ClusterOne); len(tokens) != 1 {
		t.Errorf("tokens = %d, want the one named by a Secret this controller does not own", len(tokens))
	}
}

func TestReconcileMissingDPUIsNotAnError(t *testing.T) {
	r, _, _ := tests.NewReconciler(t, tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	if res := tests.Reconcile(t, r, tests.DPUName); res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want 0", res.RequeueAfter)
	}
}

func TestDPUsForTemplate(t *testing.T) {
	second := tests.ClusterTwo
	r, _, _ := tests.NewReconciler(t,
		tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript),
		tests.DPU("dpu-2", second, false), tests.DPUCluster(second),
	)

	requests := r.DPUsForTemplate(t.Context(), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))
	if len(requests) != 1 || requests[0].Name != tests.DPUName {
		t.Errorf("requests = %+v, want only %s", requests, tests.DPUName)
	}

	unrelated := tests.JoinScript("k0s-join", tests.ClusterOne, testScript)
	unrelated.Annotations[joinscript.ClusterNameAnnotation] = "someone-elses"
	if got := r.DPUsForTemplate(t.Context(), unrelated); len(got) != 0 {
		t.Errorf("requests for an unrelated cluster = %+v, want none", got)
	}
}

func TestDPUsForDPUCluster(t *testing.T) {
	second := tests.ClusterTwo
	r, _, _ := tests.NewReconciler(t,
		tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript),
		tests.DPU("dpu-2", second, false), tests.DPU("dpu-3", second, false), tests.DPUCluster(second),
	)

	got := r.DPUsForDPUCluster(t.Context(), tests.DPUCluster(second))
	if len(got) != 2 {
		t.Fatalf("requests = %+v, want the two DPUs of that cluster", got)
	}
	for _, req := range got {
		if req.Name == tests.DPUName {
			t.Errorf("a DPU from another cluster was enqueued: %+v", got)
		}
	}
}

func TestDPUForJoinSecret(t *testing.T) {
	r, _, _ := tests.NewReconciler(t,
		tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne),
	)

	secret := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name:      dpf.JoinSecretName(tests.DPUName),
		Namespace: tests.Namespace,
	}}
	got := r.DPUForJoinSecret(t.Context(), secret)
	if len(got) != 1 || got[0].Name != tests.DPUName || got[0].Namespace != tests.Namespace {
		t.Errorf("requests = %+v, want %s in %s", got, tests.DPUName, tests.Namespace)
	}

	// Every other Secret in the namespace reaches the same watch. The suffix has to be
	// anchored at the end, so carrying it anywhere else must not enqueue anything.
	for _, name := range []string{
		"dpu-cluster-1-admin-kubeconfig",
		"bootstrap-token-abcdef",
		dpf.JoinSecretSuffix,
		tests.DPUName,
		dpf.JoinSecretName(tests.DPUName) + ".bak",
		dpf.JoinSecretName(tests.DPUName) + "-old",
		tests.DPUName + "-kubeadm-joined",
	} {
		other := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: tests.Namespace,
		}}
		if reqs := r.DPUForJoinSecret(t.Context(), other); len(reqs) != 0 {
			t.Errorf("requests for Secret %q = %+v, want none", name, reqs)
		}
	}
}

// brokenScript loses the fi of its if, which no substitution can put back.
const brokenScript = "#!/usr/bin/env bash\nset -euo pipefail\nif [ ! -x /usr/local/bin/k0s ]; then\n  echo {{ .NodeName }}\n"

func TestProbeTokenLooksLikeARealOne(t *testing.T) {
	// The script is checked with this standing in for a token that does not exist yet. A
	// probe carrying a quote or a space would fail templates that a real token would not.
	if !k0stoken.ShellSafe(controller.ProbeToken) {
		t.Errorf("ProbeToken = %q, which a shell reads differently from base64", controller.ProbeToken)
	}
}

func TestReconcileRejectsInvalidScript(t *testing.T) {
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, brokenScript))

	_, err := r.Reconcile(t.Context(), tests.Request(tests.DPUName))
	if err == nil {
		t.Fatal("expected an error, got none")
	}

	// The message is all an operator gets, so it has to name the template revision and the
	// position, otherwise it says only that something somewhere is wrong.
	for _, want := range []string{"not valid bash", "k0s-join@", ":3:1:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}

	// Fail closed. A script that cannot run must not reach the DPU at all, since the agent
	// would retry it forever with the reason visible only on the DPU itself.
	if _, err := tests.JoinSecret(t, hostClient, tests.DPUName); !apierrors.IsNotFound(err) {
		t.Errorf("a join secret was written for a broken script, err = %v", err)
	}

	// The script is checked before a token is minted, so a template nobody has fixed yet
	// costs nothing however many times it is retried.
	tests.ExpectNoTokens(t, clusters, tests.ClusterOne)

	// Both the DPU and the template that broke it are told, the latter because that is the
	// object an operator has to edit.
	tests.ExpectEvents(t, r, "JoinScriptInvalid", tests.DPUName, "k0s-join")
	tests.ExpectEventMessage(t, r, "JoinScriptInvalid", joinscript.SkipValidationAnnotation)
}

func TestReconcileSkipsValidationWhenAnnotated(t *testing.T) {
	// The escape hatch, for a script the parser rejects but a DPU would run.
	tmpl := tests.WithoutScriptValidation(tests.JoinScript("k0s-join", tests.ClusterOne, brokenScript))
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tmpl)

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret not written: %v", err)
	}

	// Unchecked means untouched, so what the DPU is handed is exactly what was rendered.
	want, err := joinscript.Render(&joinscript.Template{Name: "k0s-join", Script: brokenScript}, &joinscript.Data{NodeName: tests.DPUName})
	if err != nil {
		t.Fatalf("rendering the expected script: %v", err)
	}
	if got := string(secret.Data[dpf.JoinSecretKey]); got != want {
		t.Errorf("the script was not stored as rendered:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestReconcileStoresScriptAsRendered(t *testing.T) {
	// Validation reads the script and nothing more. Reformatting it would rewrite backticks
	// and array subscripts, which changes what runs as root on the DPU.
	odd := "#!/usr/bin/env bash\n# {{ .NodeName }}\nif true;   then\nk0s install worker `hostname`\nfi\n"
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, odd))

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret not written: %v", err)
	}

	want, err := joinscript.Render(&joinscript.Template{Name: "k0s-join", Script: odd}, &joinscript.Data{NodeName: tests.DPUName})
	if err != nil {
		t.Fatalf("rendering the expected script: %v", err)
	}
	if got := string(secret.Data[dpf.JoinSecretKey]); got != want {
		t.Errorf("the stored script is not what was rendered:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestReconcileKeepsThePreviousSecretWhenTheTemplateBreaks(t *testing.T) {
	// A DPU that has not run its script yet is holding a working one. Breaking the template
	// must not take that away, or an edit made while a DPU is provisioning strands it.
	tmpl := tests.JoinScript("k0s-join", tests.ClusterOne, testScript)
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tmpl)

	tests.Reconcile(t, r, tests.DPUName)

	before, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	tmpl.Data[joinscript.ScriptKey] = brokenScript
	if err := hostClient.Update(t.Context(), tmpl); err != nil {
		t.Fatalf("updating the template: %v", err)
	}

	tests.ReconcileFails(t, r, tests.DPUName)

	after, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("the join secret was removed: %v", err)
	}
	if !bytes.Equal(after.Data[dpf.JoinSecretKey], before.Data[dpf.JoinSecretKey]) {
		t.Errorf("the stored script changed:\n%s", after.Data[dpf.JoinSecretKey])
	}
	if after.Annotations[controller.TokenIDAnnotation] != before.Annotations[controller.TokenIDAnnotation] {
		t.Error("the token the DPU was given was replaced by one it never receives")
	}

	// The one the DPU still holds, and not the one minted for the script that failed.
	tokens := tests.Tokens(t, clusters, tests.ClusterOne)
	if len(tokens) != 1 || tokens[0].Name != "bootstrap-token-"+before.Annotations[controller.TokenIDAnnotation] {
		t.Errorf("tokens in the DPU cluster = %d, want only the one already handed out", len(tokens))
	}
}

func TestReconcileMintsNoTokenWhenRenderFails(t *testing.T) {
	// The other deterministic template failure, which has to be caught in the same place.
	unset := "k0s install worker {{ .Values.nothingSetsThis }}"
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, unset))

	tests.ReconcileFails(t, r, tests.DPUName)

	if _, err := tests.JoinSecret(t, hostClient, tests.DPUName); !apierrors.IsNotFound(err) {
		t.Errorf("a join secret was written for a template that did not render, err = %v", err)
	}

	tests.ExpectNoTokens(t, clusters, tests.ClusterOne)
	tests.ExpectEvents(t, r, "JoinScriptRenderFailed", tests.DPUName, "k0s-join")
}

func TestReconcileReportsUnreachableCluster(t *testing.T) {
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))
	clusters.AccessErr = errors.New("no route to the control plane")

	if msg := tests.ReconcileFails(t, r, tests.DPUName); !strings.Contains(msg, "no route") {
		t.Errorf("error = %q, want the access failure", msg)
	}

	if _, err := tests.JoinSecret(t, hostClient, tests.DPUName); !apierrors.IsNotFound(err) {
		t.Errorf("a join secret was written without a reachable cluster, err = %v", err)
	}

	tests.ExpectEvents(t, r, "JoinTokenMintFailed", tests.DPUName)
}

func TestReconcileReportsRefusedToken(t *testing.T) {
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))
	clusters.RefuseTokenWrites(errors.New("forbidden"))

	tests.ReconcileFails(t, r, tests.DPUName)

	if _, err := tests.JoinSecret(t, hostClient, tests.DPUName); !apierrors.IsNotFound(err) {
		t.Errorf("a join secret was written around a token that was never minted, err = %v", err)
	}

	tests.ExpectEvents(t, r, "JoinTokenMintFailed", tests.DPUName)
	tests.ExpectEventMessage(t, r, "JoinTokenMintFailed", tests.DPUName)
}

func TestReconcileNeedsNoRevokeForABrokenTemplate(t *testing.T) {
	// Revocation is best effort, and a cluster that refuses the delete used to leave a live
	// join credential behind on every retry. Nothing is minted now, so nothing is at risk.
	r, _, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, brokenScript))
	clusters.RefuseTokenDeletes(errors.New("forbidden"))

	for range 3 {
		if msg := tests.ReconcileFails(t, r, tests.DPUName); !strings.Contains(msg, "not valid bash") {
			t.Errorf("error = %q, want the parse failure", msg)
		}
	}

	tests.ExpectNoTokens(t, clusters, tests.ClusterOne)
}

func TestReconcileDistinguishesTemplateFailures(t *testing.T) {
	// Two templates of equal specificity is an operator mistake with one fix. A missing
	// script key is a different mistake, and reporting both the same way hides that.
	ambiguous, _, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne),
		tests.JoinScript("first", tests.ClusterOne, testScript), tests.JoinScript("second", tests.ClusterOne, testScript))

	tests.ReconcileFails(t, ambiguous, tests.DPUName)
	tests.ExpectEvents(t, ambiguous, "JoinScriptTemplateAmbiguous", tests.DPUName)

	empty := tests.JoinScriptConfigMap("k0s-join", tests.ClusterOne, map[string]string{"wrong-key": testScript})
	unresolved, _, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), empty)

	tests.ReconcileFails(t, unresolved, tests.DPUName)
	tests.ExpectEvents(t, unresolved, "JoinScriptTemplateUnresolved", tests.DPUName)
}

func TestReconcileRepairsAStoredScriptThatDoesNotParse(t *testing.T) {
	// What an upgrade lands on, a Secret carrying neither hash, which is how one written
	// before they existed is told apart from one of ours.
	tmpl := tests.JoinScript("k0s-join", tests.ClusterOne, testScript)
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tmpl)

	stale := tests.PreviousJoinSecret(tests.DPUName, tests.TemplateRef(t, hostClient, "k0s-join"),
		"if [ -x /usr/local/bin/k0s ]; then\n  k0s start\n", tests.Now.Add(tests.TokenTTL))
	if err := hostClient.Create(t.Context(), stale); err != nil {
		t.Fatalf("seeding the stale secret: %v", err)
	}

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if bytes.Equal(secret.Data[dpf.JoinSecretKey], stale.Data[dpf.JoinSecretKey]) {
		t.Errorf("the unparseable script was left in place:\n%s", secret.Data[dpf.JoinSecretKey])
	}
	if err := joinscript.Validate(string(secret.Data[dpf.JoinSecretKey]), "stored"); err != nil {
		t.Errorf("what replaced it does not parse either: %v", err)
	}

	// Rewritten means re-minted, and the token the old Secret named was never ours.
	if secret.Annotations[controller.TokenIDAnnotation] == stale.Annotations[controller.TokenIDAnnotation] {
		t.Error("the script was replaced but the token annotation was not")
	}
	if tokens := tests.Tokens(t, clusters, tests.ClusterOne); len(tokens) != 1 {
		t.Errorf("tokens in the DPU cluster = %d, want the one just minted", len(tokens))
	}
}

func TestReconcileRewritesAScriptBuiltFromAMovedControlPlane(t *testing.T) {
	// The API server address comes from the DPUCluster kubeconfig, not the template, so
	// nothing about the template changes when a control plane moves.
	script := "#!/usr/bin/env bash\nk0s install worker --server {{ .APIServerURL }} # {{ .JoinToken }}"
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, script))
	clusters.APIServerURL = "https://old.example:6443"

	tests.Reconcile(t, r, tests.DPUName)

	clusters.APIServerURL = "https://new.example:6443"
	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if stored := string(secret.Data[dpf.JoinSecretKey]); !strings.Contains(stored, "https://new.example:6443") {
		t.Errorf("the DPU is still being sent to the old address:\n%s", stored)
	}
}

func TestReconcileRepairsASecretEditedBehindItsBack(t *testing.T) {
	// The fingerprint covers what the script was built from, not what the Secret holds, so
	// an edit to the Secret itself is only caught by reading what is in it.
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	secret.Data[dpf.JoinSecretKey] = []byte("if [ -x /usr/local/bin/k0s ]; then\n  k0s start\n")
	if err := hostClient.Update(t.Context(), secret); err != nil {
		t.Fatalf("editing the secret: %v", err)
	}

	tests.Reconcile(t, r, tests.DPUName)

	after, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if err := joinscript.Validate(string(after.Data[dpf.JoinSecretKey]), "stored"); err != nil {
		t.Errorf("the edit was left in place: %v", err)
	}
}

func TestReconcileIgnoresATemplateEditThatChangesNothing(t *testing.T) {
	// A label on a template bumps its revision without changing a byte of what it renders.
	// Re-minting every DPU in the cluster for that would be churn and nothing else.
	tmpl := tests.JoinScript("k0s-join", tests.ClusterOne, testScript)
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tmpl)

	tests.Reconcile(t, r, tests.DPUName)

	before, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	tmpl.Labels["team"] = "networking"
	if err := hostClient.Update(t.Context(), tmpl); err != nil {
		t.Fatalf("adding a label to the template: %v", err)
	}

	tests.Reconcile(t, r, tests.DPUName)

	after, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if after.Annotations[controller.TokenIDAnnotation] != before.Annotations[controller.TokenIDAnnotation] {
		t.Error("a template edit that changed nothing minted a new token")
	}
	if tokens := tests.Tokens(t, clusters, tests.ClusterOne); len(tokens) != 1 {
		t.Errorf("tokens in the DPU cluster = %d, want one", len(tokens))
	}
}

func TestReconcilePassesValuesAndIdentityToTheTemplate(t *testing.T) {
	// Every field a template can reference, through the reconciler rather than a hand built
	// Data. Each is tagged, since the fixtures share a namespace and substrings would hide.
	script := "#!/usr/bin/env bash\n" +
		"# dpu={{ .DPUName }} dpuns={{ .DPUNamespace }}" +
		" cluster={{ .ClusterName }} clusterns={{ .ClusterNamespace }} node={{ .NodeName }}\n" +
		"k0s install worker --cri-socket {{ .Values.criSocket }} {{ .Values.extraArgs }}\n"
	cm := tests.JoinScriptConfigMap("k0s-join", tests.ClusterOne, map[string]string{
		joinscript.ScriptKey: script,
		"criSocket":          "remote:unix:///run/containerd/containerd.sock",
		"extraArgs":          "--labels dpu=true",
	})
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), cm)

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}

	stored := string(secret.Data[dpf.JoinSecretKey])
	for _, want := range []string{
		"dpu=" + tests.DPUName, "dpuns=" + tests.Namespace,
		"cluster=" + tests.ClusterOne.Name, "clusterns=" + tests.ClusterOne.Namespace,
		"node=" + dpf.NodeName(tests.DPUName),
		"remote:unix:///run/containerd/containerd.sock", "--labels dpu=true",
	} {
		if !strings.Contains(stored, want) {
			t.Errorf("the rendered script is missing %q:\n%s", want, stored)
		}
	}
}

func TestReconcileRewritesAnEmptiedSecret(t *testing.T) {
	// A Secret whose script was removed but whose annotations were left behind. Everything
	// else about it still says current, and the DPU would run nothing at all.
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript))

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	delete(secret.Data, dpf.JoinSecretKey)
	if err := hostClient.Update(t.Context(), secret); err != nil {
		t.Fatalf("emptying the secret: %v", err)
	}

	tests.Reconcile(t, r, tests.DPUName)

	after, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if !strings.Contains(string(after.Data[dpf.JoinSecretKey]), "k0s install worker") {
		t.Errorf("the emptied script was not replaced:\n%q", after.Data[dpf.JoinSecretKey])
	}
}

func TestReconcileDoesNotChurnAnOptedOutTemplate(t *testing.T) {
	// A template that opted out is allowed to hold a script the parser rejects. Checking
	// the stored one anyway would call it stale forever, minting on every reconcile.
	tmpl := tests.WithoutScriptValidation(tests.JoinScript("k0s-join", tests.ClusterOne, brokenScript))
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tmpl)

	minted := map[string]bool{}

	for range 5 {
		tests.Reconcile(t, r, tests.DPUName)

		secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
		if err != nil {
			t.Fatalf("join secret missing: %v", err)
		}

		minted[secret.Annotations[controller.TokenIDAnnotation]] = true
	}

	if len(minted) != 1 {
		t.Errorf("five reconciles minted %d tokens, want one", len(minted))
	}
	if tokens := tests.Tokens(t, clusters, tests.ClusterOne); len(tokens) != 1 {
		t.Errorf("tokens in the DPU cluster = %d, want one", len(tokens))
	}
}

func TestReconcilePrefersFlavorScopedTemplate(t *testing.T) {
	// Two DPUs of one cluster can differ by flavor, each rendering its own script.
	narrowed := tests.JoinScript("k0s-join-flavor", tests.ClusterOne, "echo flavored {{ .NodeName }}")
	narrowed.Annotations[joinscript.FlavorAnnotation] = tests.Flavor
	r, hostClient, _ := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tests.JoinScript("k0s-join", tests.ClusterOne, testScript), narrowed)

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if !strings.Contains(string(secret.Data[dpf.JoinSecretKey]), "echo flavored") {
		t.Errorf("cluster wide template was used instead of the flavor scoped one:\n%s", secret.Data[dpf.JoinSecretKey])
	}
	if !strings.HasPrefix(secret.Annotations[controller.TemplateAnnotation], "k0s-join-flavor@") {
		t.Errorf("template annotation = %q", secret.Annotations[controller.TemplateAnnotation])
	}
}

func TestReconcileToleratesAFailedRevoke(t *testing.T) {
	// Taking a token back is best effort. A DPU cluster that refuses the delete must not
	// stop the DPU being handed the script it is waiting for.
	tmpl := tests.JoinScript("k0s-join", tests.ClusterOne, testScript)
	r, hostClient, clusters := tests.NewReconciler(t, tests.DPU(tests.DPUName, tests.ClusterOne, false), tests.DPUCluster(tests.ClusterOne), tmpl)
	clusters.RefuseTokenDeletes(errors.New("forbidden"))

	tests.Reconcile(t, r, tests.DPUName)

	tmpl.Data[joinscript.ScriptKey] = testScript + "\n# edited"
	if err := hostClient.Update(t.Context(), tmpl); err != nil {
		t.Fatalf("editing the template: %v", err)
	}

	tests.Reconcile(t, r, tests.DPUName)

	secret, err := tests.JoinSecret(t, hostClient, tests.DPUName)
	if err != nil {
		t.Fatalf("join secret missing: %v", err)
	}
	if !strings.Contains(string(secret.Data[dpf.JoinSecretKey]), "# edited") {
		t.Error("the rewrite was abandoned because the old token could not be taken back")
	}

	// And the one it could not take back is still there, which is the cost of that.
	if tokens := tests.Tokens(t, clusters, tests.ClusterOne); len(tokens) != 2 {
		t.Errorf("tokens = %d, want the new one and the one that could not be deleted", len(tokens))
	}
}
