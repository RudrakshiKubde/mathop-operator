// workflow_types.go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WorkflowTaskSpec is one embedded step. The operator creates a real
// HTTPTask child object for each entry.
type WorkflowTaskSpec struct {
	// Name must be unique within this workflow; other steps reference it
	// by this name in their InputFrom — not a full apiVersion/kind/name,
	// since it's always another step in this same workflow.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// +optional
	Method string `json:"method,omitempty"`

	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Input runtime.RawExtension `json:"input,omitempty"`

	// +optional
	InputFrom []WorkflowFieldMapping `json:"inputFrom,omitempty"`
}

type WorkflowFieldMapping struct {
	// SourceStep is the Name of another step in this same workflow
	// +kubebuilder:validation:Required
	SourceStep string `json:"sourceStep"`
	// +kubebuilder:validation:Required
	SourceField string `json:"sourceField"`
	// +kubebuilder:validation:Required
	TargetField string `json:"targetField"`
}

type WorkflowSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Tasks []WorkflowTaskSpec `json:"tasks"`

	// NotifyURL, if set, receives an HTTP POST with the final Workflow status
	// once the workflow reaches a terminal phase (Succeeded or Failed).
	// +optional
	NotifyURL string `json:"notifyURL,omitempty"`
}

type WorkflowTaskStatus struct {
	Name       string `json:"name"`
	Phase      string `json:"phase"` // Pending, Running, Succeeded, Failed
	StatusCode *int32 `json:"statusCode,omitempty"`
	// +kubebuilder:pruning:PreserveUnknownFields
	Output runtime.RawExtension `json:"output,omitempty"`
	Error  string               `json:"error,omitempty"`
}

type WorkflowStatus struct {
	// +optional
	TransactionID string `json:"transactionID,omitempty"`
	// +optional
	DashboardURL string `json:"dashboardURL,omitempty"`
	Phase        string `json:"phase,omitempty"` // Pending, Running, Succeeded, Failed
	// +optional
	Tasks []WorkflowTaskStatus `json:"tasks,omitempty"`

	// NotifiedPhase records the last phase for which NotifyURL was
	// successfully called, so we don't re-POST every reconcile.
	// +optional
	NotifiedPhase string `json:"notifiedPhase,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="TransactionID",type=string,JSONPath=`.status.transactionID`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.dashboardURL`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type Workflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkflowSpec   `json:"spec,omitempty"`
	Status WorkflowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Workflow{}, &WorkflowList{})
		return nil
	})
}
