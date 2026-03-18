# Aegis Stream

A high-throughput, Kubernetes-native data router built in Go. Aegis Stream ingests binary events over TCP, routes them through a concurrent worker pool, and auto-scales based on backpressure metrics.

**Throughput:** ~150,000–200,000 events/sec end-to-end on a single node (varies with system load).

## Architecture

```
Binance WSS ──► cmd/feed ──► TCP :9000
                                │
                                ▼
               ┌─────────────────────────────────────┐
               │  aegis-stream pod                   │
               │  TCP Listener → Buffer → Workers    │
               │       │                    │        │
               │  /healthz             /metrics      │
               └─────────────────────────────────────┘
                                │
                    ┌───────────┼───────────┐
                    ▼           ▼           ▼
                 stdout    PostgreSQL     NATS
                                      (3-node JS)
                                          │
                                          ▼
                                   cmd/consumer
                                  (price alerts)
```

**Sink layer:** Pluggable sink interface routes events to stdout, PostgreSQL (batch inserts), or NATS JetStream (pub/sub fan-out).

**NATS cluster:** 3-node JetStream cluster with stream replication. Durable consumers with explicit ack for at-least-once delivery.

**Operator layer:** An `AegisPipeline` CRD lets you declare the entire deployment in a single YAML. The operator reconciles Deployments, Services, and config automatically.

**Dashboard:** A React frontend with a Go backend queries Prometheus and the K8s API to show live metrics, charts, and cost estimation.

## Project Structure

```
aegis-stream/
├── cmd/
│   ├── server/       # Main TCP server
│   ├── client/       # Test client
│   ├── bench/        # Throughput benchmark
│   ├── consumer/     # NATS JetStream consumer (price alerts)
│   ├── feed/         # Live Binance market data feeder
│   └── stress/       # Multi-connection stress test
├── internal/
│   ├── config/       # Flags + env var configuration
│   ├── frame/        # TCP length-prefix framing
│   ├── metrics/      # Prometheus metrics + /healthz
│   ├── server/       # Integration test
│   └── sink/         # Pluggable sink interface (stdout, postgres, nats)
├── proto/            # Protobuf schema (Trade, Log, Event)
├── pb/               # Generated protobuf Go code
├── k8s/
│   ├── nats/         # NATS JetStream 3-node cluster manifests
│   ├── consumer/     # Consumer deployment manifest
│   └── ...           # Server Deployment, Service, HPA
├── operator/         # Kubebuilder operator (CRD + controller)
├── dashboard/
│   ├── api/          # Go backend (Prometheus + K8s API proxy)
│   ├── web/          # React + Tailwind + Recharts frontend
│   └── k8s/          # Dashboard K8s manifests
├── Dockerfile        # Multi-stage server build (11.8MB)
├── Dockerfile.consumer  # Multi-stage consumer build
└── Makefile
```

## Quick Start

### Prerequisites

- Go 1.24+
- Protobuf compiler (`protoc`)
- Docker
- k3s (for K8s deployment)
- NATS server (for local NATS testing, or use the k3s cluster)

### Build and run locally

```bash
make build
make run          # starts the TCP server on :9000, metrics on :2112
```

### Send test events

```bash
./bin/client      # sends demo Trade and Log events
```

### Run benchmark

```bash
make bench        # starts server, blasts 100k events, reports throughput
```

### Stream live market data

```bash
# Terminal 1
./bin/server

# Terminal 2 — connects to Binance public WebSocket, no API key needed
./bin/feed
```

Real BTC, ETH, SOL trades flow through the pipeline at ~60 events/sec.

### Run stress test

```bash
# With server running:
./bin/stress -duration 15s -spike-duration 5s -rate 5000 -connections 10
```

Sends 5,000 events/sec across 10 connections, then spikes to 25,000/sec (5x).

### Run tests

```bash
make test
```

### Deploy to k3s

```bash
# Build and import the image
make docker
docker save aegis-stream:latest -o /tmp/aegis-stream.tar
sudo k3s ctr images import /tmp/aegis-stream.tar

# Deploy
kubectl apply -f k8s/deployment.yaml
kubectl apply -f k8s/service.yaml
kubectl apply -f k8s/hpa.yaml

# Verify
kubectl get pods -l app=aegis-stream
kubectl port-forward svc/aegis-stream 9000:9000 2112:2112
```

### Deploy the operator

```bash
cd operator
make docker-build IMG=aegis-operator:latest
# Import image into k3s, then:
make deploy IMG=aegis-operator:latest

# Create a pipeline
kubectl apply -f config/samples/stream_v1alpha1_aegispipeline.yaml
kubectl get aegispipelines
```

### Deploy the dashboard

```bash
# Build images
docker build -f dashboard/Dockerfile.api -t aegis-dashboard-api:latest dashboard/
docker build -t aegis-dashboard-web:latest dashboard/web/

# Import into k3s and deploy
kubectl apply -f dashboard/k8s/rbac.yaml
kubectl apply -f dashboard/k8s/deployment.yaml
kubectl apply -f dashboard/k8s/service.yaml

# Access
kubectl port-forward svc/aegis-dashboard 8080:80
# Open http://localhost:8080
```

