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

- [x] `AegisPipeline` Custom Resource Definition (CRD)
- [x] Go-based Operator (kubebuilder) that provisions workers from YAML
- [x] React dashboard for live system health and cost metrics

---

## Phase 4 — Real Data Demo

Show Aegis Stream handling real production-like data, not just synthetic benchmarks.

### 1. Live Market Data Feeder (`cmd/feed`)
- [x] Connect to Binance public WebSocket (aggTrade stream, no API key needed)
- [x] Convert real crypto trades (BTC, ETH, SOL) into aegis-stream protobuf events
- [x] Generate mixed payloads: real Trades + synthetic Log events
- [x] Reconnect with exponential backoff on disconnect
- [x] Configurable via flags/env vars (server address, symbols, log interval)

### 2. Stress Test Tool (`cmd/stress`)
- [x] Multi-connection stress test (10+ concurrent TCP connections)
- [x] Sustained phase: constant rate for configurable duration
- [x] Spike phase: burst at N× multiplier to trigger HPA scale-up
- [x] Pre-generated varied payloads (70% Trade, 30% Log, random data)
- [x] Live progress reporting + final summary with server-side verification

### 3. End-to-End Validation
- [x] Run feed + stress together against k3s cluster
- [x] Observe HPA scaling under combined real + synthetic load
- [x] Watch React dashboard and Grafana during test

---

## Phase 5 — Sink Interface + PostgreSQL

Route processed events to real storage instead of stdout. Prove the full loop: ingest → process → store → query.

### 1. Sink Interface (`internal/sink`)
- [ ] Define `Sink` interface: `Write(event *pb.Event) error` + `Close() error`
- [ ] Implement `StdoutSink` (current behavior, extracted into interface)
- [ ] Wire sink into worker loop, replacing direct `slog.Info` calls
- [ ] Make sink selectable via config/env var (`AEGIS_SINK=stdout|postgres`)

### 2. PostgreSQL Sink
- [ ] Design schema: `trades` table (symbol, price, volume, timestamp) + `logs` table (level, message, service, timestamp)
- [ ] Implement `PostgresSink` using `pgx` (Go Postgres driver)
- [ ] Batch inserts for throughput (flush every N events or M milliseconds)
- [ ] Connection pooling and retry logic
- [ ] Add `AEGIS_POSTGRES_URL` env var for connection string

### 3. PostgreSQL in K8s
- [ ] Deploy PostgreSQL to k3s (StatefulSet or Helm chart)
- [ ] Update aegis-stream Deployment with `AEGIS_SINK=postgres` and DB connection
- [ ] Update AegisPipeline CRD to support sink configuration

### 4. Dashboard Integration
- [ ] Add `/api/trades` endpoint to dashboard API (query stored trades)
- [ ] Add a trades table/view to the React dashboard
- [ ] Show recent trades flowing through the pipeline in real time

### 5. End-to-End Validation
- [ ] Run feed → aegis-stream → PostgreSQL with live Binance data
- [ ] Query stored trades from dashboard
- [ ] Verify data integrity (events in = rows stored)

---

## Phase 6 — Service-to-Service (Future)

- [ ] Kafka/NATS sink for fan-out to multiple consumers
- [ ] Consumer services (alerting, analytics, archival)

## Phase 7 — Cloud Deployment (Future)

- [ ] Deploy to real cloud K8s (EKS/GKE or VPS with k3s)
- [ ] Production TLS, auth, and networking

---

*Check off items as you complete them. Each task is a learning opportunity — write the Go code yourself, ask for guidance when stuck.*

