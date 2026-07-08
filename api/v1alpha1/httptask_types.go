package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SourceReference points at another HTTPTask by name.
type SourceReference struct {
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// FieldMapping pulls one field out of another Task's output JSON and places
// it into this Task's request body before the HTTP call is made.
type FieldMapping struct {
	// +kubebuilder:validation:Required
	SourceRef SourceReference `json:"sourceRef"`
	// SourceField is a dot-path into the source Task's status.output, e.g. "sum" or "data.result"
	// +kubebuilder:validation:Required
	SourceField string `json:"sourceField"`
	// TargetField is a dot-path into THIS task's request body, e.g. "value" or "a.b"
	// +kubebuilder:validation:Required
	TargetField string `json:"targetField"`
}

type HTTPTaskSpec struct {
	// Endpoint is the full URL to call, e.g. "http://localhost:9090/add"
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// Method defaults to POST if empty
	// +optional
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Input is the static/base JSON request body. Fields from InputFrom are
	// merged into a copy of this before the request is sent.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Input runtime.RawExtension `json:"input,omitempty"`

	// InputFrom maps fields from other HTTPTasks' outputs into this Task's
	// request body. A Task with no InputFrom entries is a "root" task.
	// +optional
	InputFrom []FieldMapping `json:"inputFrom,omitempty"`
}

type HTTPTaskStatus struct {
	// Output is the raw JSON response body from the HTTP call
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Output runtime.RawExtension `json:"output,omitempty"`

	// StatusCode of the HTTP response
	// +optional
	StatusCode *int32 `json:"statusCode,omitempty"`

	// ObservedInputHash records a hash of the last request body actually
	// sent, so we don't re-call the endpoint when nothing has changed.
	// +optional
	ObservedInputHash string `json:"observedInputHash,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
// +kubebuilder:printcolumn:name="StatusCode",type=integer,JSONPath=`.status.statusCode`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type HTTPTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HTTPTaskSpec   `json:"spec,omitempty"`
	Status HTTPTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type HTTPTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HTTPTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &HTTPTask{}, &HTTPTaskList{})
		return nil
	})
}
