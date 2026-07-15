package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:resource:categories=crossplane
type Input struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// ProviderConfigName is the provider-http ProviderConfig used for every
	// task's DisposableRequest. Defaults to "default".
	// +optional
	ProviderConfigName string `json:"providerConfigName,omitempty"`

	// DefaultHeaders are merged into every task's request unless overridden.
	// +optional
	DefaultHeaders map[string]string `json:"defaultHeaders,omitempty"`
}