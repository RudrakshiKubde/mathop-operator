package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type SubtractSpec struct {
	// +kubebuilder:validation:Required
	X int32 `json:"x"`
	// +kubebuilder:validation:Required
	Y int32 `json:"y"`
}

type SubtractStatus struct {
	// +optional
	Result *int32 `json:"result,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="X",type=integer,JSONPath=`.spec.x`
// +kubebuilder:printcolumn:name="Y",type=integer,JSONPath=`.spec.y`
// +kubebuilder:printcolumn:name="Result",type=integer,JSONPath=`.status.result`

type Subtract struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SubtractSpec   `json:"spec,omitempty"`
	Status SubtractStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type SubtractList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Subtract `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Subtract{}, &SubtractList{})
		return nil
	})
}
