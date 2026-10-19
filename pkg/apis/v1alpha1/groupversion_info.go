// Package v1alpha1 contains the v1alpha1 API of the finite resource proportional vertical autoscaler.
//
// +kubebuilder:object:generate=true
// +groupName=frpva.wrouesnel.github.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

//go:generate go tool controller-gen object paths=./...
//go:generate go tool controller-gen crd paths=./... output:crd:artifacts:config=../../../deploy/crd

// GroupName is the API group of this package.
const GroupName = "frpva.wrouesnel.github.com"

//nolint:gochecknoglobals
var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &ProportionalResourcePolicy{}, &ProportionalResourcePolicyList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