### Deploy NATS + consumer to k3s

```bash
# Deploy NATS JetStream cluster (3 nodes)
kubectl apply -f k8s/nats/configmap.yaml
kubectl apply -f k8s/nats/service.yaml
kubectl apply -f k8s/nats/statefulset.yaml

# Verify cluster
kubectl get pods -l app=aegis-nats    # all 3 should be 1/1 Running

# Build and deploy the consumer
docker build -f Dockerfile.consumer -t aegis-consumer:latest .
docker save aegis-consumer:latest -o /tmp/aegis-consumer.tar
sudo k3s ctr images import /tmp/aegis-consumer.tar
kubectl apply -f k8s/consumer/deployment.yaml

# Run the full loop: Binance → feed → aegis-stream → NATS → consumer
kubectl port-forward svc/aegis-stream 9000:9000
./bin/feed                            # in another terminal
kubectl logs -f -l app=aegis-consumer # watch trades arrive
```

## Configuration

The server reads config from flags and environment variables (env vars take priority):

| Env Var | Default | Description |
|---------|---------|-------------|
| `AEGIS_PORT` | `:9000` | TCP listen address |
| `AEGIS_METRICS_PORT` | `:2112` | HTTP metrics/health port |
| `AEGIS_WORKERS` | `100` | Worker goroutines |
| `AEGIS_QUEUE_DEPTH` | `100000` | Buffered channel capacity |
| `AEGIS_MAX_CONNS` | `1000` | Max simultaneous TCP connections |
| `AEGIS_READ_TIMEOUT` | `30s` | TCP read deadline per frame |
| `AEGIS_PROCESS_DELAY` | `0` | Per-event processing delay to simulate I/O (e.g. `10ms`) |
| `AEGIS_SINK` | `stdout` | Event destination: `stdout`, `postgres`, or `nats` |
| `AEGIS_POSTGRES_URL` | | PostgreSQL connection string (when sink=postgres) |
| `AEGIS_NATS_URL` | | NATS server URL (when sink=nats), e.g. `nats://aegis-nats:4222` |

## AegisPipeline CRD

```yaml
apiVersion: stream.aegis.io/v1alpha1
kind: AegisPipeline
metadata:
  name: my-pipeline
spec:
  replicas: 3
  workers: 50
  queueDepth: 100000
  port: 9000
  metricsPort: 2112
  costPerPodHour: "0.05"
```

The operator creates the corresponding Deployment and Service, maps spec fields to env vars, and reports status:

```
$ kubectl get aegispipelines
NAME          REPLICAS   READY   PHASE     AGE
my-pipeline   3          3       Running   5m
```

## Key Design Decisions

- **Protobuf over JSON** — binary serialization reduces message size and parse time at 200k+ events/sec
- **TCP length-prefix framing** — 4-byte Big Endian header solves TCP's stream-vs-message boundary problem
- **Worker pool + buffered channel** — fixed goroutine count avoids memory explosion from unbounded goroutine spawning
- **sync.Pool for buffers** — reuses byte buffers to minimize GC pressure
- **scratch Docker image** — 11.8MB with zero attack surface (no shell, no OS)
- **HPA on queue depth** — scales on actual backpressure, not CPU (which doesn't reflect I/O-bound load)
- **Pluggable sink interface** — `Write()` + `Close()` makes destinations interchangeable (stdout, PostgreSQL, NATS)
- **NATS JetStream over Kafka** — lightweight Go-native broker, single binary, minimal config, ideal for learning and small clusters
- **Durable consumers with explicit ack** — at-least-once delivery, survives consumer restarts

## Observability

**Prometheus metrics** exposed at `/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `aegis_events_processed_total` | Counter | Events processed, labeled by type |
| `aegis_event_errors_total` | Counter | Failed event deserializations |
| `aegis_active_connections` | Gauge | Current TCP connections |
| `aegis_queue_depth` | Gauge | Current buffered channel length |
| `aegis_processing_duration_seconds` | Histogram | Per-event processing latency |

**Grafana dashboard** (`k8s/grafana-dashboard.json`) provides 7 panels: throughput, queue depth, connections, errors, latency p50/p95, pod count, and total events.

**React dashboard** adds live cost estimation from the CRD's `costPerPodHour` field, which Grafana can't access.

## Implementation Phases

1. **Core Engine** — TCP server, protobuf, worker pool, graceful shutdown
2. **Hardening** — tests, config, Prometheus metrics, health checks, structured logging, resilience (panic recovery, read deadlines, connection limits, backpressure)
3. **K8s Integration** — Dockerfile, Deployment, Service, HPA, Grafana dashboard
4. **Operator** — CRD, kubebuilder controller, reconciliation loop, self-healing
5. **Dashboard** — Go API proxy, React/Tailwind/Recharts frontend, cost panel
6. **Real Data** — Binance WebSocket feeder, multi-connection stress test with sustained + spike phases
7. **Sink + Storage** — Pluggable sink interface, PostgreSQL persistence, dashboard trade history
8. **NATS Fan-Out** — JetStream pub/sub, 3-node cluster, durable consumer with price alerts
9. **Cloud Deploy** *(next)* — Production deployment on EKS/GKE with TLS and auth
