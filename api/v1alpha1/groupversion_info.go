// Package v1alpha1 contains the joule.azzhomelab.tech/v1alpha1 API.
// +kubebuilder:object:generate=true
// +groupName=joule.azzhomelab.tech
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of this API.
	GroupVersion = schema.GroupVersion{Group: "joule.azzhomelab.tech", Version: "v1alpha1"}

	// SchemeBuilder adds the types of this API to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types of this API to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &EnergyBudget{}, &EnergyBudgetList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
