package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Input is this Function's per-pipeline-step configuration. The task list
// itself now lives on the XR's own spec.tasks (see TaskSpec in fn.go) —
// Input only carries defaults applied to every task unless overridden.
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:resource:categories=crossplane
type Input struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// DefaultHeaders are merged into every task's HTTP request, unless a
	// task sets the same header key itself.
	// +optional
	DefaultHeaders map[string]string `json:"defaultHeaders,omitempty"`
}