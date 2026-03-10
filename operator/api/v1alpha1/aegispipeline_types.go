/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AegisPipelineSpec defines the desired state of AegisPipeline.
// Users write these fields in their YAML — the Operator reads them
// and creates a matching Deployment + Service.
type AegisPipelineSpec struct {
	// replicas is the number of aegis-stream pods to run.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=2
	Replicas int32 `json:"replicas"`

	// image is the container image to deploy.
	// +kubebuilder:default="docker.io/library/aegis-stream:latest"
	Image string `json:"image"`

	// workers is the number of worker goroutines per pod.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=50
	Workers int32 `json:"workers"`

	// queueDepth is the buffered channel capacity per pod.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:default=100000
	QueueDepth int32 `json:"queueDepth"`

	// port is the TCP listen port for event traffic.
	// +kubebuilder:default=9000
	Port int32 `json:"port"`

	// metricsPort is the HTTP port for Prometheus /metrics and /healthz.
	// +kubebuilder:default=2112
	MetricsPort int32 `json:"metricsPort"`

	// costPerPodHour is the estimated cost in USD for running one pod per hour.
	// Used by the React dashboard to calculate live cost-per-second.
	// +kubebuilder:default="0.05"
	CostPerPodHour string `json:"costPerPodHour"`

	// sinkType selects where processed events are routed: "stdout" or "postgres".
	// +kubebuilder:validation:Enum=stdout;postgres
	// +kubebuilder:default="stdout"
	// +optional
	SinkType string `json:"sinkType,omitempty"`

	// postgresURL is the connection string for the PostgreSQL sink.
	// Required when sinkType is "postgres".
	// Example: "postgres://aegis:aegis@aegis-postgres:5432/aegis"
	// +optional
	PostgresURL string `json:"postgresURL,omitempty"`
}

// AegisPipelineStatus defines the observed state of AegisPipeline.
// The Operator updates these fields so users and the dashboard can
// see what's actually running.
type AegisPipelineStatus struct {
	// readyReplicas is the number of pods that are currently Running and healthy.
	ReadyReplicas int32 `json:"readyReplicas"`

	// phase is the current lifecycle state: Pending, Running, or Failed.
	Phase string `json:"phase"`

	// conditions represent the detailed state of the resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AegisPipeline is the Schema for the aegispipelines API.
// It represents a single deployment of the aegis-stream data router.
type AegisPipeline struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec AegisPipelineSpec `json:"spec"`

	// +optional
	Status AegisPipelineStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AegisPipelineList contains a list of AegisPipeline
type AegisPipelineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AegisPipeline `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AegisPipeline{}, &AegisPipelineList{})
}
