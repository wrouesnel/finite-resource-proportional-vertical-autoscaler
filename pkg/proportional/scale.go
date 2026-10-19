package proportional

import (
	"math/big"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/apis/v1alpha1"
)

// memoryGranularity is the unit scaled byte quantities are rounded to.
const memoryGranularity = 1024

// ToUnits converts a quantity to integer base units: millicores for CPU and bytes for everything else.
func ToUnits(name corev1.ResourceName, q resource.Quantity) int64 {
	if name == corev1.ResourceCPU {
		return q.MilliValue()
	}
	return q.Value()
}

// FromUnits converts base units back to a quantity.
func FromUnits(name corev1.ResourceName, v int64) resource.Quantity {
	if name == corev1.ResourceCPU {
		return *resource.NewMilliQuantity(v, resource.DecimalSI)
	}
	return *resource.NewQuantity(v, resource.BinarySI)
}

// granularity is the smallest increment a scaled value is rounded to.
func granularity(name corev1.ResourceName) int64 {
	if name == corev1.ResourceCPU {
		return 1
	}
	return memoryGranularity
}

// ScaleUnits multiplies v by factor and rounds to the nearest granule of the resource. The result
// is never less than one granule.
func ScaleUnits(name corev1.ResourceName, v int64, factor *big.Rat) int64 {
	g := granularity(name)
	scaled := new(big.Rat).Mul(new(big.Rat).SetInt64(v), factor)
	scaled.Quo(scaled, new(big.Rat).SetInt64(g))
	// Round half up.
	scaled.Add(scaled, big.NewRat(1, 2))
	granules := new(big.Int).Quo(scaled.Num(), scaled.Denom())
	if !granules.IsInt64() {
		return v
	}
	return max(granules.Int64(), 1) * g
}

// ScaleOptions controls how a pod's original resources are scaled.
type ScaleOptions struct {
	// Factors is the scaling factor of each managed resource.
	Factors map[corev1.ResourceName]*big.Rat
	// MinRequest is the smallest request of each resource, in base units.
	MinRequest map[corev1.ResourceName]int64
	// LimitPolicy controls how limits are adjusted.
	LimitPolicy v1alpha1.LimitPolicy
}

// scaleResources returns orig with each managed resource scaled.
func (o *ScaleOptions) scaleResources(orig Resources) (corev1.ResourceList, corev1.ResourceList) {
	requests := orig.Requests.DeepCopy()
	limits := orig.Limits.DeepCopy()
	for name, factor := range o.Factors {
		reqQ, hasReq := orig.Requests[name]
		// A zero request carries no weight and is left alone.
		if !hasReq || ToUnits(name, reqQ) == 0 {
			continue
		}
		req := max(ScaleUnits(name, ToUnits(name, reqQ), factor), o.MinRequest[name])
		requests[name] = FromUnits(name, req)

		limQ, hasLim := orig.Limits[name]
		if !hasLim {
			continue
		}
		lim := ToUnits(name, limQ)
		if o.LimitPolicy != v1alpha1.LimitPolicyUnchanged {
			lim = ScaleUnits(name, lim, factor)
		}
		limits[name] = FromUnits(name, max(lim, req))
	}
	return requests, limits
}

// ScalePod returns a copy of the pod with its original resources scaled.
func ScalePod(pod *corev1.Pod, orig *OriginalResources, opts *ScaleOptions) *corev1.Pod {
	out := pod.DeepCopy()
	apply := func(containers []corev1.Container, from map[string]Resources) {
		for i := range containers {
			r, ok := from[containers[i].Name]
			if !ok {
				continue
			}
			requests, limits := opts.scaleResources(r)
			containers[i].Resources.Requests = requests
			containers[i].Resources.Limits = limits
		}
	}
	apply(out.Spec.Containers, orig.Containers)
	apply(out.Spec.InitContainers, orig.InitContainers)
	if orig.Pod != nil {
		requests, limits := opts.scaleResources(*orig.Pod)
		out.Spec.Resources = &corev1.ResourceRequirements{Requests: requests, Limits: limits}
	}
	return out
}
