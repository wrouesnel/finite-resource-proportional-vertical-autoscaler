package proportional

import (
	"math/big"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	resourcehelper "k8s.io/component-helpers/resource"

	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/apis/v1alpha1"
)

// PodEntry is a pod with its decoded original resources. Original is nil for unmanaged pods.
type PodEntry struct {
	Pod      *corev1.Pod
	Original *OriginalResources
	weight   corev1.ResourceList
}

// Managed reports whether the pod is being scaled.
func (e *PodEntry) Managed() bool {
	return e.Original != nil
}

// EntryWeight is the weight of a managed pod (see Weight).
func (c *Classifier) EntryWeight(e *PodEntry) Units {
	return c.unitsOf(e.weight)
}

// Pool is the computed state of one managed resource. All amounts are in base units.
type Pool struct {
	Spec v1alpha1.ResourcePoolSpec
	// NodeAllocatable is the total allocatable of the counted nodes.
	NodeAllocatable int64
	// Capacity is the configured capacity, or NodeAllocatable less headroom.
	Capacity int64
	// Unmanaged is the total request of unmanaged pods on counted nodes.
	Unmanaged int64
	// Overhead is the total pod overhead of managed pods.
	Overhead int64
	// Available is the capacity shared out among managed pods.
	Available int64
	// Weight is the total original request of managed pods.
	Weight int64
	// MaxPod is the most any single pod may be scaled to, or 0 for no limit. It is the largest
	// counted node's allocatable less headroom, since no pod can be scheduled beyond that.
	MaxPod int64
	// MinRequest is the smallest request of any container.
	MinRequest int64
}

// Factor is the ratio by which original requests are scaled if a pod of weight extra joins the
// managed pods.
func (p *Pool) Factor(extra int64) *big.Rat {
	total := p.Weight + extra
	if total <= 0 {
		return big.NewRat(1, 1)
	}
	return big.NewRat(p.Available, total)
}

// Plan is the proportional scaling state of the cluster.
type Plan struct {
	Pools         []*Pool
	CountedNodes  int
	ManagedPods   int
	UnmanagedPods int
	LimitPolicy   v1alpha1.LimitPolicy
}

// headroom removes percent from v.
func headroom(v int64, percent int32) int64 {
	result := new(big.Int).Mul(big.NewInt(v), big.NewInt(int64(100-percent)))
	return result.Quo(result, big.NewInt(100)).Int64()
}

// BuildPlan computes the proportional scaling state from the nodes and pods of the cluster.
func BuildPlan(c *Classifier, spec *v1alpha1.ProportionalResourcePolicySpec, nodes []corev1.Node, pods []PodEntry) *Plan {
	plan := &Plan{LimitPolicy: spec.LimitPolicyOrDefault()}

	countedNodes := make(map[string]struct{}, len(nodes))
	allocatable := Units{}
	largest := Units{}
	for i := range nodes {
		node := &nodes[i]
		if !c.NodeCounted(node) {
			continue
		}
		countedNodes[node.Name] = struct{}{}
		for name, v := range c.unitsOf(node.Status.Allocatable) {
			allocatable[name] += v
			largest[name] = max(largest[name], v)
		}
	}
	plan.CountedNodes = len(countedNodes)

	weight, unmanaged, overhead := Units{}, Units{}, Units{}
	for i := range pods {
		entry := &pods[i]
		if entry.Managed() {
			plan.ManagedPods++
			for name, v := range c.EntryWeight(entry) {
				weight[name] += v
			}
			for name, v := range c.Overhead(entry.Pod) {
				overhead[name] += v
			}
			continue
		}
		if _, ok := countedNodes[entry.Pod.Spec.NodeName]; !ok {
			continue
		}
		plan.UnmanagedPods++
		for name, v := range c.Requests(entry.Pod) {
			unmanaged[name] += v
		}
	}

	for _, poolSpec := range spec.ResourcePools() {
		name := poolSpec.Name
		pool := &Pool{
			Spec:            poolSpec,
			NodeAllocatable: allocatable[name],
			Unmanaged:       unmanaged[name],
			Overhead:        overhead[name],
			Weight:          weight[name],
		}
		if poolSpec.Capacity != nil {
			pool.Capacity = ToUnits(name, *poolSpec.Capacity)
		} else {
			pool.Capacity = headroom(pool.NodeAllocatable, poolSpec.HeadroomPercentOrDefault())
		}
		if largest[name] > 0 {
			pool.MaxPod = min(headroom(largest[name], poolSpec.HeadroomPercentOrDefault()), pool.Capacity)
		}
		if poolSpec.MinRequest != nil {
			pool.MinRequest = ToUnits(name, *poolSpec.MinRequest)
		}
		pool.Available = pool.Capacity - pool.Overhead
		if spec.SubtractUnmanagedOrDefault() {
			pool.Available -= pool.Unmanaged
		}
		pool.Available = max(pool.Available, 0)
		plan.Pools = append(plan.Pools, pool)
	}
	return plan
}

