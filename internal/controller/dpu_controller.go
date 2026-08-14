// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

// Package controller reconciles DPF DPU objects into k0s join Secrets.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"code.local/k0s-dpu-bootstrapper/internal/clusteraccess"
	"code.local/k0s-dpu-bootstrapper/internal/dpf"
	"code.local/k0s-dpu-bootstrapper/internal/joinscript"
	"code.local/k0s-dpu-bootstrapper/internal/k0stoken"
)

const (
	// ManagedByAnnotation marks a join Secret as rendered by this controller.
	ManagedByAnnotation = "k0s.mirantis.com/managed-by"
	// ManagedByValue is the value of ManagedByAnnotation.
	ManagedByValue = "k0s-dpu-bootstrapper"
	// TemplateAnnotation records the template revision the script was rendered from.
	TemplateAnnotation = "k0s.mirantis.com/join-script-template"
	// TokenExpiryAnnotation records when the embedded join token stops working.
	TokenExpiryAnnotation = "k0s.mirantis.com/token-expires-at"
	// TokenIDAnnotation records the token id, which names the Secret in the DPU cluster.
	TokenIDAnnotation = "k0s.mirantis.com/token-id"
	// InputHashAnnotation fingerprints the script as rendered with a stand in token, so a
	// change to anything the template puts in it is visible.
	InputHashAnnotation = "k0s.mirantis.com/input-hash"
	// ScriptHashAnnotation fingerprints the script that was stored, so an edit made to the
	// Secret by anyone else is visible.
	ScriptHashAnnotation = "k0s.mirantis.com/script-hash"

	reasonRendered           = "JoinScriptRendered"
	reasonTemplateAmbiguous  = "JoinScriptTemplateAmbiguous"
	reasonTemplateUnresolved = "JoinScriptTemplateUnresolved"
	reasonRenderFailed       = "JoinScriptRenderFailed"
	reasonScriptInvalid      = "JoinScriptInvalid"
	reasonMintFailed         = "JoinTokenMintFailed"
)

// DPUReconciler writes a k0s join script into the Secret the DPU agent executes.
type DPUReconciler struct {
	client.Client
	Recorder          record.EventRecorder
	NewClusterAccess  clusteraccess.Func
	Clock             func() time.Time
	TemplateNamespace string
	TokenTTL          time.Duration
	RefreshBefore     time.Duration
}

// Now reports the current time through the reconciler clock.
func (r *DPUReconciler) Now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// RecordFailure reports a failure on every object it concerns, always the DPU and for a bad
// template its ConfigMap too, and returns the error unchanged.
func (r *DPUReconciler) RecordFailure(reason string, err error, objects ...client.Object) error {
	for _, obj := range objects {
		r.Recorder.Event(obj, corev1.EventTypeWarning, reason, err.Error())
	}

	return err
}

// UpsertOwnerReference adds a reference unless one with the same UID is present, keeping
// the ownership flags that reference already carries.
func UpsertOwnerReference(refs []metav1.OwnerReference, ref *metav1.OwnerReference) []metav1.OwnerReference {
	if i := slices.IndexFunc(refs, func(existing metav1.OwnerReference) bool {
		return existing.UID == ref.UID
	}); i >= 0 {
		merged := *ref
		merged.Controller = refs[i].Controller
		merged.BlockOwnerDeletion = refs[i].BlockOwnerDeletion
		refs[i] = merged

		return refs
	}

	return append(refs, *ref)
}

func (r *DPUReconciler) WriteSecret(
	ctx context.Context,
	dpuObj *unstructured.Unstructured,
	key types.NamespacedName,
	tmpl *joinscript.Template,
	token *k0stoken.Token,
	script, inputHash string,
) (string, error) {
	var superseded string

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		// Read before it is overwritten, since this is the only record of the token the DPU
		// was previously offered.
		superseded = secret.Annotations[TokenIDAnnotation]

		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[ManagedByAnnotation] = ManagedByValue
		secret.Annotations[TemplateAnnotation] = tmpl.Ref()
		secret.Annotations[TokenExpiryAnnotation] = token.ExpiresAt.Format(time.RFC3339)
		secret.Annotations[TokenIDAnnotation] = token.ID
		secret.Annotations[InputHashAnnotation] = inputHash
		secret.Annotations[ScriptHashAnnotation] = joinscript.Hash(script)

		ownerRef := dpf.OwnerReference(dpuObj)
		secret.OwnerReferences = UpsertOwnerReference(secret.OwnerReferences, &ownerRef)

		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[dpf.JoinSecretKey] = []byte(script)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("writing join secret %s: %w", key, err)
	}

	return superseded, nil
}

