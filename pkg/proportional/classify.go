package proportional

import (
	"fmt"
	"path"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	resourcehelper "k8s.io/component-helpers/resource"

	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/apis/v1alpha1"
)

// Classifier decides which pods and nodes take part in proportional scaling.
type Classifier struct {
	excludedNamespaces  []string
	nodeSelector        labels.Selector
	includeTaintedNodes bool
	resources           []corev1.ResourceName
}

// NewClassifier builds a classifier for the policy. operatorNamespace is always excluded.
func NewClassifier(spec *v1alpha1.ProportionalResourcePolicySpec, operatorNamespace string) (*Classifier, error) {
	c := &Classifier{
		includeTaintedNodes: spec.IncludeTaintedNodes,
		nodeSelector:        labels.Everything(),
	}
	if operatorNamespace != "" {
		c.excludedNamespaces = append(c.excludedNamespaces, operatorNamespace)
	}
	for _, pattern := range spec.ExcludedNamespaces {
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("invalid excluded namespace pattern %q: %w", pattern, err)
		}
		c.excludedNamespaces = append(c.excludedNamespaces, pattern)
	}
	if spec.NodeSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(spec.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid node selector: %w", err)
		}
		c.nodeSelector = selector
	}
	for _, pool := range spec.ResourcePools() {
		c.resources = append(c.resources, pool.Name)
	}
	return c, nil
}

// NamespaceExcluded reports whether pods in the namespace are left alone.
func (c *Classifier) NamespaceExcluded(namespace string) bool {
	for _, pattern := range c.excludedNamespaces {
		if ok, _ := path.Match(pattern, namespace); ok {
			return true
		}
	}
	return false
}

// Eligible reports whether the pod may be managed. It does not consider whether the pod carries
// the original resources annotation.
func (c *Classifier) Eligible(pod *corev1.Pod) bool {
	if _, mirror := pod.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return false
	}
	if pod.Annotations[AnnotationExclude] == "true" {
		return false
	}
	return !c.NamespaceExcluded(pod.Namespace)
}

// NodeCounted reports whether the node contributes to the detected capacity of the cluster.
func (c *Classifier) NodeCounted(node *corev1.Node) bool {
	if node.Spec.Unschedulable || !node.DeletionTimestamp.IsZero() {
		return false
	}
	if !c.nodeSelector.Matches(labels.Set(node.Labels)) {
		return false
	}
	if !c.includeTaintedNodes {
		for _, taint := range node.Spec.Taints {
			if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
				return false
			}
		}
	}
	return true
}

// Resources are the managed resource names.
func (c *Classifier) Resources() []corev1.ResourceName {
	return c.resources
}

// Units is an amount of each resource in base units (see ToUnits).
type Units map[corev1.ResourceName]int64

// unitsOf converts the managed resources of a resource list to units.
func (c *Classifier) unitsOf(list corev1.ResourceList) Units {
	out := make(Units, len(c.resources))
	for _, name := range c.resources {
		if q, ok := list[name]; ok {
			out[name] = ToUnits(name, q)
		}
	}
	return out
}

// Weight is the scheduler-effective request of the pod's original resources, excluding overhead.
func (c *Classifier) Weight(pod *corev1.Pod, orig *OriginalResources) Units {
	return c.unitsOf(resourcehelper.PodRequests(OriginalPod(pod, orig), resourcehelper.PodResourcesOptions{
		ExcludeOverhead: true,
	}))
}

// Requests is the scheduler-effective request of the pod as it currently is, including overhead.
func (c *Classifier) Requests(pod *corev1.Pod) Units {
	return c.unitsOf(resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{}))
}

// Overhead is the pod overhead of the pod.
func (c *Classifier) Overhead(pod *corev1.Pod) Units {
	return c.unitsOf(pod.Spec.Overhead)
}

// PodTerminal reports whether the pod has finished and no longer holds resources.
func PodTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}
