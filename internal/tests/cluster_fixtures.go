// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"code.local/k0s-dpu-bootstrapper/internal/dpf"
)

// DPUClusterWithoutKubeconfig is a DPUCluster missing the field naming its kubeconfig.
func DPUClusterWithoutKubeconfig() *unstructured.Unstructured {
	u := dpf.NewDPUCluster()
	u.SetName("cluster-a")
	u.SetNamespace(Namespace)
	u.Object["spec"] = map[string]any{TypeKey: dpf.StaticClusterType}

	return u
}
