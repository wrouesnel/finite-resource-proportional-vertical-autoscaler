package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Defaults applied when a field is unset. These mirror the CRD defaults so policies built in code
// (or read from an apiserver which didn't default them) behave the same.
const (
	DefaultHeadroomPercent  int32 = 10
	DefaultTolerancePercent int32 = 5
	DefaultMinInterval            = 30 * time.Second
)

// DefaultResources are the resources managed when none are listed.
//
//nolint:gochecknoglobals
var DefaultResources = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage}

// HeadroomPercentOrDefault returns the headroom percentage of the resource pool.
func (r *ResourcePoolSpec) HeadroomPercentOrDefault() int32 {
	if r.HeadroomPercent == nil {
		return DefaultHeadroomPercent
	}
	return *r.HeadroomPercent
}

// ResourcePools returns the configured resource pools, or the defaults if none are configured.
func (s *ProportionalResourcePolicySpec) ResourcePools() []ResourcePoolSpec {
	if len(s.Resources) > 0 {
		return s.Resources
	}
	pools := make([]ResourcePoolSpec, 0, len(DefaultResources))
	for _, name := range DefaultResources {
		pools = append(pools, ResourcePoolSpec{Name: name})
	}
	return pools
}

// SubtractUnmanagedOrDefault returns whether unmanaged requests are removed from capacity.
func (s *ProportionalResourcePolicySpec) SubtractUnmanagedOrDefault() bool {
	if s.SubtractUnmanaged == nil {
		return true
	}
	return *s.SubtractUnmanaged
}

// LimitPolicyOrDefault returns the limit policy.
func (s *ProportionalResourcePolicySpec) LimitPolicyOrDefault() LimitPolicy {
	if s.LimitPolicy == "" {
		return LimitPolicyProportional
	}
	return s.LimitPolicy
}

// ModeOrDefault returns the rebalance mode.
func (r *RebalanceSpec) ModeOrDefault() RebalanceMode {
	if r.Mode == "" {
		return RebalanceModeInPlace
	}
	return r.Mode
}

// TolerancePercentOrDefault returns the rebalance tolerance.
func (r *RebalanceSpec) TolerancePercentOrDefault() int32 {
	if r.TolerancePercent == nil {
		return DefaultTolerancePercent
	}
	return *r.TolerancePercent
}

// MinIntervalOrDefault returns the minimum time between rebalancing passes.
func (r *RebalanceSpec) MinIntervalOrDefault() time.Duration {
	if r.MinInterval == nil {
		return DefaultMinInterval
	}
	return r.MinInterval.Duration
}
