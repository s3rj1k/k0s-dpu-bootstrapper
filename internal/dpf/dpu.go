// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

// Package dpf projects the DPF provisioning API from unstructured objects, keeping its
// CRDs a runtime rather than a compile dependency.
package dpf

import (
	"context"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// GroupName is the DPF provisioning API group.
	GroupName = "provisioning.dpu.nvidia.com"
	// Version is the API version this controller reads.
	Version = "v1alpha1"

	// KubeletConfiguredCondition is set once the agent has run the join script.
	KubeletConfiguredCondition = "KubeletConfigured"

	// JoinSecretKey is the Secret key the DPU agent reads and executes with bash.
	JoinSecretKey = "join"

	// KubeconfigSecretKey is the key DPF requires in a kubeconfig Secret.
	KubeconfigSecretKey = "super-admin.conf"

	// StaticClusterType is the DPUCluster type for an externally managed control plane.
	StaticClusterType = "static"
)

var (
	// DPUGVK identifies a single DPU object.
	DPUGVK = schema.GroupVersionKind{Group: GroupName, Version: Version, Kind: "DPU"}
	// DPUClusterGVK identifies a single DPUCluster object.
	DPUClusterGVK = schema.GroupVersionKind{Group: GroupName, Version: Version, Kind: "DPUCluster"}
)

func NewObject(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

func NewList(gvk schema.GroupVersionKind) *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	return l
}

// NewDPU returns an empty DPU object ready for a client Get.
func NewDPU() *unstructured.Unstructured {
	return NewObject(DPUGVK)
}

// NewDPUList returns an empty DPU list ready for a client List.
func NewDPUList() *unstructured.UnstructuredList {
	return NewList(DPUGVK)
}

// NewDPUCluster returns an empty DPUCluster object ready for a client Get.
func NewDPUCluster() *unstructured.Unstructured {
	return NewObject(DPUClusterGVK)
}

// ClusterRef points at the DPUCluster a DPU belongs to.
type ClusterRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// ObjectKey returns the ref as a client key.
func (r ClusterRef) ObjectKey() client.ObjectKey {
	return types.NamespacedName{Namespace: r.Namespace, Name: r.Name}
}

// IsZero reports whether the DPU has not been assigned to a cluster yet.
func (r ClusterRef) IsZero() bool { return r.Name == "" || r.Namespace == "" }

func (r ClusterRef) String() string { return r.Namespace + "/" + r.Name }

// Condition holds only the condition fields this controller reads.
type Condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// AgentStatus carries the conditions the agent reports from the DPU.
type AgentStatus struct {
	Conditions []Condition `json:"conditions"`
}

// DPUStatus is the projected DPU status.
type DPUStatus struct {
	AgentStatus *AgentStatus `json:"agentStatus"`
	Phase       string       `json:"phase"`
}

// DPUSpec is the projected DPU spec.
type DPUSpec struct {
	Cluster   ClusterRef `json:"cluster"`
	BFB       string     `json:"bfb"`
	DPUFlavor string     `json:"dpuFlavor"`
}

// DPU is the projection of the fields this controller reads from a DPU object.
type DPU struct {
	Status DPUStatus `json:"status"`
	Spec   DPUSpec   `json:"spec"`
}

// KubeletConfigured reports whether the agent has already run the join script.
func (d *DPU) KubeletConfigured() bool {
	if d.Status.AgentStatus == nil {
		return false
	}

	i := slices.IndexFunc(d.Status.AgentStatus.Conditions, func(c Condition) bool {
		return c.Type == KubeletConfiguredCondition
	})

	return i >= 0 && d.Status.AgentStatus.Conditions[i].Status == string(metav1.ConditionTrue)
}

// ProjectDPU converts an unstructured DPU into the projection.
func ProjectDPU(u *unstructured.Unstructured) (*DPU, error) {
	dpu := &DPU{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, dpu); err != nil {
		return nil, fmt.Errorf("projecting DPU %s/%s: %w", u.GetNamespace(), u.GetName(), err)
	}
	return dpu, nil
}

// ClusterIndexKey is the field index mapping a DPU to its DPUCluster.
const ClusterIndexKey = "spec.cluster"

// ClusterIndexValue is the index value for a DPU, its cluster.
func ClusterIndexValue(obj client.Object) []string {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	dpu, err := ProjectDPU(u)
	if err != nil || dpu.Spec.Cluster.IsZero() {
		return nil
	}
	return []string{dpu.Spec.Cluster.String()}
}

// IndexDPUByCluster registers ClusterIndexKey on the manager's cache.
func IndexDPUByCluster(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, NewDPU(), ClusterIndexKey, ClusterIndexValue)
}

// DPUClusterSpec is the projected DPUCluster spec.
type DPUClusterSpec struct {
	// Type is kamaji, static, or a value defined by a vendor.
	Type string `json:"type"`
	// Kubeconfig names a Secret in the DPUCluster's namespace.
	Kubeconfig string `json:"kubeconfig"`
}

// DPUCluster is the projection of the fields this controller reads from a DPUCluster.
type DPUCluster struct {
	Spec DPUClusterSpec `json:"spec"`
}

// ProjectDPUCluster converts an unstructured DPUCluster into the projection.
func ProjectDPUCluster(u *unstructured.Unstructured) (*DPUCluster, error) {
	dc := &DPUCluster{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, dc); err != nil {
		return nil, fmt.Errorf("projecting DPUCluster %s/%s: %w", u.GetNamespace(), u.GetName(), err)
	}
	return dc, nil
}

// JoinSecretName is the name DPF gives a DPU's join Secret, and the only name the agent
// may read in zero trust mode.
func JoinSecretName(dpuName string) string {
	return dpuName + "-kubeadm-join"
}

// NodeName is the name the DPU registers under, which DPF takes from the DPU object.
func NodeName(dpuName string) string {
	return dpuName
}

// OwnerReference builds the plain owner reference DPF sets on the join Secret.
func OwnerReference(u *unstructured.Unstructured) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: DPUGVK.GroupVersion().String(),
		Kind:       DPUGVK.Kind,
		Name:       u.GetName(),
		UID:        u.GetUID(),
	}
}
