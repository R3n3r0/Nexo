// Package v1alpha1 is the API of the Nexo operator: the NexoCluster resource
// (group nexo.r3n3r0.github.io), a Nexo deployment of one or more pools of
// servers on Kubernetes.
//
// +kubebuilder:object:generate=true
// +groupName=nexo.r3n3r0.github.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version of the API.
var GroupVersion = schema.GroupVersion{Group: "nexo.r3n3r0.github.io", Version: "v1alpha1"}

// SchemeBuilder registers the types of the API.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the types of the API to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &NexoCluster{}, &NexoClusterList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