// UpToDate reports whether a Secret still carries a usable script, and for how long.
func (r *DPUReconciler) UpToDate(
	secret *corev1.Secret, tmpl *joinscript.Template, inputHash string,
) (time.Duration, bool) {
	if secret.Annotations[ManagedByAnnotation] != ManagedByValue {
		return 0, false
	}

	// Everything the script says, other than the token, reduced to one string. A Secret
	// from a build that wrote no hash has none of this and is rewritten.
	if secret.Annotations[InputHashAnnotation] != inputHash {
		return 0, false
	}
	// What the Secret holds, rather than what it was built from. Anything written by anyone
	// else, emptied, or left by a build that stamped no hash, is not ours and is replaced.
	if secret.Annotations[ScriptHashAnnotation] != joinscript.Hash(string(secret.Data[dpf.JoinSecretKey])) {
		return 0, false
	}
	expiresAt, err := time.Parse(time.RFC3339, secret.Annotations[TokenExpiryAnnotation])
	if err != nil {
		return 0, false
	}
	renewIn := expiresAt.Sub(r.Now()) - r.RefreshBefore
	if renewIn <= 0 {
		return 0, false
	}
	return renewIn, true
}

// CurrentScript reports whether the join Secret holds a usable script, and for how long.
func (r *DPUReconciler) CurrentScript(
	ctx context.Context, key types.NamespacedName, tmpl *joinscript.Template, inputHash string,
) (time.Duration, bool, error) {
	existing := &corev1.Secret{}

	switch err := r.Get(ctx, key, existing); {
	case err == nil:
		renewIn, ok := r.UpToDate(existing, tmpl, inputHash)

		return renewIn, ok, nil
	case apierrors.IsNotFound(err):
		// Absent, so it gets created. A DPF create that lands second is tolerated.
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("getting join secret %s: %w", key, err)
	}
}

// RevokeToken drops a bootstrap token that nothing will use, best effort. A token whose
// write may have landed is never revoked, since the DPU could already be acting on it.
func RevokeToken(ctx context.Context, c client.Client, tokenID string) {
	if err := k0stoken.Revoke(ctx, c, tokenID); err != nil {
		ctrllog.FromContext(ctx).Error(err, "leaving behind a bootstrap token nothing will use",
			"tokenID", tokenID)
	}
}

// RevokeForDeletedDPU takes back the join token of a DPU on its way out, best effort. The
// Secret goes with the DPU, but the token it names lives in a cluster nothing here collects.
func (r *DPUReconciler) RevokeForDeletedDPU(ctx context.Context, dpuObj *unstructured.Unstructured, dpu *dpf.DPU) {
	log := ctrllog.FromContext(ctx)

	if dpu.Spec.Cluster.IsZero() {
		return
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: dpuObj.GetNamespace(), Name: dpf.JoinSecretName(dpuObj.GetName())}

	if err := r.Get(ctx, key, secret); err != nil {
		return
	}

	tokenID := secret.Annotations[TokenIDAnnotation]
	if secret.Annotations[ManagedByAnnotation] != ManagedByValue || tokenID == "" {
		return
	}

	clusterObj := dpf.NewDPUCluster()
	if err := r.Get(ctx, dpu.Spec.Cluster.ObjectKey(), clusterObj); err != nil {
		log.V(1).Info("leaving a token behind, the DPUCluster is already gone",
			"tokenID", tokenID, "cluster", dpu.Spec.Cluster.String())

		return
	}

	access, err := r.NewClusterAccess(ctx, r.Client, clusterObj)
	if err != nil {
		log.V(1).Info("leaving a token behind, the DPU cluster cannot be reached",
			"tokenID", tokenID, "cluster", dpu.Spec.Cluster.String())

		return
	}

	RevokeToken(ctx, access.Client, tokenID)
}

const (
	// ProbeTokenSize is how long the stand in token is. A real one runs to a few kilobytes,
	// and a shorter stand in would let a script pass the size check and then outgrow it.
	ProbeTokenSize = 4096
	// ProbeExpiry stands in for the expiry of a token that does not exist yet. Fixed rather
	// than the real deadline, or the same inputs would fingerprint differently every time.
	ProbeExpiry = "1970-01-01T00:00:00Z"
)

// ProbeToken stands in for a real token while the script is checked. Base64 like the one it
// replaces, so a script that parses with this parses with that.
var ProbeToken = strings.Repeat("Y2hlY2tpbmcgdGhlIGpvaW4gc2NyaXB0", ProbeTokenSize/32)

// Plan is everything a join Secret is built from, settled before a token exists for it.
type Plan struct {
	Access    *clusteraccess.Access
	Data      joinscript.Data
	InputHash string
}

