package metrics

import (
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metric types used here:
//
// Counter — only goes up (total events processed, total errors).
//   Use for: "how many X happened since the server started?"
//
// Gauge — goes up and down (active connections, current queue depth).
//   Use for: "what is the current value of X right now?"
//
// Histogram — tracks the distribution of values (processing latency).
//   Use for: "what does the p50/p95/p99 latency look like?"

var (
	// EventsProcessed counts total events successfully deserialized and routed.
	// Labels let you filter by event type (trade vs log) in Grafana queries.
	EventsProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aegis_events_processed_total",
			Help: "Total number of events processed, labeled by type",
		},
		[]string{"type"}, // "trade" or "log"
	)

	// EventErrors counts events that failed to deserialize.
	EventErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "aegis_event_errors_total",
			Help: "Total number of events that failed to unmarshal",
		},
	)

	// ActiveConnections tracks how many TCP clients are connected right now.
	// Workers increment on connect, decrement on disconnect.
	ActiveConnections = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "aegis_active_connections",
			Help: "Number of currently connected TCP clients",
		},
	)

	// QueueDepth tracks how many jobs are sitting in the channel waiting
	// for a worker. This is the key metric for HPA scaling in K8s —
	// when it stays high, the system needs more pods.
	QueueDepth = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "aegis_queue_depth",
			Help: "Current number of jobs in the worker queue",
		},
	)

	// ProcessingDuration tracks how long each event takes from unmarshal
	// to routing. Buckets define the histogram boundaries in seconds.
	ProcessingDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "aegis_processing_duration_seconds",
			Help:    "Time spent processing each event",
			Buckets: []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01},
			// Buckets in seconds: 10us, 50us, 100us, 500us, 1ms, 5ms, 10ms
			// Chosen for a sub-millisecond system — most events should land in the first few buckets.
		},
	)
)

// Register adds all metrics to the default Prometheus registry.
// Must be called once at startup before any metrics are recorded.
func Register() {
	prometheus.MustRegister(EventsProcessed)
	prometheus.MustRegister(EventErrors)
	prometheus.MustRegister(ActiveConnections)
	prometheus.MustRegister(QueueDepth)
	prometheus.MustRegister(ProcessingDuration)
}

// shutdownFlag is set to 1 during graceful shutdown.
// The /healthz handler checks this to return 503 instead of 200.
var shutdownFlag atomic.Int32

// SetShutdown marks the server as shutting down.
// After this, /healthz returns 503 so K8s stops sending new traffic.
func SetShutdown() {
	shutdownFlag.Store(1)
}

// Serve starts an HTTP server on the given address that exposes:
//   - /metrics  — Prometheus scrape endpoint
//   - /healthz  — K8s liveness/readiness probe
//
// This runs on a separate port from the TCP data plane so monitoring
// never competes with event traffic.
func Serve(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// During shutdown, return 503 so K8s removes this pod from the Service
		// endpoints and stops routing new traffic to it.
		if shutdownFlag.Load() == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("shutting down\n"))
			return
		}
		// 200 = healthy, K8s keeps sending traffic
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})
	return http.ListenAndServe(addr, mux)
}
