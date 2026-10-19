package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LimitPolicy controls what happens to container limits when requests are scaled.
// +kubebuilder:validation:Enum=Proportional;Unchanged
type LimitPolicy string

const (
	// LimitPolicyProportional scales limits by the same factor as requests. This preserves the
	// limit:request ratio of every container, and so preserves the pod QoS class.
	LimitPolicyProportional LimitPolicy = "Proportional"
	// LimitPolicyUnchanged leaves the original limits in place, raising them only where the scaled
	// request would otherwise exceed them.
	LimitPolicyUnchanged LimitPolicy = "Unchanged"
)

// RebalanceMode controls how already running pods are brought back into proportion.
// +kubebuilder:validation:Enum=InPlace;Disabled
type RebalanceMode string

const (
	// RebalanceModeInPlace resizes running pods through the pods/resize subresource.
	RebalanceModeInPlace RebalanceMode = "InPlace"
	// RebalanceModeDisabled only scales pods when they are admitted.
	RebalanceModeDisabled RebalanceMode = "Disabled"
)

// ResourcePoolSpec configures one finite resource of the cluster.
type ResourcePoolSpec struct {
	// Name is the resource to manage.
	// +kubebuilder:validation:Enum=cpu;memory;ephemeral-storage
	Name corev1.ResourceName `json:"name"`

	// Capacity is the total amount of the resource in the cluster. If unset, it is detected as
	// the sum of the allocatable resources of the counted nodes, less HeadroomPercent.
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty"`

	// HeadroomPercent is the percentage of detected node allocatable which is held back.
	// It does not apply when Capacity is set.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=99
	// +optional
	HeadroomPercent *int32 `json:"headroomPercent,omitempty"`

	// MinRequest is the smallest request any container will be scaled down to.
	// +optional
	MinRequest *resource.Quantity `json:"minRequest,omitempty"`
}

// RebalanceSpec configures rebalancing of running pods.
type RebalanceSpec struct {
	// Mode selects how running pods are rebalanced.
	// +kubebuilder:default=InPlace
	// +optional
	Mode RebalanceMode `json:"mode,omitempty"`

	// TolerancePercent is how far a running container's request may drift from its proportional
	// value before the pod is resized.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	TolerancePercent *int32 `json:"tolerancePercent,omitempty"`

	// MinInterval is the shortest time between two rebalancing passes.
	// +kubebuilder:default="30s"
	// +optional
	MinInterval *metav1.Duration `json:"minInterval,omitempty"`

	// AdoptExistingPods records the current requests of running pods which were never admitted
	// by the webhook (e.g. those created before the operator was installed) as their proportional
	// weights, and then manages them like any other pod.
	// +optional
	AdoptExistingPods bool `json:"adoptExistingPods,omitempty"`
}

// ProportionalResourcePolicySpec configures the autoscaler.
type ProportionalResourcePolicySpec struct {
	// Suspend stops both admission-time scaling and rebalancing.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Resources are the finite resources which are shared out proportionally.
	// +kubebuilder:default={{name: cpu}, {name: memory}, {name: ephemeral-storage}}
	// +listType=map
	// +listMapKey=name
	// +optional
	Resources []ResourcePoolSpec `json:"resources,omitempty"`

	// ExcludedNamespaces are namespace names or glob patterns (e.g. "kube-*") whose pods are not
	// managed. The requests of pods in these namespaces are treated as fixed. The namespace the
	// operator runs in is always excluded.
	// +kubebuilder:default={"kube-system", "kube-public", "kube-node-lease"}
	// +optional
	ExcludedNamespaces []string `json:"excludedNamespaces,omitempty"`

	// NodeSelector restricts which nodes are counted when detecting cluster capacity.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`

	// IncludeTaintedNodes counts nodes with NoSchedule or NoExecute taints when detecting cluster
	// capacity. By default they are skipped, as are unschedulable (cordoned) and not-ready nodes.
	// +optional
	IncludeTaintedNodes bool `json:"includeTaintedNodes,omitempty"`

	// SubtractUnmanaged removes the requests of unmanaged pods from the capacity which is shared
	// out among managed pods.
	// +kubebuilder:default=true
	// +optional
	SubtractUnmanaged *bool `json:"subtractUnmanaged,omitempty"`

	// LimitPolicy controls how container limits are adjusted.
	// +kubebuilder:default=Proportional
	// +optional
	LimitPolicy LimitPolicy `json:"limitPolicy,omitempty"`

	// Rebalance configures rebalancing of running pods.
	// +kubebuilder:default={}
	// +optional
	Rebalance RebalanceSpec `json:"rebalance,omitempty"`
}

// ResourcePoolStatus reports the observed state of one finite resource.
type ResourcePoolStatus struct {
	// Name is the resource.
	Name corev1.ResourceName `json:"name"`
	// NodeAllocatable is the total allocatable resource of the counted nodes.
	NodeAllocatable resource.Quantity `json:"nodeAllocatable"`
	// Capacity is the configured capacity, or the detected capacity after headroom.
	Capacity resource.Quantity `json:"capacity"`
	// Unmanaged is the total request of unmanaged pods on counted nodes.
	Unmanaged resource.Quantity `json:"unmanaged"`
	// Available is the capacity shared out among managed pods.
	Available resource.Quantity `json:"available"`
	// TotalWeight is the sum of the original requests of all managed pods.
	TotalWeight resource.Quantity `json:"totalWeight"`
	// Factor is the ratio of Available to TotalWeight by which original requests are scaled.
	Factor string `json:"factor"`
}

// ProportionalResourcePolicyStatus is the observed state of the autoscaler.
type ProportionalResourcePolicyStatus struct {
	// ObservedGeneration is the generation last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// CountedNodes is the number of nodes counted towards detected capacity.
	// +optional
	CountedNodes int32 `json:"countedNodes,omitempty"`
	// ManagedPods is the number of pods being scaled.
	// +optional
	ManagedPods int32 `json:"managedPods,omitempty"`
	// UnmanagedPods is the number of pods on counted nodes which are not being scaled.
	// +optional
	UnmanagedPods int32 `json:"unmanagedPods,omitempty"`
	// Resources is the state of each managed resource.
	// +listType=map
	// +listMapKey=name
	// +optional
	Resources []ResourcePoolStatus `json:"resources,omitempty"`
	// LastRebalanceTime is when running pods were last rebalanced.
	// +optional
	LastRebalanceTime *metav1.Time `json:"lastRebalanceTime,omitempty"`
	// Conditions describe the state of the autoscaler.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ProportionalResourcePolicy configures the cluster-wide proportional scaling of pod resources.
// The operator acts on a single policy, named "default" unless configured otherwise.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=prp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.countedNodes`
// +kubebuilder:printcolumn:name="Managed",type=integer,JSONPath=`.status.managedPods`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ProportionalResourcePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProportionalResourcePolicySpec   `json:"spec,omitempty"`
	Status ProportionalResourcePolicyStatus `json:"status,omitempty"`
}

// ProportionalResourcePolicyList contains a list of ProportionalResourcePolicy.
//
// +kubebuilder:object:root=true
type ProportionalResourcePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProportionalResourcePolicy `json:"items"`
}
