package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AddReference points at the Add resource whose result should be squared
type AddReference struct {
	// Name of the Add resource, in the same namespace as this Square
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// SquareSpec defines the desired state of Square
type SquareSpec struct {
	// AddRef references the Add resource to read the sum from
	// +kubebuilder:validation:Required
	AddRef AddReference `json:"addRef"`
}

// SquareStatus defines the observed state of Square
type SquareStatus struct {
	// Result is (referenced Add's result) squared
	// +optional
	Result *int32 `json:"result,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="AddRef",type=string,JSONPath=`.spec.addRef.name`
// +kubebuilder:printcolumn:name="Result",type=integer,JSONPath=`.status.result`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Square is the Schema for the squares API
type Square struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SquareSpec   `json:"spec,omitempty"`
	Status SquareStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SquareList contains a list of Square
type SquareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Square `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Square{}, &SquareList{})
		return nil
	})
}
