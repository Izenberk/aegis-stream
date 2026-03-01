# Aegis Stream: Improvement Tasks

**Goal:** Learn Go & K8s by building. Logic code is written by hand; boilerplate/config is assisted.

---

## Phase 1.5 — Harden Before You Distribute

These should be done before starting Phase 2 (K8s).

### 1. Makefile + Unit Tests
- [x] Create a `Makefile` with targets: `build`, `test`, `proto`, `bench`
- [x] Write unit tests for TCP length-prefix framing (read/write)
- [x] Write unit tests for protobuf serialization/deserialization
- [x] Write an integration test that starts the server and sends events

### 2. Configuration via Env Vars / Flags
- [x] Replace hard-coded constants (port, worker count, buffer size, queue depth)
- [x] Use `flag` package or environment variables so K8s can configure pods externally
- [x] Validate config values on startup (e.g. port range, positive worker count)

### 3. Prometheus Metrics Endpoint
- [x] Add an HTTP `/metrics` endpoint using `prometheus/client_golang`
- [x] Export counters: events processed, errors, active connections
- [x] Export gauges: queue depth, worker count
- [x] Export histograms: event processing latency

### 4. HTTP Health Check (`/healthz`)
- [x] Add an HTTP listener (same port as /metrics) for health probes
- [x] Return 200 when server is accepting connections and workers are running
- [x] Return 503 during shutdown

### 5. Structured Logging (`log/slog`)
- [x] Replace all `log.Printf` calls with `slog` (Info, Warn, Error levels)
- [x] Add contextual fields: remote addr, worker ID, event fields
- [x] Remove commented-out debug `fmt.Printf` lines

### 6. Resilience Improvements
- [x] Add `recover()` in worker goroutines to prevent silent crashes
- [x] Set TCP read deadlines to prevent slow-client blocking
- [x] Add a max connection limit to prevent file descriptor exhaustion
- [x] Log or signal when the job channel is near capacity (backpressure warning)

### 7. Benchmark Accuracy
- [x] Reuse Prometheus `aegis_events_processed_total` as server-side counter
- [x] Update bench tool to query `/metrics` before and after, report true end-to-end throughput

---

## Phase 2 — Kubernetes Integration

Only start these after Phase 1.5 is complete.

- [x] Multi-stage `Dockerfile` (scratch base, actual: 11.8MB)
- [x] Import image into k3s via `docker save` + `k3s ctr images import`
- [x] `Deployment` manifest with env-based config, liveness/readiness probes
- [x] `Service` manifest (ClusterIP, TCP + metrics ports)
- [x] `HorizontalPodAutoscaler` based on Prometheus `aegis_queue_depth` metric
- [x] Grafana dashboard (7 panels: throughput, queue depth, connections, errors, latency, replicas, total events)

---

## Phase 3 — Kubernetes Operator

- [ ] `AegisPipeline` Custom Resource Definition (CRD)
- [ ] Go-based Operator (kubebuilder) that provisions workers from YAML
- [ ] React dashboard for live system health and cost metrics

---

*Check off items as you complete them. Each task in Phase 1.5 is a learning opportunity — write the Go code yourself, ask for guidance when stuck.*

