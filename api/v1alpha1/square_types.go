package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SourceReference points at ANY resource that publishes a status.result field.
// Square has zero compile-time / import-level dependency on what that resource's
// Go type is — it's resolved purely at runtime via apiVersion + kind.
type SourceReference struct {
	// APIVersion of the referenced resource, e.g. "math.example.com/v1alpha1"
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Kind of the referenced resource, e.g. "Add" or "Subtract"
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Name of the referenced resource, in the same namespace as this Square
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// SquareSpec defines the desired state of Square
type SquareSpec struct {
	// SourceRef references any resource whose status.result should be squared
	// +kubebuilder:validation:Required
	SourceRef SourceReference `json:"sourceRef"`
}

// SquareStatus defines the observed state of Square
type SquareStatus struct {
	// +optional
	Result *int32 `json:"result,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="SourceKind",type=string,JSONPath=`.spec.sourceRef.kind`
// +kubebuilder:printcolumn:name="SourceName",type=string,JSONPath=`.spec.sourceRef.name`
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
