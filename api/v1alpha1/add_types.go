package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AddSpec defines the desired state of Add
type AddSpec struct {
	// X is the first operand
	// +kubebuilder:validation:Required
	X int32 `json:"x"`

	// Y is the second operand
	// +kubebuilder:validation:Required
	Y int32 `json:"y"`
}

// AddStatus defines the observed state of Add
type AddStatus struct {
	// Result is x + y, set by the controller
	// +optional
	Result *int32 `json:"result,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="X",type=integer,JSONPath=`.spec.x`
// +kubebuilder:printcolumn:name="Y",type=integer,JSONPath=`.spec.y`
// +kubebuilder:printcolumn:name="Result",type=integer,JSONPath=`.status.result`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Add is the Schema for the adds API
type Add struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AddSpec   `json:"spec,omitempty"`
	Status AddStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AddList contains a list of Add
type AddList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Add `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Add{}, &AddList{})
		return nil
	})
}
