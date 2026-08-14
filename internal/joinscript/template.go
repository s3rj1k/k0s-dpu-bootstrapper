// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

// Package joinscript resolves and renders the join script a DPU executes, from a labeled
// ConfigMap so that no CRD has to be installed into someone else's cluster.
package joinscript

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"code.local/k0s-dpu-bootstrapper/internal/dpf"
)

// ErrAmbiguous reports that more than one equally specific template serves one DPU, which
// only an operator can resolve. Every other resolve failure is a different problem.
var ErrAmbiguous = errors.New("more than one join script template matches")

const (
	// TemplateLabel marks a ConfigMap as a join script template.
	TemplateLabel = "k0s.mirantis.com/dpu-join-script"
	// TemplateLabelValue is the only accepted value for TemplateLabel.
	TemplateLabelValue = "true"

	// ClusterNameAnnotation scopes a template to a DPUCluster by name.
	ClusterNameAnnotation = "k0s.mirantis.com/dpu-cluster-name"
	// ClusterNamespaceAnnotation scopes a template to a DPUCluster by namespace.
	ClusterNamespaceAnnotation = "k0s.mirantis.com/dpu-cluster-namespace"
	// FlavorAnnotation optionally narrows a template to one DPUFlavor in that cluster.
	// A template without it serves every flavor in the cluster.
	FlavorAnnotation = "k0s.mirantis.com/dpu-flavor"
	// SkipValidationAnnotation set to "true" hands the rendered script to the DPU without
	// parsing it, the escape hatch when the parser is wrong about a working script.
	SkipValidationAnnotation = "k0s.mirantis.com/skip-script-validation"

	// ScriptKey holds the script template. Every other key in the ConfigMap is a value
	// exposed to that template under .Values.
	ScriptKey = "join.sh"
)

// Template is a resolved join script template.
type Template struct {
	Values          map[string]string
	Name            string
	Namespace       string
	ResourceVersion string
	UID             types.UID
	Script          string
	// SkipValidation carries SkipValidationAnnotation through to the caller.
	SkipValidation bool
}

// ConfigMapRef identifies the ConfigMap a template was resolved from, which is enough for
// an Event to be recorded against it.
func (t *Template) ConfigMapRef() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: t.Name, Namespace: t.Namespace, UID: t.UID},
	}
}

// ValuesFromData returns every ConfigMap key except the script.
func ValuesFromData(data map[string]string) map[string]string {
	values := maps.Clone(data)
	delete(values, ScriptKey)

	return values
}

// Ref identifies the template revision a script came from.
func (t *Template) Ref() string {
	return t.Name + "@" + t.ResourceVersion
}

// Target is the DPU a template is matched against.
type Target struct {
	Cluster dpf.ClusterRef
	Flavor  string
}

// ClusterRefFromAnnotations reads the DPUCluster a template ConfigMap is scoped to.
func ClusterRefFromAnnotations(annotations map[string]string) dpf.ClusterRef {
	return dpf.ClusterRef{
		Name:      annotations[ClusterNameAnnotation],
		Namespace: annotations[ClusterNamespaceAnnotation],
	}
}

// Resolve finds the one template serving a DPU, where a template narrowed to its flavor
// wins over one serving the whole cluster and two equally specific ones are an error.
func Resolve(ctx context.Context, c client.Reader, namespace string, target Target) (*Template, error) {
	list := &corev1.ConfigMapList{}
	if err := c.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{TemplateLabel: TemplateLabelValue})},
	); err != nil {
		return nil, fmt.Errorf("listing join script templates in %s: %w", namespace, err)
	}

	var scoped, clusterWide []*corev1.ConfigMap
	for i := range list.Items {
		cm := &list.Items[i]

		ref := ClusterRefFromAnnotations(cm.Annotations)

		// Both halves of the scope are required, and a template carrying only one of them
		// would otherwise be ignored without a word.
		if ref.IsZero() {
			ctrllog.FromContext(ctx).Info("ignoring a join script template that is not scoped to a DPUCluster",
				"configMap", cm.Namespace+"/"+cm.Name,
				"requiredAnnotations", ClusterNameAnnotation+", "+ClusterNamespaceAnnotation)

			continue
		}

		if ref != target.Cluster {
			continue
		}

		switch flavor := cm.Annotations[FlavorAnnotation]; flavor {
		case "":
			clusterWide = append(clusterWide, cm)
		case target.Flavor:
			scoped = append(scoped, cm)
		}
	}

	matches, scope := scoped, "DPUFlavor "+target.Flavor
	if len(matches) == 0 {
		matches, scope = clusterWide, "DPUCluster "+target.Cluster.String()
	}

	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
	default:
		names := make([]string, 0, len(matches))
		for _, cm := range matches {
			names = append(names, cm.Name)
		}
		return nil, fmt.Errorf("%w: found %d for %s (%v), exactly one is required",
			ErrAmbiguous, len(matches), scope, names)
	}

	cm := matches[0]
	script, ok := cm.Data[ScriptKey]
	if !ok || script == "" {
		return nil, fmt.Errorf("join script template %s/%s is missing the %q key", cm.Namespace, cm.Name, ScriptKey)
	}

	return &Template{
		Name:            cm.Name,
		Namespace:       cm.Namespace,
		ResourceVersion: cm.ResourceVersion,
		UID:             cm.UID,
		Script:          script,
		Values:          ValuesFromData(cm.Data),
		SkipValidation:  cm.Annotations[SkipValidationAnnotation] == "true",
	}, nil
}

// Data is what a join script template can reference.
type Data struct {
	Values           map[string]string
	JoinToken        string
	TokenExpiresAt   string
	APIServerURL     string
	NodeName         string
	DPUName          string
	DPUNamespace     string
	ClusterName      string
	ClusterNamespace string
}

// Render executes the template. A value that is not set is an error, since the result
// runs as root on the DPU.
func Render(t *Template, data *Data) (string, error) {
	tmpl, err := template.New(t.Name).Option("missingkey=error").Parse(t.Script)
	if err != nil {
		return "", fmt.Errorf("parsing join script template %s/%s: %w", t.Namespace, t.Name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering join script template %s/%s: %w", t.Namespace, t.Name, err)
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("join script template %s/%s rendered empty", t.Namespace, t.Name)
	}
	return buf.String(), nil
}
