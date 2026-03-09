package prometheus

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client queries the Prometheus HTTP API for instant metric values.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a Prometheus client pointing at the given base URL
// (e.g. "http://prometheus:9090").
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Metrics holds the dashboard metrics extracted from Prometheus.
type Metrics struct {
	EventsPerSec      map[string]float64 `json:"eventsPerSec"`      // keyed by event type
	QueueDepth        float64            `json:"queueDepth"`
	ActiveConnections float64            `json:"activeConnections"`
	ErrorRate         float64            `json:"errorRate"`
	LatencyP50        float64            `json:"latencyP50"`
	LatencyP95        float64            `json:"latencyP95"`
	PodCount          float64            `json:"podCount"`
	TotalEvents       float64            `json:"totalEvents"`
}

// promResponse is the JSON shape returned by Prometheus /api/v1/query.
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]interface{}    `json:"value"` // [timestamp, "value"]
		} `json:"result"`
	} `json:"data"`
}

// FetchMetrics runs all the PromQL queries and returns a Metrics struct.
func (c *Client) FetchMetrics() (*Metrics, error) {
	m := &Metrics{
		EventsPerSec: make(map[string]float64),
	}

	// Events per second by type
	resp, err := c.query(`sum(rate(aegis_events_processed_total[5m])) by (type)`)
	if err != nil {
		return nil, fmt.Errorf("events/sec query: %w", err)
	}
	for _, r := range resp.Data.Result {
		val := parseValue(r.Value)
		typeName := r.Metric["type"]
		if typeName == "" {
			typeName = "unknown"
		}
		m.EventsPerSec[typeName] = val
	}

	// Queue depth
	resp, err = c.query(`sum(aegis_queue_depth)`)
	if err != nil {
		return nil, fmt.Errorf("queue depth query: %w", err)
	}
	if len(resp.Data.Result) > 0 {
		m.QueueDepth = parseValue(resp.Data.Result[0].Value)
	}

	// Active connections
	resp, err = c.query(`sum(aegis_active_connections)`)
	if err != nil {
		return nil, fmt.Errorf("connections query: %w", err)
	}
	if len(resp.Data.Result) > 0 {
		m.ActiveConnections = parseValue(resp.Data.Result[0].Value)
	}

	// Error rate
	resp, err = c.query(`sum(rate(aegis_event_errors_total[5m]))`)
	if err != nil {
		return nil, fmt.Errorf("error rate query: %w", err)
	}
	if len(resp.Data.Result) > 0 {
		m.ErrorRate = parseValue(resp.Data.Result[0].Value)
	}

	// Latency P50
	resp, err = c.query(`histogram_quantile(0.50, sum(rate(aegis_processing_duration_seconds_bucket[5m])) by (le))`)
	if err != nil {
		return nil, fmt.Errorf("p50 query: %w", err)
	}
	if len(resp.Data.Result) > 0 {
		m.LatencyP50 = parseValue(resp.Data.Result[0].Value)
	}

	// Latency P95
	resp, err = c.query(`histogram_quantile(0.95, sum(rate(aegis_processing_duration_seconds_bucket[5m])) by (le))`)
	if err != nil {
		return nil, fmt.Errorf("p95 query: %w", err)
	}
	if len(resp.Data.Result) > 0 {
		m.LatencyP95 = parseValue(resp.Data.Result[0].Value)
	}

	// Pod count
	resp, err = c.query(`count(aegis_queue_depth)`)
	if err != nil {
		return nil, fmt.Errorf("pod count query: %w", err)
	}
	if len(resp.Data.Result) > 0 {
		m.PodCount = parseValue(resp.Data.Result[0].Value)
	}

	// Total events
	resp, err = c.query(`sum(aegis_events_processed_total)`)
	if err != nil {
		return nil, fmt.Errorf("total events query: %w", err)
	}
	if len(resp.Data.Result) > 0 {
		m.TotalEvents = parseValue(resp.Data.Result[0].Value)
	}

	return m, nil
}

// query executes a Prometheus instant query.
func (c *Client) query(promQL string) (*promResponse, error) {
	u := fmt.Sprintf("%s/api/v1/query?query=%s", c.baseURL, url.QueryEscape(promQL))

	resp, err := c.httpClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned %d: %s", resp.StatusCode, string(body))
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if pr.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: status=%s", pr.Status)
	}

	return &pr, nil
}

// parseValue extracts the float64 from a Prometheus [timestamp, "value"] pair.
// Prometheus can return "NaN" or "+Inf" for some queries (e.g. histogram_quantile
// with no data). Go's json.Encoder can't serialize NaN/Inf — it produces an
// empty response. We convert these to 0 so the JSON is always valid.
func parseValue(v [2]interface{}) float64 {
	s, ok := v[1].(string)
	if !ok {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}
