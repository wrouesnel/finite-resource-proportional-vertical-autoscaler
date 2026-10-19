// Package proportional implements the calculation at the heart of the autoscaler: the original
// requests of every managed pod are treated as weights, and each pod is given its proportional share
// of the finite resources of the cluster.
package proportional

import (
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

const (
	// AnnotationOriginalResources records the resources a pod was submitted with. It is managed by
	// the operator and is the source of the pod's proportional weight.
	AnnotationOriginalResources = "frpva.wrouesnel.github.com/original-resources"
	// AnnotationExclude opts a pod out of management when set to "true".
	AnnotationExclude = "frpva.wrouesnel.github.com/exclude"
)

var errNoAnnotation = errors.New("pod has no original resources annotation")

// Resources are the requests and limits of a container or pod.
type Resources struct {
	Requests corev1.ResourceList `json:"requests,omitempty"`
	Limits   corev1.ResourceList `json:"limits,omitempty"`
}

// OriginalResources is the content of AnnotationOriginalResources. Containers are keyed by name.
type OriginalResources struct {
	Containers     map[string]Resources `json:"containers,omitempty"`
	InitContainers map[string]Resources `json:"initContainers,omitempty"`
	Pod            *Resources           `json:"pod,omitempty"`
}

func fromRequirements(r corev1.ResourceRequirements) Resources {
	return Resources{Requests: r.Requests.DeepCopy(), Limits: r.Limits.DeepCopy()}
}

// RecordOriginalResources captures the current resources of the pod.
func RecordOriginalResources(pod *corev1.Pod) *OriginalResources {
	orig := &OriginalResources{
		Containers:     make(map[string]Resources, len(pod.Spec.Containers)),
		InitContainers: make(map[string]Resources, len(pod.Spec.InitContainers)),
	}
	for i := range pod.Spec.Containers {
		orig.Containers[pod.Spec.Containers[i].Name] = fromRequirements(pod.Spec.Containers[i].Resources)
	}
	for i := range pod.Spec.InitContainers {
		orig.InitContainers[pod.Spec.InitContainers[i].Name] = fromRequirements(pod.Spec.InitContainers[i].Resources)
	}
	if pod.Spec.Resources != nil {
		r := fromRequirements(*pod.Spec.Resources)
		orig.Pod = &r
	}
	return orig
}

// Encode serializes the original resources for storage in the annotation.
func (o *OriginalResources) Encode() (string, error) {
	data, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("encoding original resources: %w", err)
	}
	return string(data), nil
}

// DecodeOriginalResources parses an annotation value.
func DecodeOriginalResources(value string) (*OriginalResources, error) {
	orig := &OriginalResources{}
	if err := json.Unmarshal([]byte(value), orig); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", AnnotationOriginalResources, err)
	}
	return orig, nil
}

// OriginalResourcesOf decodes the original resources annotation of the pod.
func OriginalResourcesOf(pod *corev1.Pod) (*OriginalResources, error) {
	value, ok := pod.Annotations[AnnotationOriginalResources]
	if !ok {
		return nil, errNoAnnotation
	}
	return DecodeOriginalResources(value)
}

// SetOriginalResources writes the annotation onto the pod.
func SetOriginalResources(pod *corev1.Pod, orig *OriginalResources) error {
	value, err := orig.Encode()
	if err != nil {
		return err
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationOriginalResources] = value
	return nil
}

// OriginalPod returns a copy of the pod with its original resources restored.
func OriginalPod(pod *corev1.Pod, orig *OriginalResources) *corev1.Pod {
	out := pod.DeepCopy()
	restore := func(containers []corev1.Container, from map[string]Resources) {
		for i := range containers {
			if r, ok := from[containers[i].Name]; ok {
				containers[i].Resources.Requests = r.Requests.DeepCopy()
				containers[i].Resources.Limits = r.Limits.DeepCopy()
			}
		}
	}
	restore(out.Spec.Containers, orig.Containers)
	restore(out.Spec.InitContainers, orig.InitContainers)
	if orig.Pod != nil {
		out.Spec.Resources = &corev1.ResourceRequirements{
			Requests: orig.Pod.Requests.DeepCopy(),
			Limits:   orig.Pod.Limits.DeepCopy(),
		}
	}
	return out
}

// RecordMissing adds the current resources of any containers of the pod which are not yet recorded,
// such as sidecars injected by another admission webhook after the pod was first scaled.
func (o *OriginalResources) RecordMissing(pod *corev1.Pod) {
	record := func(containers []corev1.Container, into *map[string]Resources) {
		for i := range containers {
			if _, ok := (*into)[containers[i].Name]; ok {
				continue
			}
			if *into == nil {
				*into = map[string]Resources{}
			}
			(*into)[containers[i].Name] = fromRequirements(containers[i].Resources)
		}
	}
	record(pod.Spec.Containers, &o.Containers)
	record(pod.Spec.InitContainers, &o.InitContainers)
}