// RenderScript renders the join script and rejects one that is not valid bash. Both are
// reported on the DPU and on the template, since the template is what has to be edited.
func (r *DPUReconciler) RenderScript(
	dpuObj *unstructured.Unstructured, tmpl *joinscript.Template, data *joinscript.Data,
) (string, error) {
	script, err := joinscript.Render(tmpl, data)
	if err != nil {
		return "", r.RecordFailure(reasonRenderFailed, err, dpuObj, tmpl.ConfigMapRef())
	}

	if tmpl.SkipValidation {
		return script, nil
	}

	if err := joinscript.Validate(script, tmpl.Ref()); err != nil {
		// The parser is stricter than bash in a few corners, so the way out is named here
		// rather than left for the operator to find in the README.
		return "", r.RecordFailure(reasonScriptInvalid,
			fmt.Errorf("%w (annotate the template %s=true to store it unchecked)",
				err, joinscript.SkipValidationAnnotation),
			dpuObj, tmpl.ConfigMapRef())
	}

	return script, nil
}

// PlanJoinSecret settles what the script has to say and checks that it is valid bash, before
// anything has been created for it. The token is the one input missing, and cannot matter.
func (r *DPUReconciler) PlanJoinSecret(
	ctx context.Context, dpuObj *unstructured.Unstructured, dpu *dpf.DPU, tmpl *joinscript.Template,
) (*Plan, error) {
	clusterObj := dpf.NewDPUCluster()
	if err := r.Get(ctx, dpu.Spec.Cluster.ObjectKey(), clusterObj); err != nil {
		return nil, fmt.Errorf("getting DPUCluster %s: %w", dpu.Spec.Cluster, err)
	}

	// Read every time, since the API server address lives here rather than in the template,
	// and a control plane that moves changes nothing the template revision would show.
	access, err := r.NewClusterAccess(ctx, r.Client, clusterObj)
	if err != nil {
		return nil, r.RecordFailure(reasonMintFailed, err, dpuObj)
	}

	plan := &Plan{Access: access, Data: joinscript.Data{
		JoinToken:        ProbeToken,
		TokenExpiresAt:   ProbeExpiry,
		APIServerURL:     access.APIServerURL,
		NodeName:         dpf.NodeName(dpuObj.GetName()),
		DPUName:          dpuObj.GetName(),
		DPUNamespace:     dpuObj.GetNamespace(),
		ClusterName:      dpu.Spec.Cluster.Name,
		ClusterNamespace: dpu.Spec.Cluster.Namespace,
		Values:           tmpl.Values,
	}}

	// A broken template fails the same way on every retry, so checking it here is what keeps
	// a permanent failure from minting a credential every time it is retried.
	probe, err := r.RenderScript(dpuObj, tmpl, &plan.Data)
	if err != nil {
		return nil, err
	}

	plan.InputHash = joinscript.Hash(probe)

	return plan, nil
}

// WriteJoinSecret mints a token, renders the script with it and stores the result.
func (r *DPUReconciler) WriteJoinSecret(
	ctx context.Context,
	dpuObj *unstructured.Unstructured,
	tmpl *joinscript.Template,
	plan *Plan,
	secretKey types.NamespacedName,
) error {
	access := plan.Access

	token, err := k0stoken.Mint(ctx, access.Client, access.APIServerURL, access.CACert, r.TokenTTL, r.Now())
	if err != nil {
		return r.RecordFailure(reasonMintFailed,
			fmt.Errorf("minting join token for DPU %s/%s: %w", dpuObj.GetNamespace(), dpuObj.GetName(), err), dpuObj)
	}

	plan.Data.JoinToken = token.Encoded
	plan.Data.TokenExpiresAt = token.ExpiresAt.Format(time.RFC3339)

	// The same template and the same values, differing only in a token Mint has already
	// checked is base64, so reaching this is a bug rather than a template someone wrote.
	script, err := r.RenderScript(dpuObj, tmpl, &plan.Data)
	if err != nil {
		RevokeToken(ctx, access.Client, token.ID)

		return err
	}

	superseded, err := r.WriteSecret(ctx, dpuObj, secretKey, tmpl, token, script, plan.InputHash)
	if err != nil {
		return err
	}

	// Only once the replacement is the one on offer. A DPU that had read the old one retries
	// the whole script every 30s, so it picks up the new token on its next pass.
	if superseded != "" && superseded != token.ID {
		RevokeToken(ctx, access.Client, superseded)
	}

	expiry := token.ExpiresAt.Format(time.RFC3339)
	r.Recorder.Eventf(dpuObj, corev1.EventTypeNormal, reasonRendered,
		"Wrote k0s join script to Secret %s from template %s (token expires %s)",
		secretKey.Name, tmpl.Ref(), expiry)
	ctrllog.FromContext(ctx).Info("rendered join script",
		"secret", secretKey.String(), "template", tmpl.Ref(), "tokenExpiresAt", expiry)

	return nil
}

