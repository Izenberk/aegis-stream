# Aegis Stream

A high-throughput, Kubernetes-native data router built in Go. Aegis Stream ingests binary events over TCP, routes them through a concurrent worker pool, and auto-scales based on backpressure metrics.

**Peak throughput:** 239,231 events/sec on a single node.

## Architecture

```
Clients (TCP)
    │
    ▼
┌─────────────────────────────────────────────┐
│  aegis-stream pod                           │
│  ┌─────────┐   ┌────────┐   ┌───────────┐  │
│  │ TCP     │──▶│ Buffer │──▶│ Worker    │  │
│  │ Listener│   │ Channel│   │ Pool (N)  │  │
│  └─────────┘   └────────┘   └───────────┘  │
│       │                           │         │
│  /healthz                    /metrics       │
└─────────────────────────────────────────────┘
    │                               │
    ▼                               ▼
K8s Probes                    Prometheus
                                    │
                              ┌─────┴──────┐
                              │ prometheus- │
                              │ adapter     │
                              └─────┬──────┘
                                    │
                                    ▼
                              HPA (2-10 pods)
```

**Operator layer:** An `AegisPipeline` CRD lets you declare the entire deployment in a single YAML. The operator reconciles Deployments, Services, and config automatically.

**Dashboard:** A React frontend with a Go backend queries Prometheus and the K8s API to show live metrics, charts, and cost estimation.

## Project Structure

```
aegis-stream/
├── cmd/
│   ├── server/       # Main TCP server
│   ├── client/       # Test client
│   └── bench/        # Throughput benchmark
├── internal/
│   ├── config/       # Flags + env var configuration
│   ├── frame/        # TCP length-prefix framing
│   ├── metrics/      # Prometheus metrics + /healthz
│   └── server/       # Integration test
├── proto/            # Protobuf schema (Trade, Log, Event)
├── pb/               # Generated protobuf Go code
├── k8s/              # K8s manifests (Deployment, Service, HPA)
├── operator/         # Kubebuilder operator (CRD + controller)
├── dashboard/
│   ├── api/          # Go backend (Prometheus + K8s API proxy)
│   ├── web/          # React + Tailwind + Recharts frontend
│   └── k8s/          # Dashboard K8s manifests
├── Dockerfile        # Multi-stage build (11.8MB image)
└── Makefile
```

## Quick Start

### Prerequisites

- Go 1.23+
- Protobuf compiler (`protoc`)
- Docker
- k3s (for K8s deployment)

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
