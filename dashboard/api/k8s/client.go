package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// PipelineInfo is the JSON shape returned by /api/pipeline.
type PipelineInfo struct {
	Name   string       `json:"name"`
	Spec   PipelineSpec `json:"spec"`
	Status PipelineStatus `json:"status"`
}

type PipelineSpec struct {
	Replicas       int32  `json:"replicas"`
	Workers        int32  `json:"workers"`
	QueueDepth     int32  `json:"queueDepth"`
	Port           int32  `json:"port"`
	MetricsPort    int32  `json:"metricsPort"`
	CostPerPodHour string `json:"costPerPodHour"`
	Image          string `json:"image"`
}

type PipelineStatus struct {
	ReadyReplicas int32  `json:"readyReplicas"`
	Phase         string `json:"phase"`
}

var aegisPipelineGVR = schema.GroupVersionResource{
	Group:    "aegis.io",
	Version:  "v1alpha1",
	Resource: "aegispipelines",
}

// Client fetches AegisPipeline custom resources from the K8s API.
type Client struct {
	dynClient dynamic.Interface
	namespace string
	name      string
}

// NewClient creates a K8s client using in-cluster config.
// name is the AegisPipeline CR name, namespace is where it lives.
func NewClient(namespace, name string) (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}

	return &Client{
		dynClient: dyn,
		namespace: namespace,
		name:      name,
	}, nil
}

// FetchPipeline retrieves the AegisPipeline CR and returns a PipelineInfo.
func (c *Client) FetchPipeline() (*PipelineInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obj, err := c.dynClient.Resource(aegisPipelineGVR).
		Namespace(c.namespace).
		Get(ctx, c.name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting AegisPipeline %s/%s: %w", c.namespace, c.name, err)
	}

	return parsePipeline(obj)
}

func parsePipeline(obj *unstructured.Unstructured) (*PipelineInfo, error) {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, err
	}

	var raw struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec   PipelineSpec   `json:"spec"`
		Status PipelineStatus `json:"status"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	return &PipelineInfo{
		Name:   raw.Metadata.Name,
		Spec:   raw.Spec,
		Status: raw.Status,
	}, nil
}