// ScaleOptions returns the scaling of a pod with the given weight. If joining is true the pod is
// not yet counted in the plan (i.e. it is being admitted) and its weight is added to the total.
func (p *Plan) ScaleOptions(weight Units, joining bool) *ScaleOptions {
	opts := &ScaleOptions{
		Factors:     make(map[corev1.ResourceName]*big.Rat, len(p.Pools)),
		MinRequest:  make(map[corev1.ResourceName]int64, len(p.Pools)),
		LimitPolicy: p.LimitPolicy,
	}
	for _, pool := range p.Pools {
		name := pool.Spec.Name
		w := weight[name]
		extra := int64(0)
		if joining {
			extra = w
		}
		factor := pool.Factor(extra)
		if pool.MaxPod > 0 && w > 0 {
			if limit := big.NewRat(pool.MaxPod, w); limit.Cmp(factor) < 0 {
				factor = limit
			}
		}
		opts.Factors[name] = factor
		opts.MinRequest[name] = pool.MinRequest
	}
	return opts
}

// EntryCache caches the decoded original resources of pods, since decoding every annotation of
// the cluster on each admission would be expensive.
type EntryCache struct {
	mu      sync.Mutex
	entries map[types.UID]cachedEntry
}

type cachedEntry struct {
	annotation string
	original   *OriginalResources
	weight     corev1.ResourceList
}

// NewEntryCache returns an empty cache.
func NewEntryCache() *EntryCache {
	return &EntryCache{entries: map[types.UID]cachedEntry{}}
}

// Entry classifies a pod. Pods which are ineligible, or lack a valid annotation, are unmanaged.
func (e *EntryCache) Entry(c *Classifier, pod *corev1.Pod) PodEntry {
	annotation, ok := pod.Annotations[AnnotationOriginalResources]
	if !ok || !c.Eligible(pod) {
		return PodEntry{Pod: pod}
	}

	e.mu.Lock()
	cached, hit := e.entries[pod.UID]
	e.mu.Unlock()
	if hit && cached.annotation == annotation {
		return PodEntry{Pod: pod, Original: cached.original, weight: cached.weight}
	}

	orig, err := DecodeOriginalResources(annotation)
	if err != nil {
		return PodEntry{Pod: pod}
	}
	weight := resourcehelper.PodRequests(OriginalPod(pod, orig), resourcehelper.PodResourcesOptions{ExcludeOverhead: true})
	if pod.UID != "" {
		e.mu.Lock()
		e.entries[pod.UID] = cachedEntry{annotation: annotation, original: orig, weight: weight}
		e.mu.Unlock()
	}
	return PodEntry{Pod: pod, Original: orig, weight: weight}
}

// Retain drops cached entries of pods not in uids.
func (e *EntryCache) Retain(uids map[types.UID]struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for uid := range e.entries {
		if _, ok := uids[uid]; !ok {
			delete(e.entries, uid)
		}
	}
}

// Entries classifies every non-terminal pod in the list.
func (e *EntryCache) Entries(c *Classifier, pods []corev1.Pod) []PodEntry {
	entries := make([]PodEntry, 0, len(pods))
	for i := range pods {
		if PodTerminal(&pods[i]) {
			continue
		}
		entries = append(entries, e.Entry(c, &pods[i]))
	}
	return entries
}