// Reconcile renders the join Secret for one DPU.
func (r *DPUReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	dpuObj := dpf.NewDPU()
	if err := r.Get(ctx, req.NamespacedName, dpuObj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	dpu, err := dpf.ProjectDPU(dpuObj)
	if err != nil {
		return ctrl.Result{}, err
	}

	if dpuObj.GetDeletionTimestamp() != nil {
		// The Secret is owned by the DPU and goes with it. Its token is not owned by
		// anything, and lives in another cluster.
		r.RevokeForDeletedDPU(ctx, dpuObj, dpu)

		return ctrl.Result{}, nil
	}

	// Once the kubelet is configured the agent never reads the Secret again.
	if dpu.KubeletConfigured() {
		log.V(1).Info("DPU already joined, nothing to render")
		return ctrl.Result{}, nil
	}
	if dpu.Spec.Cluster.IsZero() {
		log.V(1).Info("DPU is not assigned to a DPUCluster yet")
		return ctrl.Result{}, nil
	}

	tmpl, err := joinscript.Resolve(ctx, r.Client, r.TemplateNamespace, joinscript.Target{
		Cluster: dpu.Spec.Cluster,
		Flavor:  dpu.Spec.DPUFlavor,
	})
	if err != nil {
		// Only one of the failures here is a duplicate template. A missing script key or a
		// failed list must not be reported as one.
		reason := reasonTemplateUnresolved
		if errors.Is(err, joinscript.ErrAmbiguous) {
			reason = reasonTemplateAmbiguous
		}

		return ctrl.Result{}, r.RecordFailure(reason, err, dpuObj)
	}
	if tmpl == nil {
		// Not a cluster this controller manages.
		log.V(1).Info("no join script template for DPUCluster, skipping", "cluster", dpu.Spec.Cluster.String())
		return ctrl.Result{}, nil
	}

	secretKey := types.NamespacedName{
		Namespace: dpuObj.GetNamespace(),
		Name:      dpf.JoinSecretName(dpuObj.GetName()),
	}

	// Worked out before the Secret is looked at, since what it should say is what decides
	// whether what is there will do.
	plan, err := r.PlanJoinSecret(ctx, dpuObj, dpu, tmpl)
	if err != nil {
		return ctrl.Result{}, err
	}

	renewIn, current, err := r.CurrentScript(ctx, secretKey, tmpl, plan.InputHash)
	if err != nil {
		return ctrl.Result{}, err
	}
	if current {
		log.V(1).Info("join script is current", "renewIn", renewIn.Round(time.Second))

		return ctrl.Result{RequeueAfter: renewIn}, nil
	}

	if err := r.WriteJoinSecret(ctx, dpuObj, tmpl, plan, secretKey); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.TokenTTL - r.RefreshBefore}, nil
}

// DPUsForCluster lists the DPUs of one cluster through the field index.
func (r *DPUReconciler) DPUsForCluster(ctx context.Context, cluster dpf.ClusterRef) []reconcile.Request {
	if cluster.IsZero() {
		return nil
	}
	list := dpf.NewDPUList()
	if err := r.List(ctx, list, client.MatchingFields{dpf.ClusterIndexKey: cluster.String()}); err != nil {
		ctrllog.FromContext(ctx).Error(err, "listing DPUs of cluster", "cluster", cluster.String())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].GetNamespace(),
			Name:      list.Items[i].GetName(),
		}})
	}
	return requests
}

// DPUsForTemplate enqueues every DPU whose cluster a changed template is scoped to.
func (r *DPUReconciler) DPUsForTemplate(ctx context.Context, obj client.Object) []reconcile.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || cm.Labels[joinscript.TemplateLabel] != joinscript.TemplateLabelValue {
		return nil
	}
	return r.DPUsForCluster(ctx, joinscript.ClusterRefFromAnnotations(cm.Annotations))
}

// DPUsForDPUCluster enqueues a cluster's DPUs when the cluster itself changes.
func (r *DPUReconciler) DPUsForDPUCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.DPUsForCluster(ctx, dpf.ClusterRef{Name: obj.GetName(), Namespace: obj.GetNamespace()})
}

// SetupWithManager registers the controller. Secrets are neither cached nor watched.
func (r *DPUReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if r.NewClusterAccess == nil {
		r.NewClusterAccess = clusteraccess.NewCache().Get
	}
	if r.Recorder == nil {
		// Every failure path records an Event, so a missing recorder would panic there.
		r.Recorder = mgr.GetEventRecorderFor("k0s-dpu-bootstrapper")
	}
	if err := dpf.IndexDPUByCluster(ctx, mgr.GetFieldIndexer()); err != nil {
		return fmt.Errorf("indexing DPUs by cluster: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("dpu-join-secret").
		For(dpf.NewDPU()).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.DPUsForTemplate)).
		Watches(dpf.NewDPUCluster(), handler.EnqueueRequestsFromMapFunc(r.DPUsForDPUCluster)).
		Complete(r)
}
