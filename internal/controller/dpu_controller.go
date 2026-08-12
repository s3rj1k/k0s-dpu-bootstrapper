// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

// Package controller reconciles DPF DPU objects into k0s join Secrets.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

	reasonRendered           = "JoinScriptRendered"
	reasonTemplateAmbiguous  = "JoinScriptTemplateAmbiguous"
	reasonTemplateUnresolved = "JoinScriptTemplateUnresolved"
	reasonRenderFailed       = "JoinScriptRenderFailed"
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

// RecordFailure reports a failure on the DPU and returns the error unchanged.
func (r *DPUReconciler) RecordFailure(obj *unstructured.Unstructured, reason string, err error) error {
	r.Recorder.Event(obj, corev1.EventTypeWarning, reason, err.Error())

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
	script string,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[ManagedByAnnotation] = ManagedByValue
		secret.Annotations[TemplateAnnotation] = tmpl.Ref()
		secret.Annotations[TokenExpiryAnnotation] = token.ExpiresAt.Format(time.RFC3339)
		secret.Annotations[TokenIDAnnotation] = token.ID

		ownerRef := dpf.OwnerReference(dpuObj)
		secret.OwnerReferences = UpsertOwnerReference(secret.OwnerReferences, &ownerRef)

		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[dpf.JoinSecretKey] = []byte(script)
		return nil
	})
	if err != nil {
		return fmt.Errorf("writing join secret %s: %w", key, err)
	}
	return nil
}

// UpToDate reports whether a Secret still carries a usable script, and for how long.
func (r *DPUReconciler) UpToDate(secret *corev1.Secret, tmpl *joinscript.Template) (time.Duration, bool) {
	if secret.Annotations[ManagedByAnnotation] != ManagedByValue {
		return 0, false
	}
	if secret.Annotations[TemplateAnnotation] != tmpl.Ref() {
		return 0, false
	}
	if len(secret.Data[dpf.JoinSecretKey]) == 0 {
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
	ctx context.Context, key types.NamespacedName, tmpl *joinscript.Template,
) (time.Duration, bool, error) {
	existing := &corev1.Secret{}

	switch err := r.Get(ctx, key, existing); {
	case err == nil:
		renewIn, ok := r.UpToDate(existing, tmpl)

		return renewIn, ok, nil
	case apierrors.IsNotFound(err):
		// Absent, so it gets created. A DPF create that lands second is tolerated.
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("getting join secret %s: %w", key, err)
	}
}

// RevokeToken drops a bootstrap token that was minted but never handed out, best effort.
// A write that may have landed is never revoked, since the DPU could already hold it.
func RevokeToken(ctx context.Context, c client.Client, token *k0stoken.Token) {
	if err := k0stoken.Revoke(ctx, c, token.ID); err != nil {
		ctrllog.FromContext(ctx).Error(err, "leaving behind a bootstrap token that was never handed out",
			"tokenID", token.ID)
	}
}

// RenderJoinSecret mints a token, renders the script and writes the Secret.
func (r *DPUReconciler) RenderJoinSecret(
	ctx context.Context,
	dpuObj *unstructured.Unstructured,
	dpu *dpf.DPU,
	tmpl *joinscript.Template,
	secretKey types.NamespacedName,
) error {
	clusterObj := dpf.NewDPUCluster()
	if err := r.Get(ctx, dpu.Spec.Cluster.ObjectKey(), clusterObj); err != nil {
		return fmt.Errorf("getting DPUCluster %s: %w", dpu.Spec.Cluster, err)
	}

	access, err := r.NewClusterAccess(ctx, r.Client, clusterObj)
	if err != nil {
		return r.RecordFailure(dpuObj, reasonMintFailed, err)
	}

	token, err := k0stoken.Mint(ctx, access.Client, access.APIServerURL, access.CACert, r.TokenTTL, r.Now())
	if err != nil {
		return r.RecordFailure(dpuObj, reasonMintFailed,
			fmt.Errorf("minting join token for DPU %s/%s: %w", dpuObj.GetNamespace(), dpuObj.GetName(), err))
	}

	script, err := joinscript.Render(tmpl, &joinscript.Data{
		JoinToken:        token.Encoded,
		TokenExpiresAt:   token.ExpiresAt.Format(time.RFC3339),
		APIServerURL:     access.APIServerURL,
		NodeName:         dpf.NodeName(dpuObj.GetName()),
		DPUName:          dpuObj.GetName(),
		DPUNamespace:     dpuObj.GetNamespace(),
		ClusterName:      dpu.Spec.Cluster.Name,
		ClusterNamespace: dpu.Spec.Cluster.Namespace,
		Values:           tmpl.Values,
	})
	if err != nil {
		// The script the token was minted for was never written, and the render fails again
		// on every retry, so leaving it behind would pile up live credentials.
		RevokeToken(ctx, access.Client, token)

		return r.RecordFailure(dpuObj, reasonRenderFailed, err)
	}

	if err := r.WriteSecret(ctx, dpuObj, secretKey, tmpl, token, script); err != nil {
		return err
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
	if dpuObj.GetDeletionTimestamp() != nil {
		// The Secret is owned by the DPU and goes with it.
		return ctrl.Result{}, nil
	}

	dpu, err := dpf.ProjectDPU(dpuObj)
	if err != nil {
		return ctrl.Result{}, err
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

		return ctrl.Result{}, r.RecordFailure(dpuObj, reason, err)
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

	renewIn, current, err := r.CurrentScript(ctx, secretKey, tmpl)
	if err != nil {
		return ctrl.Result{}, err
	}
	if current {
		log.V(1).Info("join script is current", "renewIn", renewIn.Round(time.Second))

		return ctrl.Result{RequeueAfter: renewIn}, nil
	}

	if err := r.RenderJoinSecret(ctx, dpuObj, dpu, tmpl, secretKey); err != nil {
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
