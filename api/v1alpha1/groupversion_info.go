// Package v1alpha1 contains API Schema definitions for T-Cloud cluster networking.
// +kubebuilder:object:generate=true
// +groupName=infrastructure.otc.t-systems.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "infrastructure.otc.t-systems.com", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
