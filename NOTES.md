# Aegis Stream: Learning Notes

Reference notes explaining the *why* behind each implementation decision.

---

## Phase 1: Core Concepts

### Protocol Buffers (Protobuf)

**What:** A binary serialization format created by Google. You define your data shape in a `.proto` file, then a compiler generates Go structs for you.

**Why not JSON?** JSON is text-based. Every field name (`"symbol"`, `"price"`) is sent as a string on every message. Protobuf uses small integer tags instead (field 1, field 2), making messages much smaller and faster to parse. For a system targeting 200k+ events/sec, this matters.

**The `oneof` pattern** (`schema.proto` line 23):
```protobuf
oneof payload {
  Trade trade = 2;
  Log log = 3;
}
```
This means an `Event` carries *either* a Trade *or* a Log, never both. In Go, this becomes a type switch — a core Go pattern for handling polymorphism without inheritance:
```go
switch payload := event.Payload.(type) {
case *pb.Event_Trade:
    // handle trade
case *pb.Event_Log:
    // handle log
}
```

**`option go_package`**: Tells the protobuf compiler where to place the generated Go code. Without this, it wouldn't know which Go module the code belongs to.

---

### TCP Length-Prefix Framing

**The problem:** TCP is a *stream* protocol, not a message protocol. If you send two 50-byte messages, TCP might deliver them as one 100-byte chunk, or as three chunks of 30+40+30 bytes. There are no built-in message boundaries.

**The solution:** Before every message, send a 4-byte header containing the message's length in Big Endian byte order:
```
[4 bytes: length][N bytes: protobuf payload]
[4 bytes: length][N bytes: protobuf payload]
...
```

**Why Big Endian?** It's the network standard ("network byte order"). Both sender and receiver agree on how to interpret the 4 bytes, regardless of what CPU architecture they run on.

**How it works in code:**

*Sender side* (`cmd/client/main.go`):
```go
lengthBuf := make([]byte, 4)
binary.BigEndian.PutUint32(lengthBuf, uint32(len(data)))  // encode length
conn.Write(lengthBuf)                                      // send header
conn.Write(data)                                           // send payload
```

*Receiver side* (`cmd/server/main.go`):
```go
lengthBuf := make([]byte, 4)
io.ReadFull(conn, lengthBuf)                               // read exactly 4 bytes
msgLen := binary.BigEndian.Uint32(lengthBuf)               // decode length
msgBuf := make([]byte, msgLen)
io.ReadFull(conn, msgBuf)                                  // read exactly msgLen bytes
```

**Why `io.ReadFull`?** A regular `conn.Read()` might return fewer bytes than requested (TCP can fragment). `io.ReadFull` loops internally until it gets *exactly* the number of bytes you asked for, or returns an error. This guarantees you always get a complete frame.

---

### Worker Pool Pattern

**The problem:** If you spawn a new goroutine for every incoming event, a burst of 200k events creates 200k goroutines. Each goroutine costs ~2-8KB of stack memory. That's up to 1.6GB of memory just for stacks, plus GC pressure from all those allocations.

**The solution:** Pre-spawn a fixed number of workers (100) that read from a shared channel:
```go
jobs := make(chan Job, 100000)   // buffered channel = the queue

for i := 0; i < 100; i++ {
    go worker(&wg, i, jobs)     // 100 workers, always running
}
```

**Why a buffered channel?** The `100000` capacity means up to 100,000 jobs can queue up without blocking the sender. Without this buffer, every `jobs <- job` would block until a worker picks it up, and the TCP reader couldn't keep pace with incoming data.

**The flow:**
1. TCP connection handler reads a frame and pushes a `Job` onto the channel
2. The next *available* worker picks it up automatically (Go's channel scheduling)
3. Worker processes the event, then loops back to wait for the next job

This is a fundamental Go concurrency pattern — channels as work queues between producers and consumers.

---

### sync.Pool (Zero-Copy Memory)

**The problem:** Every incoming message needs a byte buffer. If you `make([]byte, N)` for each message, the garbage collector (GC) must track and eventually free millions of small allocations. GC pauses kill latency in a high-throughput system.

**The solution:** Reuse buffers instead of allocating new ones:
```go
var bufferPool = sync.Pool{
    New: func() any {
        b := make([]byte, 4096)
        return &b
    },
}
```

**The lifecycle:**
1. **Borrow:** `bufPtr := bufferPool.Get().(*[]byte)` — grab a pre-allocated buffer
2. **Use:** `msgBuf := (*bufPtr)[:msgLen]` — slice it to the exact message size
3. **Return:** `bufferPool.Put(job.Buf)` — give it back for the next message

**Why 4096 bytes?** Most protobuf-encoded Trade/Log events are well under 4KB. A fixed-size pool avoids allocating different sizes for different messages.

**Why pass `*[]byte` (pointer to slice)?** A slice header in Go is already a small struct (pointer + length + capacity). But `sync.Pool` stores `any` (interface), and storing a slice in an interface causes an allocation. Storing a *pointer* to the slice avoids this extra allocation. It's a micro-optimization, but it matters at 200k+ events/sec.

**The `Job` struct ties it together:**
```go
type Job struct {
    Data []byte   // the actual message bytes (a sub-slice of Buf)
    Buf  *[]byte  // pointer to the pool buffer, so workers can return it
}
```
The worker needs both: `Data` to process the message, and `Buf` to return the underlying buffer to the pool when done.

---

### Graceful Shutdown

**The problem:** In Kubernetes, pods get killed regularly (scaling down, deployments, node drains). If the server dies mid-processing, events in the channel queue are lost.

**The solution:** A coordinated 4-step shutdown sequence:

```
Signal (SIGTERM/SIGINT)
  → 1. Close the TCP listener   (stop accepting new connections)
  → 2. Cancel the context        (tell connection handlers to stop reading)
  → 3. Close the jobs channel    (signal workers: no more jobs coming)
  → 4. wg.Wait()                 (block until all workers finish current jobs)
```

**Why this order matters:**
- Close listener *first* so no new connections arrive
- Cancel context *second* so existing handlers stop reading new frames
- Close channel *third* so workers drain remaining queued jobs
- Wait *last* so in-flight work completes before the process exits

**Key Go primitives used:**

| Primitive | Role |
|-----------|------|
| `os/signal` + `chan os.Signal` | Catch SIGTERM/SIGINT from K8s or Ctrl+C |
| `context.WithCancel` | Propagate "stop" signal to all goroutines |
| `close(jobs)` | Makes `for job := range jobs` exit after draining |
| `sync.WaitGroup` | Blocks `main()` until all workers return |

**Why K8s cares:** When K8s sends SIGTERM, it waits 30 seconds (default `terminationGracePeriodSeconds`) before force-killing. This shutdown sequence ensures queued events are processed within that window.

---

### `runtime.GOMAXPROCS(runtime.NumCPU())`

**What:** Sets the maximum number of OS threads that can execute Go code simultaneously.

**Why it's here:** Go defaults to using all CPUs since Go 1.5, so this line is technically redundant now. It's kept as explicit documentation — making it clear that this is a CPU-bound workload that should use all available cores. In K8s, this becomes relevant when you set CPU limits on pods and want the Go runtime to respect them.

---

### Benchmark Design (`cmd/bench`)

**The approach:** Pre-generate a single protobuf event, serialize it once, then blast 100k copies of the same binary frame to the server.

**Why pre-generate?** If you serialized inside the loop, you'd be benchmarking protobuf marshaling speed, not server throughput. By pre-building the frame, the benchmark measures pure network + server processing speed.

**The limitation:** The benchmark only measures client-side *send* speed — how fast data can be pushed into the TCP socket. It doesn't verify the server actually *processed* all 100k events. The OS TCP buffer absorbs the writes, so the client can finish before the server is done. This is why "Benchmark Accuracy" is a TODO item.

---

## Make & Makefile

### What is Make?

`make` is a build tool that's been around since 1976. It reads a file called `Makefile` in your project root and executes the commands you defined as "targets." Nearly every C, C++, and Go project uses it. In the K8s ecosystem, it's the standard way to build, test, and deploy.

### `go run` vs `make run`

| | `go run cmd/server/main.go` | `make run` |
|---|---|---|
| **What happens** | Compiles to a temp dir, runs, then deletes the binary | Compiles to `bin/server`, then runs that binary |
| **Binary reusable?** | No — gone after the process exits | Yes — `./bin/server` works without recompiling |
| **When to use** | Quick one-off testing during development | When you need the binary to persist (Docker, K8s, benchmarks) |

`make run` depends on `make build`, so it always compiles fresh before running. But the binary stays in `bin/` for reuse.

### Why Make matters for later phases

- **Docker** copies a compiled binary into the image — `go run` doesn't produce one
- **K8s** deploys containers with binaries, not source code
- **CI/CD** pipelines use `make test`, `make build` as standard steps
- **Team convention** — anyone clones the repo and knows `make build` works

### Makefile syntax basics

```makefile
target: dependencies
	command        # MUST be a real tab character, not spaces
```

- **target** — the name you type after `make` (e.g., `make build`)
- **dependencies** — other targets to run first (e.g., `run: build` means build first)
- **command** — shell commands to execute (each line runs in a separate shell)
- **`.PHONY`** — declares targets that aren't file names (prevents conflicts with directories)

### Aegis Stream Makefile targets

A Makefile is a build automation file — instead of typing long commands, you run `make build` or `make test`. It's standard in Go projects and across the K8s ecosystem.

**`.PHONY: build test bench proto clean run`**
Tells Make these are command names, not file names. Without this, if a folder called `build` existed, `make build` would say "nothing to do" because Make thinks the target file already exists.

**`build`**
Compiles all three binaries (`server`, `client`, `bench`) into a `bin/` directory. The `-o` flag controls the output path so binaries don't clutter the project root.

**`run: build`**
The `: build` part is a *dependency* — it means "run the `build` target first, then execute this one." So `make run` always recompiles before starting the server.

**`test`**
`go test ./...` is Go's convention for "run all tests in all packages recursively." The `-v` flag gives verbose output so you see each test name and its pass/fail result.

**`bench`**
Starts the server in the background (`&`), waits 1 second for it to bind the port, runs the benchmark tool, then kills the server. The `@` prefix suppresses printing the command itself, and `|| true` prevents Make from failing if the process already exited.

**`proto`**
Runs the protobuf compiler to regenerate `pb/schema.pb.go` from the `.proto` schema. Use this whenever you modify `proto/schema.proto`.

**`clean`**
Removes the `bin/` directory. Standard convention — every Makefile has a `clean` target for removing build artifacts.

---

## Configuration

### Why externalize config?

Hard-coded values like `Port = ":9000"` work for a single binary on your laptop. But in K8s, you deploy the *same* container image to different environments (dev, staging, prod) with different settings. You can't recompile the binary for each environment — you pass config from outside.

### The priority chain

```
Defaults → Flags → Environment Variables
(lowest)              (highest)
```

- **Defaults** — safe values baked into the code (`":9000"`, `100` workers)
- **Flags** — command-line overrides for local dev: `./bin/server -port :8080`
- **Env vars** — override everything, used by K8s Deployment manifests

### Why env vars win over flags

In K8s, you define env vars in the Deployment YAML:
```yaml
env:
  - name: AEGIS_PORT
    value: ":9000"
  - name: AEGIS_WORKERS
    value: "200"
```
K8s injects these into the container at startup. You can't easily pass flags to a container's entrypoint, but env vars are first-class.

### Key Go patterns used

**`os.LookupEnv` vs `os.Getenv`:**
- `os.Getenv("X")` returns `""` if unset — you can't tell "unset" from "set to empty string"
- `os.LookupEnv("X")` returns `(value, exists)` — you only override when `exists` is true

**`strconv.Atoi`:**
Env vars are always strings. `strconv.Atoi` converts `"100"` → `100`. Returns an error for invalid input like `"abc"`.

**Validation at startup:**
Catch bad config immediately with clear error messages, rather than crashing later with a confusing panic. Fail fast, fail loud.

---

## Prometheus Metrics

### What is Prometheus?

Prometheus is a monitoring system that *pulls* (scrapes) metrics from your app over HTTP. Your app exposes a `/metrics` endpoint, Prometheus visits it every 15 seconds (by default), and stores the time-series data. You then query it with Grafana dashboards.

### Why a separate port?

The TCP data plane (port 9000) handles high-speed binary event traffic. The HTTP metrics endpoint (port 2112) serves Prometheus scrapes. Keeping them separate means monitoring never competes with event processing for the same listener.

### Three metric types

**Counter** — only goes up. Resets to 0 when the process restarts.
```go
EventsProcessed.WithLabelValues("trade").Inc()  // +1
```
Use for: "how many trades have we processed since startup?"

**Gauge** — goes up and down. Reflects a current value.
```go
ActiveConnections.Inc()   // client connected
ActiveConnections.Dec()   // client disconnected
QueueDepth.Set(float64(len(jobs)))  // current queue size
```
Use for: "how many clients are connected *right now*?"

**Histogram** — tracks the distribution of values across predefined buckets.
```go
ProcessingDuration.Observe(time.Since(start).Seconds())
```
Use for: "what is the p95 latency?" Prometheus calculates percentiles from the bucket counts.

### Labels

Labels let you slice one metric by dimensions:
```go
EventsProcessed.WithLabelValues("trade").Inc()
EventsProcessed.WithLabelValues("log").Inc()
```
In Grafana, you can query `aegis_events_processed_total{type="trade"}` to see only trade events.

### Why this matters for K8s

The HorizontalPodAutoscaler (HPA) in Phase 2 will watch `aegis_queue_depth`. When it stays high, HPA knows the current pods can't keep up and spins up more replicas. When it drops, HPA scales down to save resources. This is the "elastic" part of the project.

### Testing metrics manually

```bash
curl localhost:2112/metrics | grep aegis
```
This shows all `aegis_*` metrics with their current values. Prometheus does the same thing automatically on a schedule.

---

## Health Checks (`/healthz`)

### What are K8s health probes?

K8s doesn't just start your pod and forget about it. It continuously checks if the pod is healthy using HTTP probes:

| Probe | Question it answers | What happens on failure |
|-------|-------------------|----------------------|
| **Liveness** | "Is the process stuck?" | K8s kills and restarts the pod |
| **Readiness** | "Can it accept traffic?" | K8s removes the pod from the Service (no new traffic) |

Both hit the same `/healthz` endpoint. The difference is how K8s reacts to failure.

### Why return 503 during shutdown?

The shutdown sequence is:

```
1. SIGTERM arrives
2. /healthz starts returning 503       ← K8s stops sending new traffic
3. Close TCP listener                  ← no new connections
4. Cancel context                      ← handlers stop reading
5. Close jobs channel + drain workers  ← finish in-flight work
6. Process exits
```

Without step 2, K8s keeps routing traffic to the pod during steps 3-6, causing connection errors for clients. The 503 tells K8s "I'm going away, send traffic elsewhere" *before* we actually stop listening.

### Why `atomic.Int32`?

The shutdown flag is set by `main()` and read by HTTP handler goroutines simultaneously. `atomic.Int32` is a lock-free, thread-safe integer — cheaper than a mutex for a simple flag. Same concept as `atomic.Int64` used in the integration test for counting processed events.

### Why share the port with /metrics?

Both `/healthz` and `/metrics` are operational endpoints — they serve the infrastructure (K8s, Prometheus), not the business logic (event processing). Putting them on the same HTTP server (port 2112) keeps things simple: one port to configure in K8s manifests, one health check URL.

---

## Structured Logging (`log/slog`)

### Why not `log.Printf`?

`log.Printf` produces plain text:
```
2026/03/01 12:00:00 [Worker 5] Trade | BTC-USD | Price: 64230.50 | Vol: 5
```
This is fine for reading in a terminal. But in K8s, logs are collected by systems like Loki, ELK, or CloudWatch. These systems need to *parse* your logs to make them searchable. Plain text requires regex parsing — fragile and slow.

### What `slog` gives you

`slog` with `JSONHandler` produces structured JSON:
```json
{"time":"2026-03-01T12:00:00Z","level":"INFO","msg":"routed trade","worker":5,"symbol":"BTC-USD","price":64230.5,"volume":5}
```
Every field is a key-value pair. Log aggregators can index them directly — no regex needed. In Grafana/Loki you can query: `{symbol="BTC-USD"}` or `{level="ERROR"}`.

### Key differences from `log`

| `log` (old) | `slog` (new) |
|-------------|-------------|
| `log.Printf("Worker %d: error: %v", id, err)` | `slog.Error("unmarshal failed", "worker", id, "error", err)` |
| Message and data mixed in format string | Message is separate, data is key-value pairs |
| `log.Fatalf` exits the process | `slog` has no Fatal — use `slog.Error` + `os.Exit(1)` |
| One output format (text) | Choose `TextHandler` (dev) or `JSONHandler` (production/K8s) |

### The setup pattern

```go
logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
slog.SetDefault(logger)
```

- `slog.New` creates a logger with a specific handler (JSON in our case)
- `slog.SetDefault` makes it the global logger — so `slog.Info(...)` anywhere in the code uses it
- `os.Stdout` because K8s captures container stdout automatically

### Log levels

| Level | When to use |
|-------|------------|
| `slog.Info` | Normal operations: server started, event routed, client connected |
| `slog.Warn` | Recoverable issues: failed to accept connection, retrying |
| `slog.Error` | Failures: unmarshal failed, config invalid, server can't start |

### Why K8s cares

K8s itself doesn't read your logs, but the ecosystem around it does. Prometheus handles metrics (numbers over time), while log aggregators handle events (what happened and when). Structured JSON logs make your app a good citizen in both systems.

---

## Resilience

### 1. Panic Recovery (`recover`)

**The problem:** If a worker goroutine panics (e.g. nil pointer, index out of range), it crashes silently. The `sync.WaitGroup` never decrements, so the server hangs on shutdown — `wg.Wait()` blocks forever.

**The solution:**
```go
defer func() {
    if r := recover(); r != nil {
        slog.Error("worker panic recovered", "worker", id, "panic", r)
    }
}()
```

`recover()` only works inside a `defer`. It catches the panic, logs it, and lets the goroutine exit cleanly. The worker is lost, but the server keeps running.

**Go rule:** `recover()` must be called *directly* inside a `defer` function. It does nothing if called from a regular function, even one called by a deferred function.

### 2. TCP Read Deadlines

**The problem:** Without a deadline, `frame.Read(conn)` blocks forever waiting for data. A slow or stalled client holds a goroutine and a connection slot indefinitely. 1000 stalled clients = 1000 stuck goroutines = no capacity for real traffic.

**The solution:**
```go
conn.SetReadDeadline(time.Now().Add(readTimeout))
```

This tells the OS: "if no data arrives within 30 seconds, return an error." The deadline resets on every loop iteration, so an active client that sends frames regularly never times out.

**Why `time.Now().Add()` and not a fixed time?** Deadlines in Go are absolute timestamps, not durations. You must recalculate `Now() + timeout` each time, or the deadline stays in the past after the first frame.

### 3. Connection Limit (Semaphore Pattern)

**The problem:** Every TCP connection spawns a goroutine. With no limit, a flood of connections exhausts file descriptors (the OS limit on open sockets, typically 1024 or 65535). Once exhausted, the server can't accept *any* new connections — including health checks.

**The solution:** A buffered channel as a counting semaphore:
```go
connSem := make(chan struct{}, cfg.MaxConns)  // e.g. 1000 slots

// Acquire a slot
select {
case connSem <- struct{}{}:
    go handleConnection(...)
default:
    conn.Close()  // at capacity, reject immediately
}

// Release a slot (inside handleConnection)
defer func() { <-connSem }()
```

**Why a channel and not a counter?** A channel naturally blocks when full. The `select/default` pattern lets you try non-blocking: if the channel is full, fall through to the `default` and reject. No mutex needed.

**Why `struct{}`?** An empty struct uses zero bytes of memory. The channel values carry no data — they're just tokens representing "one connection slot."

### 4. Backpressure Warning

**The problem:** The job channel can fill up silently. Once full, `jobs <- data` blocks, and the TCP handler stops reading — which eventually fills the OS TCP buffer and stalls the client. All of this happens with no visibility.

**The solution:** Check queue fullness before pushing and log a warning at 80%:
```go
if queueLen > queueCap*80/100 {
    slog.Warn("queue backpressure", "depth", queueLen, "capacity", queueCap)
}
```

This is an early warning system. In K8s, you'd set up a Prometheus alert on `aegis_queue_depth` to auto-scale before hitting 100%. The 80% threshold gives the HPA time to react and spin up new pods.

---

## Benchmark Accuracy

### The original problem

The first benchmark measured *client-side send speed* — how fast bytes go into the TCP socket. But the OS has a TCP send buffer (typically 128KB-4MB). The client can finish writing long before the server finishes processing. So "239k events/sec" was really "239k events/sec into the OS buffer," not true end-to-end throughput.

### The fix: query server-side metrics

The improved benchmark:
1. **Before sending:** Scrapes `aegis_events_processed_total` from `/metrics` to get a baseline count
2. **Sends all events:** Same as before — blast 100k frames
3. **Closes the connection:** Forces the server to read all buffered frames (no data left sitting in OS buffers)
4. **Polls `/metrics`:** Waits until the processed count reaches the expected total (or times out after 10s)
5. **Reports both rates:** Client send rate *and* server-side end-to-end rate

### Why close the connection before measuring?

TCP is buffered at multiple levels: the Go runtime, the OS send buffer, the network, the OS receive buffer, and the server's Go runtime. Closing the connection flushes the client side and tells the server "no more data" so `frame.Read` returns an error after the last frame. Without closing, frames could sit in buffers uncounted.

### Why poll instead of a callback?

The bench tool is a separate process from the server. It can't access the server's memory. But the Prometheus `/metrics` endpoint is already there — we reuse it instead of building a new communication channel. This is a common pattern: use the observability infrastructure you already have.

### Prometheus text format parsing

The `/metrics` endpoint returns plain text like:
```
aegis_events_processed_total{type="trade"} 100000
aegis_events_processed_total{type="log"} 0
```
The bench tool parses this with string splitting — simple but effective. In production, you'd use the Prometheus client library, but for a benchmark tool, basic parsing is fine.

---

## Phase 2: Kubernetes Integration

### Kubernetes Fundamentals

#### What is Kubernetes?

Kubernetes (K8s) is a system that manages containers across multiple machines. You tell it *what* you want (e.g., "run 3 copies of my app"), and it figures out *how* — which machine to put each copy on, how to restart them if they crash, how to route traffic to them.

Without K8s, you'd SSH into servers, manually start processes, monitor them yourself, and scramble when things fail. K8s automates all of this.

#### Why does K8s exist?

**The single-server problem:** Your Go binary runs great on one laptop. But in production:
- What if the server crashes? Your app is down.
- What if traffic spikes 10x? One server can't handle it.
- What if you need to deploy a new version? You'd have downtime.

**The many-servers problem:** You could run your binary on 10 servers, but then:
- How do you decide which server runs what?
- How do clients know which server to connect to?
- If server 3 crashes, who restarts the app and redirects traffic?

K8s solves both problems. You describe what you want, K8s makes it happen across any number of machines.

#### How K8s is structured

```
┌─────────────────────────────────────────┐
│              Control Plane              │
│  ┌───────────┐  ┌──────────────────┐    │
│  │ API Server│  │    Scheduler     │    │
│  │ (kubectl  │  │ (picks which     │    │
│  │  talks    │  │  node runs each  │    │
│  │  to this) │  │  pod)            │    │
│  └───────────┘  └──────────────────┘    │
│  ┌──────────────────────────────────┐   │
│  │  Controller Manager              │   │
│  │  (watches desired state vs       │   │
│  │   actual state, fixes gaps)      │   │
│  └──────────────────────────────────┘   │
│  ┌──────────────────────────────────┐   │
│  │  etcd (database storing all      │   │
│  │  cluster state as key-value)     │   │
│  └──────────────────────────────────┘   │
└─────────────────────────────────────────┘

┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│   Node 1     │  │   Node 2     │  │   Node 3     │
│  ┌────────┐  │  │  ┌────────┐  │  │  ┌────────┐  │
│  │ Pod A  │  │  │  │ Pod B  │  │  │  │ Pod D  │  │
│  │ Pod C  │  │  │  │ Pod E  │  │  │  │ Pod F  │  │
│  └────────┘  │  │  └────────┘  │  │  └────────┘  │
│  kubelet     │  │  kubelet     │  │  kubelet     │
│  (agent)     │  │  (agent)     │  │  (agent)     │
└──────────────┘  └──────────────┘  └──────────────┘
```

**Control Plane** — the brain. Decides what runs where.
**Nodes** — the workers. Machines that actually run your containers.
**kubelet** — an agent on each node that takes orders from the control plane and manages containers on that node.

In our k3s setup, everything runs on one machine (your laptop). In production, the control plane and nodes would be separate machines.

#### Core K8s objects

Everything in K8s is an "object" described by YAML. Here's how they relate:

```
Deployment (manages)
  └── ReplicaSet (created automatically, ensures N pods exist)
       └── Pod (one or more containers running together)
            └── Container (your Docker image running)

Service (routes traffic to)
  └── Pods (found by matching labels)

HPA (scales)
  └── Deployment (adjusts replica count)
```

#### Pod — the smallest unit

A Pod is one or more containers that share the same network and storage. In our case, each Pod runs one container (the aegis-stream binary).

**Why Pods, not just containers?** Sometimes related containers need to share resources. For example, a sidecar container that collects logs from the main app. They share `localhost` inside the Pod, so they communicate without networking overhead.

**Pods are ephemeral.** They can be killed and recreated at any time. They get new IP addresses on restart. This is why you never hard-code Pod IPs — you use Services instead.

**Pod lifecycle:**
```
Pending → ContainerCreating → Running → Terminating → (deleted)
```
- **Pending** — waiting for a node to be assigned
- **ContainerCreating** — pulling the image, starting the container
- **Running** — all containers are up and healthy
- **Terminating** — received SIGTERM, gracefully shutting down

#### Desired state vs actual state

This is the core idea of K8s. You declare what you want:
```yaml
replicas: 3    # "I want 3 pods"
```

K8s continuously compares:
- **Desired state:** 3 pods running
- **Actual state:** maybe only 2 pods are running (one crashed)
- **Action:** start 1 more pod

This loop runs constantly. You never say "start a pod" — you say "I want 3 pods" and K8s figures out the rest. This is called **declarative** configuration (describe the goal) vs **imperative** (describe the steps).

#### Namespaces — organizing your cluster

Namespaces are virtual clusters within your physical cluster. They isolate resources:

| Namespace | What's in it |
|-----------|-------------|
| `default` | Your app (aegis-stream) |
| `monitoring` | Prometheus, Grafana, prometheus-adapter |
| `kube-system` | K8s internal components (DNS, metrics-server) |

Resources in different namespaces can't see each other by default. That's why Grafana uses the full DNS name `prometheus-server.monitoring.svc.cluster.local` — it needs the namespace (`monitoring`) to cross the boundary.

#### Labels and selectors — how K8s connects things

Labels are key-value tags attached to any K8s object:
```yaml
labels:
  app: aegis-stream
```

Selectors find objects by their labels:
```yaml
selector:
  matchLabels:
    app: aegis-stream
```

This is how everything connects:
- **Deployment** finds its Pods by label
- **Service** finds its Pods by label
- **HPA** finds its Deployment by name

If the labels don't match, things silently fail — traffic doesn't route, scaling doesn't work. Label matching is the most common source of K8s configuration bugs.

#### Rolling updates — zero-downtime deploys

When you change a Deployment (new image, new env var), K8s doesn't kill all pods and restart them. It does a **rolling update**:

```
1. Start 1 new pod with new config
2. Wait for it to pass readiness probe
3. Kill 1 old pod
4. Repeat until all pods are updated
```

This means your app stays available during deploys. There's always at least one healthy pod serving traffic. This is why readiness probes matter — K8s won't kill old pods until new ones are confirmed healthy.

#### K8s networking model

Every Pod gets its own IP address. Pods can talk to each other directly by IP — no NAT, no port mapping. But Pod IPs change on restart, so you use Services for stable addresses.

```
Client (your laptop)
  │
  │ kubectl port-forward
  ▼
Service (stable DNS name, load balances)
  │
  ├──► Pod 1 (10.42.0.15)
  └──► Pod 2 (10.42.0.16)
```

**Inside the cluster:** Services get DNS names automatically. Any pod can reach `aegis-stream.default.svc.cluster.local:9000`.

**Outside the cluster:** Three options:
- `kubectl port-forward` — dev only, creates a tunnel
- `NodePort` — exposes a port on every node's IP
- `LoadBalancer` — creates a cloud load balancer (production)

#### Common kubectl commands

| Command | What it does |
|---------|-------------|
| `kubectl get pods` | List all pods in the current namespace |
| `kubectl get pods -w` | Watch pods in real time (updates live) |
| `kubectl get svc` | List all services |
| `kubectl get hpa` | List all autoscalers |
| `kubectl logs <pod>` | View a pod's stdout logs |
| `kubectl logs -l app=aegis-stream` | View logs for all pods with a label |
| `kubectl describe pod <pod>` | Detailed info + events (useful for debugging) |
| `kubectl apply -f file.yaml` | Create or update resources from a file |
| `kubectl delete -f file.yaml` | Delete resources defined in a file |
| `kubectl set env deployment/X KEY=VAL` | Change an env var (triggers rolling restart) |
| `kubectl port-forward svc/X 8080:80` | Tunnel local port to a service |
| `kubectl get pods -n monitoring` | List pods in a specific namespace |
| `kubectl rollout status deployment/X` | Wait for a rolling update to finish |

---

### Multi-stage Dockerfile

**Why multi-stage?** A Go build needs the full toolchain (~800MB). Your production container only needs the compiled binary (~12MB). Multi-stage builds use one image to compile and a second image to run:

```
Stage 1 (golang:alpine)  →  compile  →  /bin/server
Stage 2 (scratch)         →  copy /bin/server  →  final image (11.8MB)
```

**Why `scratch`?** It's a completely empty image — no OS, no shell, no package manager, nothing. The only files are what you explicitly `COPY` in. This gives you:
- **Tiny size** — only the binary and CA certs (11.8MB vs 800MB+)
- **Minimal attack surface** — no shell means attackers can't exec into the container
- **Fast pulls** — K8s nodes download small images much faster during scale-up

**Why `CGO_ENABLED=0`?** Go can call C libraries via CGo. But C code links dynamically to `libc`, which doesn't exist in `scratch`. Disabling CGo produces a statically linked binary that contains everything it needs — no external dependencies.

**Why `-ldflags="-s -w"`?**
- `-s` strips the symbol table (function names, used for debugging)
- `-w` strips DWARF debug info
- Together they reduce binary size by ~30% with no runtime impact

**Why copy `go.mod` before source code?** Docker caches each layer. If `go.mod` hasn't changed, `go mod download` is skipped entirely on rebuild. This means changing a `.go` file only triggers the compile step, not the dependency download. Saves minutes on iterative builds.

**`EXPOSE 9000 2112`** doesn't actually publish ports — it's documentation. K8s reads these as hints when writing Service manifests, but the actual port mapping is done in the Deployment YAML.

**`ENTRYPOINT ["/server"]`** uses the exec form (JSON array), not the shell form (`ENTRYPOINT /server`). The exec form runs the binary directly as PID 1, which means it receives SIGTERM from K8s directly — critical for our graceful shutdown to work.

---

### k3s Setup

**What is k3s?** A lightweight Kubernetes distribution by Rancher. It's a single binary (~50MB) that runs the full K8s API. Ideal for local development and learning — you get real K8s behavior without the overhead of minikube or kind.

**k3s vs Docker:** k3s uses containerd (not Docker) as its container runtime. Docker images aren't visible to k3s by default. To get images in:
```bash
docker save aegis-stream:latest -o /tmp/aegis-stream.tar
sudo k3s ctr images import /tmp/aegis-stream.tar
```

**kubeconfig:** K8s tools (`kubectl`) need a config file to know where the cluster is. k3s writes it to `/etc/rancher/k3s/k3s.yaml`. Copying it to `~/.kube/config` lets you run `kubectl` without `sudo`.

---

### Deployment

**What it does:** A Deployment tells K8s "run N copies of this container and keep them alive." If a pod crashes, K8s replaces it. If a node dies, K8s reschedules.

**Key fields:**

| Field | Purpose |
|-------|---------|
| `replicas: 2` | How many identical pods to run |
| `selector.matchLabels` | How the Deployment finds its pods (by label) |
| `template` | Blueprint for each pod — every pod is a copy of this |
| `imagePullPolicy: Never` | Use local image, don't pull from Docker Hub |
| `env` | Config via environment variables (read by `internal/config`) |

**Probes — how K8s monitors your app:**

| Probe | Question | On failure |
|-------|----------|-----------|
| `livenessProbe` | "Is the process stuck?" | K8s kills and restarts the pod |
| `readinessProbe` | "Can it accept traffic?" | K8s removes pod from Service (no traffic) |

Both hit our `/healthz` endpoint. During graceful shutdown, `/healthz` returns 503, so the readiness probe fails and K8s stops sending traffic *before* the pod actually dies.

**`initialDelaySeconds`:** How long to wait after the container starts before the first probe. Gives the server time to bind ports and spawn workers.

---

### Service

**The problem:** Pods are ephemeral — they get new IP addresses every time they restart. You can't hard-code a pod IP.

**The solution:** A Service provides a stable DNS name (`aegis-stream.default.svc.cluster.local`) that always routes to healthy pods. It load-balances across all pods that match its `selector`.

**How it finds pods:** The Service's `selector: app: aegis-stream` matches the Deployment's `template.metadata.labels.app: aegis-stream`. Any pod with that label automatically gets traffic.

**Service types:**

| Type | Reachable from | Use case |
|------|---------------|----------|
| `ClusterIP` (default) | Inside the cluster only | Internal services |
| `NodePort` | Outside via node IP + port | Quick external access |
| `LoadBalancer` | Outside via cloud load balancer | Production |

We use `ClusterIP` because Aegis Stream is an internal data router. For local testing, `kubectl port-forward` tunnels traffic from your laptop into the cluster.

**`port` vs `targetPort`:**
- `port: 9000` — what other services in the cluster connect to
- `targetPort: tcp-data` — forwarded to the container's named port (9000)

Using named ports (`tcp-data`, `http-metrics`) instead of numbers means you can change the container port without updating the Service.

---

### kubectl port-forward

**What it does:** Creates a tunnel from `localhost` on your machine to a Service (or pod) inside the cluster. No external networking needed.

```bash
kubectl port-forward svc/aegis-stream 9000:9000 2112:2112
```

This maps:
- `localhost:9000` → Service port 9000 → pod port 9000 (TCP data)
- `localhost:2112` → Service port 2112 → pod port 2112 (metrics/healthz)

**Not for production.** Port-forward is a dev tool. In production, you'd use a LoadBalancer Service or an Ingress controller.

---

### Helm

**What is Helm?** The package manager for Kubernetes — like `apt` for Ubuntu or `brew` for macOS, but for your cluster. Instead of writing dozens of YAML files for complex tools (Prometheus, Grafana, databases), you install them with one command.

**Key concepts:**

| Concept | What it is |
|---------|-----------|
| **Chart** | A package of K8s manifests (like a `.deb` or `.rpm` file) |
| **Repository** | Where charts are hosted (like an apt repo) |
| **Release** | A running instance of a chart in your cluster |
| **Values** | Config overrides you pass to customize a chart |

**Why use Helm?** Installing Prometheus manually requires ~15 YAML files (Deployment, Service, ConfigMap, RBAC, ServiceAccount, etc.). A Helm chart packages all of them into one installable unit:
```bash
helm install prometheus prometheus-community/prometheus
```

This creates all the resources, wired together correctly. You can customize with `--set` flags or a `values.yaml` file.

**Helm vs writing your own YAML:** Use Helm for third-party tools (Prometheus, Grafana, databases). Write your own YAML for your app (aegis-stream) — because you need to understand every line of your own deployment.

---

### Prometheus Scraping in K8s

**How Prometheus discovers pods:** Prometheus watches the K8s API for pods with specific annotations:
```yaml
annotations:
  prometheus.io/scrape: "true"     # "yes, scrape me"
  prometheus.io/port: "2112"       # which port
  prometheus.io/path: "/metrics"   # which path
```

Prometheus sees these annotations, connects to the pod's IP on port 2112, and pulls metrics every 15 seconds. No configuration file needed — it's fully automatic.

**Labels vs Annotations:**
- **Labels** are for K8s itself — selectors, Services, Deployments use them to find resources
- **Annotations** are for external tools — Prometheus, CI/CD systems, monitoring. K8s stores them but doesn't act on them

---

### prometheus-adapter

**The problem:** HPA speaks the K8s custom metrics API. Prometheus speaks PromQL. They don't understand each other.

**The solution:** prometheus-adapter sits in between and translates:
```
HPA → "what's aegis_queue_depth for pod X?"
  → adapter queries Prometheus with PromQL
  → adapter returns the value in K8s API format
  → HPA makes scaling decisions
```

**The rules config** tells the adapter which Prometheus metrics to expose:
```yaml
seriesQuery: 'aegis_queue_depth{namespace!="",pod!=""}'
```
This means: "find the `aegis_queue_depth` metric, and expose it per-pod so HPA can read it."

---

### HorizontalPodAutoscaler (HPA)

**What it does:** Watches a metric, compares it to a target, and adjusts the number of replicas in a Deployment.

**The scaling formula:**
```
desiredReplicas = currentReplicas × (currentMetricValue / targetMetricValue)
```
Example: 2 pods, queue depth 3000, target 1000:
```
desiredReplicas = 2 × (3000 / 1000) = 6 pods
```

**Key fields:**

| Field | Purpose |
|-------|---------|
| `minReplicas: 2` | Floor — never scale below this (ensures redundancy) |
| `maxReplicas: 10` | Ceiling — never scale above this (cost control) |
| `averageValue: "1000"` | Target queue depth per pod — HPA aims for this |

**Scale up vs scale down behavior:**
- **Scale up fast** — bursts are time-sensitive (30s stabilization, add 2 pods at a time)
- **Scale down slowly** — avoid flapping: scaling down then immediately back up wastes resources (120s stabilization, remove 1 pod at a time)

**Stabilization window:** HPA waits this long before acting. If queue depth spikes for 5 seconds then drops, the 30s window prevents unnecessary scale-up. Only sustained load triggers scaling.

**The full data flow:**
```
aegis-stream pod → /metrics (aegis_queue_depth)
  → Prometheus scrapes every 15s
  → prometheus-adapter translates to K8s API
  → HPA reads every 15s
  → HPA adjusts Deployment replicas
  → K8s creates/deletes pods
```

**Milli-units in K8s metrics:** When HPA shows `9835800m/1k`, the `m` means milli. So `9835800m` = 9835.8. K8s uses milli-units internally for precision. `1k` = 1000 (the target).

---

### Stress Testing Observations

**What we tested:** 5 concurrent bench clients blasting 100k events each, with workers reduced to 1 to create backpressure.

**What we observed:**
```
Replicas: 2 → 4 → 6 → 8 → 10 (hit maxReplicas)
Queue depth peaked at ~50k, then slowly drained
Scale-up took ~3 minutes (30s stabilization + 2 pods every 60s)
Scale-down takes ~10 minutes (120s stabilization + 1 pod every 60s)
```

**Key lessons learned:**

1. **Prometheus has a 15s scrape delay.** HPA doesn't see real-time queue depth — it sees a 15-30 second old snapshot. This is why stabilization windows exist.

2. **Scale-up is intentionally faster than scale-down.** Bursts need fast reaction (add capacity). But scaling down too fast causes *flapping* — removing pods then immediately needing them back. The asymmetry (30s up / 120s down) prevents this.

3. **maxReplicas is a safety net, not a guarantee.** Once you hit 10 pods, HPA stops scaling even if load keeps rising. The queue fills up, TCP handlers block, and clients get errors. In production, set alerts on queue depth to catch this before users are affected.

4. **Worker count matters as much as pod count.** Reducing from 50 to 1 worker created massive backpressure. Scaling pods helps, but tuning workers per pod is equally important.

5. **`kubectl set env` triggers a rolling restart.** Changing an env var replaces all pods — useful for live config changes, but breaks active connections and port-forwards.

---

### Grafana Dashboard

**What is Grafana?** A visualization tool that connects to data sources (like Prometheus) and displays metrics as charts, graphs, and stats. Prometheus stores the data, Grafana makes it human-readable.

**Connecting Grafana to Prometheus:** Grafana needs a data source URL. Since both run inside the cluster, Grafana uses the K8s internal DNS name:
```
http://prometheus-server.monitoring.svc.cluster.local:80
```
This is the Service DNS pattern: `<service-name>.<namespace>.svc.cluster.local`.

**Dashboard panels and their PromQL queries:**

| Panel | Query | What it tells you |
|-------|-------|-------------------|
| Events/sec | `rate(aegis_events_processed_total[1m])` | Current throughput — is the system under load? |
| Queue Depth | `sum(aegis_queue_depth)` | Backpressure — are workers keeping up? |
| Active Connections | `sum(aegis_active_connections)` | How many clients are connected right now? |
| Error Rate | `rate(aegis_event_errors_total[1m])` | Are events failing to deserialize? |
| Latency p95 | `histogram_quantile(0.95, ...)` | 95% of events are processed within this time |
| Pod Replicas | `count(aegis_queue_depth)` | How many pods are running (each pod reports this metric) |
| Total Events | `sum(aegis_events_processed_total)` | Cumulative count since pods started |

**Key PromQL functions:**
- `rate(counter[1m])` — converts a counter (only goes up) into a per-second rate over the last 1 minute
- `sum()` — adds values across all pods
- `sum() by (label)` — groups the sum by a label (e.g., by event type)
- `histogram_quantile(0.95, ...)` — calculates the 95th percentile from histogram buckets

**Dashboard as code:** The dashboard is stored as a JSON file (`k8s/grafana-dashboard.json`). This means it's version-controlled and reproducible — anyone can import it into a fresh Grafana instance.

---

## Phase 3: Kubernetes Operator

### What Is an Operator?

**The problem:** In Phase 2, we created K8s resources by hand — writing `deployment.yaml`, `service.yaml`, `hpa.yaml`, and applying them with `kubectl apply`. If someone wants to deploy a second aegis-stream pipeline with different settings, they'd have to duplicate and edit all those YAML files. This doesn't scale.

**The solution:** An Operator is a program that runs inside K8s and automates the management of a complex application. Instead of users managing Deployments + Services + HPAs separately, they write **one** custom YAML describing what they want:

```yaml
apiVersion: stream.aegis.io/v1alpha1
kind: AegisPipeline
metadata:
  name: aegis-demo
spec:
  replicas: 2
  workers: 50
  queueDepth: 100000
```

The Operator reads this and creates all the low-level resources automatically. Think of it as: **the user describes the "what", the Operator handles the "how".**

**Real-world analogy:** A thermostat is an operator. You set the desired temperature (desired state). The thermostat constantly checks the actual temperature (actual state) and turns the heater on/off to close the gap. You don't manually control the heater — the thermostat operates it for you.

---

### CRD (Custom Resource Definition)

**What:** A CRD extends the K8s API with a new resource type. Once you install our CRD, K8s understands `AegisPipeline` the same way it understands `Deployment` or `Service`. You can use `kubectl get aegispipelines`, `kubectl describe`, `kubectl delete`, etc.

**Two parts to a CRD:**

1. **The CRD YAML** (`config/crd/bases/stream.aegis.io_aegispipelines.yaml`) — tells K8s the schema: what fields exist, their types, validation rules. This is generated automatically from the Go types by `controller-gen`.

2. **The Go types** (`api/v1alpha1/aegispipeline_types.go`) — the source of truth. You define your spec as a Go struct and use kubebuilder marker comments for validation:

```go
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:Maximum=20
// +kubebuilder:default=2
Replicas int32 `json:"replicas"`
```

These markers generate OpenAPI validation in the CRD YAML. K8s will reject a CR with `replicas: 0` or `replicas: 25` before the operator even sees it.

**Spec vs Status pattern:**
- **Spec** = desired state (what the user writes)
- **Status** = observed state (what the operator writes back)

This separation is fundamental in K8s. Users control Spec, controllers control Status. They never cross.

**`json:` struct tags** — K8s stores everything as JSON in etcd. The struct tags control the JSON field names. `omitempty` means "don't include this field if it's zero/nil" (saves storage). `omitzero` is a newer variant that also omits zero-value structs.

---

### Kubebuilder

**What:** A CLI tool that generates the boilerplate for building operators. Writing an operator from scratch requires ~2000 lines of setup code (API registration, scheme builders, manager configuration, RBAC, Dockerfiles, Makefiles). Kubebuilder generates all of this so you can focus on the **reconciliation logic** — the part that actually matters.

**Key commands we used:**
```bash
# Scaffold the project — creates go.mod, Makefile, main.go, Dockerfile, etc.
kubebuilder init --domain aegis.io --repo aegis-stream/operator

# Create a new API (CRD types) and controller
kubebuilder create api --group stream --version v1alpha1 --kind AegisPipeline
```

**What `--group`, `--version`, `--kind` mean:**
- **Group** (`stream`) — a namespace for your API, like a Java package. Combined with domain: `stream.aegis.io`
- **Version** (`v1alpha1`) — API version. `v1alpha1` means "experimental, may change". Progresses to `v1beta1` then `v1` as it stabilizes
- **Kind** (`AegisPipeline`) — the resource type name. This becomes what you use in `kind:` in YAML

Together they form the full API: `stream.aegis.io/v1alpha1/AegisPipeline`

**Generated code we regenerate with `make`:**
- `make generate` — runs `controller-gen` to regenerate `zz_generated.deepcopy.go`. Every K8s object must implement `DeepCopyObject()` so the API server can safely copy objects. This is pure boilerplate — the tool generates it from your struct definitions.
- `make manifests` — runs `controller-gen` to generate CRD YAML (from struct tags + markers) and RBAC ClusterRole YAML (from `+kubebuilder:rbac` comments).

---

### The Reconciliation Loop

**This is the core concept of K8s operators.** The Reconcile function is called every time something relevant changes — a CR is created, updated, or deleted, or a child resource (Deployment, Service) changes.

**Level-triggered vs edge-triggered:**
- **Edge-triggered** = "react when something happens" (event-driven). Problem: if you miss an event, you're out of sync forever.
- **Level-triggered** = "compare desired vs actual state and fix the gap." Even if you miss 10 events, the next reconciliation still converges to the right state.

K8s operators are level-triggered. The Reconcile function doesn't ask "what event happened?" — it asks "what should exist, and does it?"

**Our Reconcile flow (5 steps):**

```
User applies AegisPipeline CR
         │
         ▼
Step 1: Fetch the CR
         │ (if deleted → return, garbage collection handles cleanup)
         ▼
Step 2: Build desired Deployment from CR spec
         │
         ▼
Step 3: CreateOrUpdate Deployment
         │ (idempotent: create if missing, update if different, skip if same)
         ▼
Step 4: CreateOrUpdate Service
         │ (same pattern, preserves immutable ClusterIP)
         ▼
Step 5: Update CR status (readyReplicas, phase)
```

**`CreateOrUpdate` pattern:**
This is the idempotent reconciliation primitive. It:
1. Tries to get the existing resource
2. If not found → creates it
3. If found → calls your mutate function, then updates if changed

The mutate function is where you set the fields you care about. You don't build the entire object from scratch because K8s adds its own fields (resourceVersion, uid, status, etc.) that you must preserve.

---

### Owner References and Garbage Collection

**The problem:** If a user deletes their AegisPipeline CR, the Deployment and Service it created should also be deleted. Without automation, these would become orphans — running pods with no parent to manage them.

**The solution:** When the controller creates a Deployment, it sets an `ownerReference` pointing back to the AegisPipeline CR:

```go
controllerutil.SetControllerReference(&pipeline, &deploy, r.Scheme)
```

This tells K8s: "this Deployment is owned by this AegisPipeline." When the owner is deleted, K8s automatically deletes all owned resources. This is called **cascading deletion** — same concept as `ON DELETE CASCADE` in SQL foreign keys.

**Why `SetControllerReference` not `SetOwnerReference`?** The "controller" variant also marks this owner as the *managing* controller, which means:
- Only one controller can be the managing owner (prevents conflicts)
- The controller gets watch events when owned resources change

---

### RBAC Markers

The controller needs K8s API permissions to create Deployments and Services. These are declared via comment markers:

```go
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
```

`make manifests` reads these comments and generates a ClusterRole YAML. Key details:
- `groups=apps` — Deployments live in the `apps` API group
- `groups=""` — Services live in the core API group (empty string)
- `verbs` — the actions the operator is allowed to take

Without these, the operator would get "Forbidden" errors when trying to create resources. K8s denies by default — you must explicitly grant every permission.

---

### Owns() — Watching Child Resources

In `SetupWithManager`, we declare what the controller watches:

```go
ctrl.NewControllerManagedBy(mgr).
    For(&streamv1alpha1.AegisPipeline{}).  // watch CRs
    Owns(&appsv1.Deployment{}).             // watch owned Deployments
    Owns(&corev1.Service{}).                // watch owned Services
    Complete(r)
```

- `For()` — primary watch. Any change to an AegisPipeline CR triggers Reconcile.
- `Owns()` — secondary watch. If someone manually deletes a Deployment that our operator created, the controller detects it and recreates it. This is the **self-healing** property of operators.

The Owns() watch works because of ownerReferences — K8s can look up the owner CR from the Deployment's metadata and trigger Reconcile for that specific CR.

---

### React Dashboard (Custom UI)

**Why not just Grafana?** Grafana is excellent for ops monitoring, but it has limitations for a learning project:
1. It can't read K8s custom resources — so we can't show CRD fields like `costPerPodHour`
2. You can't customize the layout or add computed fields (like cost estimation)
3. Building a full-stack dashboard teaches React, TypeScript, and API design

**Architecture:**
```
Browser (React) → Nginx → Go API → Prometheus + K8s API
```

The Go backend queries two data sources:
- **Prometheus** instant query API (`/api/v1/query`) for metrics (same PromQL as Grafana)
- **K8s dynamic client** for the AegisPipeline CR (spec + status fields)

**Key concepts learned:**

**Vite** — A fast frontend build tool. Unlike Create React App (which bundles with Webpack), Vite serves source files directly during development using native ES modules. This means near-instant hot reloads. For production, it uses Rollup to bundle optimized output.

**Tailwind CSS** — A utility-first CSS framework. Instead of writing `.card { background: gray; padding: 1rem; }`, you write `className="bg-gray-800 p-4"` directly in JSX. Pros: no CSS files to manage, consistent design tokens. Cons: long class strings. For a dashboard with repetitive card layouts, it's very productive.

**Recharts** — A React charting library built on D3 and SVG. Components like `<LineChart>`, `<AreaChart>`, and `<Tooltip>` compose together declaratively. Each chart receives data as a prop and handles rendering, scaling, and interaction internally.

**React Hooks (useState, useEffect, useCallback, useRef):**
- `useState` — holds data that triggers re-renders when changed (metrics, pipeline info)
- `useEffect` — runs side effects like setting up polling intervals. The cleanup function (`return () => clearInterval(id)`) prevents memory leaks when the component unmounts
- `useCallback` — memoizes the fetch function so it doesn't get recreated on every render. Without this, the `useEffect` dependency array would trigger unnecessary re-subscriptions
- `useRef` — holds the history buffer without triggering re-renders. We use state for the final history (to trigger renders) but ref for the mutable working copy (to avoid stale closures)

**Polling pattern:**
```typescript
useEffect(() => {
  fetchMetrics();                        // immediate first fetch
  const id = setInterval(fetchMetrics, 5000);  // then every 5s
  return () => clearInterval(id);        // cleanup on unmount
}, [fetchMetrics]);
```

This is simpler than WebSockets for our use case. The 5-second interval matches Prometheus's scrape rate, so faster polling wouldn't give fresher data.

**History buffer:** A fixed-size array of 24 snapshots (2 minutes at 5s intervals) that Recharts renders as time-series charts. New snapshots are appended with `[...history, newSnapshot].slice(-24)`, which keeps only the last 24 entries.

**Cost calculation:** The CRD's `costPerPodHour` field enables real-time cost estimation:
```
cost/sec = costPerPodHour × readyReplicas / 3600
```
This updates automatically as pods scale up/down, showing the direct cost impact of autoscaling.

**Sidecar container pattern:** The dashboard runs as two containers in one Pod — Nginx (serves React, proxies `/api/*`) and Go (queries Prometheus + K8s). They share `localhost` inside the Pod, so Nginx proxies to `localhost:8080` without network overhead.

---

### Spec-to-Env-Var Mapping

The operator bridges the gap between the CRD and our aegis-stream server binary. The server reads config from environment variables (via `internal/config`). The operator translates CRD fields to env vars:

| CRD Field | Env Var | What it controls |
|-----------|---------|------------------|
| `spec.replicas` | (Deployment replicas) | Number of pods |
| `spec.workers` | `AEGIS_WORKERS` | Worker goroutines per pod |
| `spec.queueDepth` | `AEGIS_QUEUE_DEPTH` | Buffered channel capacity |
| `spec.port` | `AEGIS_PORT` | TCP listen port |
| `spec.metricsPort` | `AEGIS_METRICS_PORT` | Prometheus/health port |
| `spec.image` | (Container image) | Which container to run |

This means the same server binary we built in Phase 1 runs unchanged. The operator just configures it differently per CR.

---

## Phase 4: Real Data Demo

### Live Market Data Feeder (`cmd/feed`)

**The problem:** Up to Phase 3, all testing used synthetic benchmarks — identical payloads, single connection. This proves throughput but doesn't demonstrate real-world value.

**The solution:** Connect to a real crypto exchange and route live trades through the pipeline.

**Binance WebSocket API:**
- Public aggregate trade streams — no API key needed
- Combined stream URL: `wss://stream.binance.com:9443/stream?streams=btcusdt@aggTrade/ethusdt@aggTrade/solusdt@aggTrade`
- Delivers JSON messages like: `{"s":"BTCUSDT","p":"42500.50","q":"2.5","E":1672515782136}`

**Data flow:**
```
Binance WSS → JSON → parse → pb.Trade → proto.Marshal → frame.Write → TCP :9000
```

**Key patterns:**
- **Reconnect with exponential backoff:** On disconnect, wait 1s → 2s → 4s → max 30s before retrying. Reset delay on successful connection. This prevents hammering a down server.
- **Mixed payloads:** Every N trades, also send a synthetic `pb.Log` event. Demonstrates the `oneof` pattern from Phase 1 with real data.
- **Graceful shutdown:** Same `os/signal` + `context.WithCancel` pattern as the server. Cancel context → close WebSocket → close TCP → print final stats.

---

### Stress Test Tool (`cmd/stress`)

**The problem:** The bench tool sends from one connection with identical payloads. Real production has many clients sending varied data with traffic spikes.

**The solution:** Multi-connection stress test with two phases:
```
Time 0s                         30s                    40s
├─────── Sustained ──────────────┤──── Spike ───────────┤
  10 conns × 500/s = 5,000/s      10 conns × 2,500/s = 25,000/s
```

**Payload strategy:**
- Pre-generate ~100 varied payloads before the timed test (no allocation during send)
- 70% Trade (random symbols, realistic prices), 30% Log (random levels, varied messages)
- Each payload pre-marshaled and pre-framed into `[]byte` — zero allocation in the hot loop

**Rate limiting per connection:**
```go
perConnRate := rate / numConns  // distribute evenly
ticker := time.NewTicker(time.Second / time.Duration(perConnRate))
```
Each connection gets its own ticker for pacing. Round-robin through the payload pool.

**Server-side verification:** Same pattern as bench — scrape `/metrics` before and after, compare `aegis_events_processed_total` to calculate actual server-side throughput and loss percentage.

---

### AEGIS_PROCESS_DELAY — Simulating Real Workloads

**The problem:** With 50 workers and sub-microsecond processing, the server processes events faster than any stress test can send them. The queue never fills, so HPA never sees high `aegis_queue_depth` and never scales up.

**The solution:** A configurable delay added to each worker:
```go
if cfg.ProcessDelay > 0 {
    time.Sleep(cfg.ProcessDelay)
}
```

With `AEGIS_PROCESS_DELAY=10ms` and 5 workers, max throughput per pod ≈ 500 events/sec. This makes the queue fill up under load, triggering HPA scaling.

**Why this matters:** In a real system, workers would do I/O (database writes, API calls, network hops) that takes milliseconds. The delay simulates this. Without it, the demo only proves "Go is fast at in-memory work" — not that the autoscaling architecture works.

---

## Phase 5: Sink Interface + PostgreSQL

### The Sink Interface Pattern

**The problem:** In Phases 1-4, workers logged events to stdout via `slog.Info`. This proves the pipeline works but doesn't *store* data. To be useful, events need to go somewhere — a database, a message queue, a file.

**The solution:** Define an interface that abstracts the destination:
```go
type Sink interface {
    Write(event *pb.Event) error
    Close() error
}
```

Any destination just implements these two methods. The worker doesn't know or care where events end up:
```go
func worker(s sink.Sink, jobs <-chan Job) {
    for job := range jobs {
        event := unmarshal(job.Data)
        s.Write(event)  // could be stdout, PostgreSQL, Kafka, etc.
    }
}
```

**Why interfaces matter in Go:** Go interfaces are implicit — you don't declare "implements Sink." If a struct has `Write(*pb.Event) error` and `Close() error`, it *is* a Sink. This means you can add new sink types (Kafka, S3, etc.) without changing existing code.

**Current implementations:**
- `StdoutSink` — extracts the original `slog.Info` logging (default)
- `PostgresSink` — batch inserts into PostgreSQL

---

### PostgreSQL Sink — Batch Inserts

**The problem:** Inserting one row per event at 5,000 events/sec means 5,000 SQL queries per second. Each query has network round-trip overhead (even on localhost: ~0.5ms). At scale, the database becomes the bottleneck.

**The solution:** Buffer events and insert many rows in one SQL statement:

```go
type PostgresSink struct {
    pool       *pgxpool.Pool
    mu         sync.Mutex
    trades     []tradeRow    // buffer
    logs       []logRow      // buffer
}
```

**Flush triggers:**
1. **Buffer full** (100 events) — high throughput: events flush immediately when the buffer fills
2. **Timer** (500ms) — low throughput: a background goroutine flushes periodically so events don't sit in the buffer forever

**Why both triggers?** With only a buffer-size trigger, during low traffic (e.g., 1 event/sec), events would wait in the buffer for 100 seconds before being written. The timer ensures they're visible within 500ms.

**Transaction-based flush:**
```go
tx, _ := pool.Begin(ctx)
for _, t := range trades {
    tx.Exec(ctx, "INSERT INTO trades ...", t.symbol, t.price, ...)
}
tx.Commit(ctx)
```

All rows in a batch succeed or fail together. No partial writes.

**Connection pooling (`pgxpool`):**
The sink uses `pgxpool.Pool`, not a single `pgx.Conn`. A pool maintains multiple connections and hands them out to concurrent callers. This matters because:
- Multiple goroutines may flush simultaneously
- Database connections are expensive to create (TCP handshake + auth + TLS)
- The pool reuses connections, amortizing that cost

**Close() flushes remaining events:**
```go
func (p *PostgresSink) Close() error {
    p.mu.Lock()
    defer p.mu.Unlock()
    // Flush any buffered events before closing
    p.flush()
    p.pool.Close()
}
```
On graceful shutdown, the server closes the sink *after* all workers finish. This ensures no events are lost in the buffer.

---

### PostgreSQL Schema

```sql
CREATE TABLE trades (
    id        SERIAL PRIMARY KEY,
    event_id  TEXT NOT NULL,
    symbol    TEXT NOT NULL,
    price     DOUBLE PRECISION NOT NULL,
    volume    INTEGER NOT NULL,
    event_ts  BIGINT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE logs (
    id        SERIAL PRIMARY KEY,
    event_id  TEXT NOT NULL,
    level     TEXT NOT NULL,
    message   TEXT NOT NULL,
    service   TEXT NOT NULL,
    event_ts  BIGINT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);
```

**Key design decisions:**
- `event_id` — the protobuf event ID, for deduplication and tracing
- `event_ts` — the original event timestamp (nanoseconds), separate from `created_at` (when the row was inserted). The gap between these shows pipeline latency.
- `SERIAL PRIMARY KEY` — auto-incrementing integer. Simple and fast for append-heavy workloads.
- `TIMESTAMPTZ` — timestamp with timezone. Always store timestamps with timezone info to avoid ambiguity.

---

### PostgreSQL in Kubernetes

**StatefulSet vs Deployment:**

| | Deployment | StatefulSet |
|---|---|---|
| **Pod identity** | Random names (`aegis-stream-7b4d9`)  | Stable names (`aegis-postgres-0`) |
| **Storage** | No persistent volumes by default | PersistentVolumeClaim per pod |
| **Ordering** | All pods start/stop simultaneously | Start in order: 0, 1, 2... |
| **Use case** | Stateless apps (our aegis-stream) | Databases, queues (need stable storage) |

PostgreSQL needs StatefulSet because:
1. Data must survive pod restarts (PVC keeps data on disk)
2. Each replica needs its own volume (not shared)
3. Stable network identity for replication (if we add replicas later)

**The manifests:**

**Secret** (`k8s/postgres/secret.yaml`):
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aegis-postgres
stringData:
  POSTGRES_USER: aegis
  POSTGRES_PASSWORD: aegis
  POSTGRES_DB: aegis
```
The `postgres:17` Docker image reads these env vars on first boot to create the user and database. `stringData` accepts plain text — K8s base64-encodes it automatically.

**StatefulSet** (`k8s/postgres/statefulset.yaml`):
Uses `envFrom: secretRef` to inject all Secret keys as env vars. The `volumeClaimTemplates` section tells K8s to create a 1Gi PersistentVolumeClaim for each pod. Even if the pod is deleted, the PVC (and your data) remains.

**Headless Service** (`k8s/postgres/service.yaml`):
```yaml
clusterIP: None  # headless
```
A headless Service creates DNS records directly to pod IPs instead of load-balancing. For a single-replica database, the DNS name `aegis-postgres` resolves directly to the pod. This is standard for StatefulSets.

---

### Pod-to-Pod Communication in Kubernetes

**How aegis-stream connects to PostgreSQL in k3s:**

The connection string uses the **K8s Service DNS name**:
```
postgres://aegis:aegis@aegis-postgres:5432/aegis
```

This works because:
1. The PostgreSQL Service is named `aegis-postgres`
2. Both are in the `default` namespace
3. K8s DNS automatically resolves `aegis-postgres` to the pod IP

**Full DNS hierarchy:**
```
aegis-postgres                                    ← same namespace (shorthand)
aegis-postgres.default                            ← explicit namespace
aegis-postgres.default.svc                        ← explicit service
aegis-postgres.default.svc.cluster.local          ← fully qualified
```

All four resolve to the same IP. Within the same namespace, the short name works.

**Comparison with Docker Compose:**
| | Docker Compose | Kubernetes |
|---|---|---|
| **Define services** | `docker-compose.yml` | Multiple YAML files (`deployment.yaml`, `service.yaml`) |
| **Networking** | Automatic, by service name | Service DNS, must create Service objects |
| **Start all** | `docker-compose up` | `kubectl apply -f k8s/` |
| **Scaling** | `docker-compose up --scale app=3` | HPA or `kubectl scale` |
| **Persistence** | `volumes:` in compose file | PersistentVolumeClaim in StatefulSet |
| **Port access** | `ports: "5432:5432"` | `kubectl port-forward` (dev) or LoadBalancer (prod) |

K8s is more complex but gives you auto-scaling, self-healing, rolling updates, and multi-node deployment that Compose doesn't handle.

---

### `kubectl port-forward` — Tunneling Into the Cluster

**What it does:** Creates a TCP tunnel from your local machine to a pod or service inside the cluster.

```bash
kubectl port-forward svc/aegis-stream 9000:9000 2112:2112
```

**Visual:**
```
Your laptop                          k3s cluster
┌─────────────┐                     ┌───────────────────┐
│ localhost    │     tunnel          │ aegis-stream svc  │
│   :9000 ────┼─────────────────────┼──► :9000 (TCP)    │
│   :2112 ────┼─────────────────────┼──► :2112 (metrics)│
└─────────────┘                     └───────────────────┘
```

**Key behaviors:**
- Only works while the `kubectl port-forward` process is running — kill it and the tunnel closes
- If the target pod restarts (e.g., rolling update), the tunnel breaks and you must restart it
- Multiple ports can be forwarded in one command: `9000:9000 2112:2112`
- The left side is your local port, the right side is the service/pod port

**Why not for production?** Port-forward is a debugging tool:
- Single connection, no load balancing
- Breaks on pod restarts
- Requires `kubectl` access (not suitable for end users)

In production, you'd use a `LoadBalancer` Service (cloud provider gives you a public IP) or an `Ingress` controller (routes HTTP traffic by hostname/path).

---

### Prometheus `rate()` Window Sizing

**The problem:** Our dashboard showed flat lines despite active traffic. The rate queries used `[1m]` windows, but Prometheus scrapes every 60 seconds.

**Why it matters:** `rate()` needs at least 2 data points within the window to calculate a per-second rate. With a 60s scrape interval and a `[1m]` window, you get at most 1-2 points — often not enough.

**The fix:** Use `[5m]` windows:
```
rate(aegis_events_processed_total[5m])    ← 5 data points in window
```

**Rule of thumb:** Set the rate window to at least 4× the scrape interval. With 60s scrapes, use `[4m]` or `[5m]`. This gives enough data points for a smooth rate calculation.

---

### NaN Handling in Go's JSON Encoder

**The problem:** The dashboard API returned empty responses (Content-Length: 0) even though metrics were fetched successfully.

**Root cause:** Prometheus returns `NaN` for `histogram_quantile()` when there's no data (no events = no histogram buckets). Go's `json.Encoder` cannot serialize `NaN` or `Inf` float values — the JSON spec doesn't support them. Instead of returning an error, the encoder silently produces incomplete output.

**The fix:**
```go
func parseValue(v [2]interface{}) float64 {
    f, err := strconv.ParseFloat(s, 64)
    if math.IsNaN(f) || math.IsInf(f, 0) {
        return 0  // safe for JSON
    }
    return f
}
```

**Lesson:** Always sanitize float values before JSON encoding in Go. NaN and Inf are valid IEEE 754 floats but invalid JSON. This is a common gotcha.

---

## Phase 6: Service-to-Service with NATS

### What Is a Message Broker?

**The problem:** In Phases 1-5, aegis-stream sends events to exactly one destination (stdout or PostgreSQL). If you want a second consumer (e.g., a price alert service), you'd have to modify aegis-stream to write to both. Add a third consumer? Modify again. This doesn't scale.

**The solution:** Put a message broker in between. The producer (aegis-stream) publishes events once. Any number of consumers subscribe independently. Adding a new consumer requires zero changes to the producer.

**Without a broker (point-to-point):**
```
aegis-stream ──► PostgreSQL
              ──► Alert Service      ← must modify aegis-stream for each new consumer
              ──► Analytics Service
```

**With a broker (pub/sub):**
```
aegis-stream ──► NATS ──► PostgreSQL consumer
                      ──► Alert consumer        ← just subscribes, no changes to aegis-stream
                      ──► Analytics consumer
```

This is called the **publish/subscribe** (pub/sub) pattern. It decouples producers from consumers — they don't know about each other, they only know about the broker.

---

### Why NATS?

NATS is an open-source message broker written in Go. Created by Derek Collison (who also built messaging systems at TIBCO and Apcera).

**Why we chose NATS over Kafka:**

| | NATS | Kafka |
|---|---|---|
| **Written in** | Go | Java/Scala |
| **Binary size** | ~20MB | ~500MB + JVM |
| **Startup time** | Milliseconds | Seconds (JVM warmup) |
| **K8s deploy** | Simple StatefulSet | Needs Strimzi operator (complex) |
| **Config** | Minimal (works out of the box) | Dozens of tuning parameters |
| **Go client** | First-class (same language) | Good but JVM-native |
| **Philosophy** | Simple, fast, "always on" | Durable log, replay, exactly-once |

NATS follows the same philosophy as our project: lightweight, fast, Go-native. It's a single binary — similar to k3s being a lightweight Kubernetes.

---

### Core NATS Concepts

#### Subjects — Message Addresses

A subject is a string name that messages are published to and subscribed from. Think of it like a TV channel — publishers broadcast to a channel, subscribers tune in.

```
aegis.trades    ← all trade events go here
aegis.logs      ← all log events go here
```

**Hierarchical naming:** NATS uses dots as separators (like a file path). This enables wildcard subscriptions:
- `aegis.*` — subscribe to both `aegis.trades` and `aegis.logs`
- `aegis.>` — subscribe to anything starting with `aegis.` (including nested subjects)

#### Publish — Fire and Forget

The publisher sends a message to a subject. It doesn't know (or care) who's listening:

```go
nc.Publish("aegis.trades", data)  // send and move on
```

If nobody is subscribed, the message is silently dropped (in core NATS). This is fine for real-time data — if no one's listening, there's nothing to save.

#### Subscribe — Receive Messages

A subscriber registers interest in a subject. Every message published to that subject is delivered to every subscriber:

```go
nc.Subscribe("aegis.trades", func(msg *nats.Msg) {
    // process the trade event
    event := unmarshal(msg.Data)
})
```

**Key behavior:** If 3 services subscribe to `aegis.trades`, all 3 receive every message. This is fan-out — one publish, multiple deliveries.

#### Queue Groups — Load Balancing

What if you have 5 instances of the same consumer? You don't want each instance processing the same message — you want load balancing.

**Queue groups** solve this:
```go
nc.QueueSubscribe("aegis.trades", "alert-workers", handler)
```

All subscribers in the same queue group (`"alert-workers"`) share messages — each message goes to exactly one member. Different queue groups still get all messages independently.

```
Publisher ──► NATS subject "aegis.trades"
                    │
                    ├──► Queue group "alert-workers" (5 instances share messages)
                    │       Instance 1 gets message A
                    │       Instance 2 gets message B
                    │       Instance 3 gets message C ...
                    │
                    └──► Queue group "analytics" (3 instances share messages)
                            Instance 1 gets message A
                            Instance 2 gets message B ...
```

This is how you scale consumers horizontally in Kubernetes — run multiple pods in the same queue group.

---

### JetStream — Persistent Messaging

Core NATS is "fire and forget" — if no one is subscribed when a message arrives, it's gone. JetStream adds persistence.

**What JetStream adds:**

| Feature | Core NATS | JetStream |
|---|---|---|
| **Message storage** | None (in-memory only) | Disk or memory, configurable |
| **Replay** | No — miss it, lose it | Yes — consumers can start from any point |
| **Delivery guarantee** | At-most-once | At-least-once (with ack) |
| **Consumer tracking** | None | Server tracks what each consumer has processed |

**When you need JetStream:**
- Consumer crashes and restarts — it can resume from where it left off
- You need an audit trail of all messages
- Consumers process at different speeds — slow ones don't lose messages

**When core NATS is enough:**
- Real-time monitoring dashboards (showing live data, old data doesn't matter)
- Notifications (if you missed it, a new one is coming soon anyway)

**Streams and Consumers:**
```
Stream "AEGIS" (stores messages from aegis.trades and aegis.logs)
    │
    ├──► Consumer "alert-service" (tracks: processed up to message #1500)
    │
    └──► Consumer "analytics" (tracks: processed up to message #1200, slower)
```

A **Stream** defines what subjects to capture and how long to keep messages. A **Consumer** is a stateful subscription that remembers its position. If it disconnects and reconnects, it resumes from where it left off.

---

### NATS in Kubernetes

**Deployment:** NATS runs as a StatefulSet (like PostgreSQL) because JetStream stores data on disk.

```
k8s/nats/
├── statefulset.yaml    # NATS server with JetStream enabled
└── service.yaml        # Headless service for aegis-nats:4222
```

**Ports:**

| Port | Purpose |
|---|---|
| 4222 | Client connections (what aegis-stream connects to) |
| 8222 | HTTP monitoring (health checks, stats) |
| 6222 | Cluster routing (for multi-node NATS, not needed for dev) |

**Connection from aegis-stream:**
```
nats://aegis-nats:4222
```
Same K8s DNS pattern as PostgreSQL — the Service name resolves to the pod IP.

---

### The Consumer Service Pattern

A consumer is a standalone Go service that subscribes to NATS and does something with the events. This is the first time we build a **separate microservice** that communicates with aegis-stream indirectly (through NATS, not TCP).

```go
// cmd/consumer/main.go — simplified
func main() {
    nc, _ := nats.Connect("nats://aegis-nats:4222")

    nc.Subscribe("aegis.trades", func(msg *nats.Msg) {
        var event pb.Event
        proto.Unmarshal(msg.Data, &event)
        trade := event.GetTrade()
        if trade.Price > threshold {
            slog.Warn("price alert", "symbol", trade.Symbol, "price", trade.Price)
        }
    })

    // Block until shutdown signal
    <-ctx.Done()
}
```

**What this teaches:**
- Building independent microservices that communicate via messages (not direct calls)
- Deploying multiple services in K8s that work together
- The difference between synchronous (HTTP/TCP) and asynchronous (message queue) communication

---

### How NATS Fits Our Architecture

```
Phase 1-4:  Binance → feed → aegis-stream → stdout (log and forget)
Phase 5:    Binance → feed → aegis-stream → PostgreSQL (store)
Phase 6:    Binance → feed → aegis-stream → NATS → multiple consumers
```

Each phase adds a more sophisticated event destination:
1. **stdout** — proves the pipeline works
2. **PostgreSQL** — proves we can store and query events
3. **NATS** — proves we can fan out to independent services

This progression (point-to-point → storage → pub/sub) mirrors how real systems evolve.

---

### JetStream API Upgrade (Core NATS → JetStream)

**Why upgrade?** Core NATS `Publish()` is fire-and-forget — the client copies data to an internal buffer and hopes the server gets it. If the server crashes, messages in transit are lost. JetStream `Publish()` waits for a **PubAck** — the server confirms the message is persisted to disk before returning.

**Sink changes:**
```go
// Before: fire-and-forget
conn.Publish(subject, data)

// After: guaranteed delivery with server acknowledgement
js, _ := jetstream.New(conn)
js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
    Name:     "AEGIS",
    Subjects: []string{"aegis.trades", "aegis.logs"},
})
js.Publish(ctx, subject, data)  // blocks until PubAck received
```

`CreateOrUpdateStream` is idempotent — safe to call on every startup. The sink self-bootstraps: deploy it and the stream is guaranteed to exist.

**Consumer changes:**
```go
// Before: ephemeral subscription (miss messages if offline)
conn.Subscribe(subject, func(msg *nats.Msg) {
    process(msg.Data)   // .Data is a field
})

// After: durable consumer with explicit acknowledgement
cons, _ := js.CreateOrUpdateConsumer(ctx, "AEGIS", jetstream.ConsumerConfig{
    Durable:        "price-alerts",     // survives restarts
    FilterSubjects: []string{subject},  // only trades (or logs)
    AckPolicy:      jetstream.AckExplicitPolicy,
})
cons.Consume(func(msg jetstream.Msg) {
    process(msg.Data())  // .Data() is a method call
    msg.Ack()            // tell NATS we're done
})
```

**Key differences to remember:**
- `Durable` — NATS remembers the consumer's position across restarts
- `AckExplicit` — you must call `msg.Ack()`. If the consumer crashes mid-processing, NATS redelivers the message (at-least-once delivery)
- `msg.Data()` is a method (JetStream), not `msg.Data` field (Core NATS)
- `FilterSubjects` (plural) — common typo: `FilterSubject` won't compile

**The import path:**
```go
"github.com/nats-io/nats.go"           // base connection (nats.Connect)
"github.com/nats-io/nats.go/jetstream" // JetStream API (jetstream.New)
```
Both come from the same module — the `jetstream` sub-package ships with `nats.go`.

---

### NATS Clustering in Kubernetes

Scaling from 1 NATS node to a 3-node cluster in k3s surfaced four real-world issues. Each one teaches a fundamental K8s concept.

#### Issue 1: PVC Provisioner Crash

**Symptom:** `aegis-nats-1` stuck in `Pending`, PVC not provisioning.

**Root cause:** The `local-path-provisioner` pod in `kube-system` was in `CrashLoopBackOff`. It couldn't reach the K8s API server due to internal cluster networking issues (`i/o timeout` to `10.43.0.1:443`).

**Fix:** `sudo systemctl restart k3s` to restore internal cluster networking.

**Lesson:** When PVCs won't provision, check the provisioner pod first (`kubectl get pods -n kube-system | grep local-path`). The problem is often the provisioner itself, not your manifest.

#### Issue 2: Missing `server_name`

**Symptom:** Pods crash with `jetstream cluster requires server_name to be set`.

**Root cause:** JetStream clustering needs each node to have a unique identity. Single-node mode doesn't require this, so it wasn't in our config.

**Fix:** Pass `--name $(HOSTNAME)` as a CLI arg in the StatefulSet container args. K8s sets `HOSTNAME` to the pod name (`aegis-nats-0`, `aegis-nats-1`, etc.).

**Why not an env var?** NATS doesn't read `server_name` from environment variables. The `--name` CLI flag is the correct way to set it dynamically. We still define the `HOSTNAME` env var in the StatefulSet using `fieldRef: metadata.name` so that `$(HOSTNAME)` resolves in the args.

#### Issue 3: StatefulSet Ordering Deadlock

**Symptom:** `aegis-nats-1` restarts in a loop, `aegis-nats-2` never created.

**Root cause:** Default `podManagementPolicy: OrderedReady` creates pods sequentially — it won't create pod N+1 until pod N is `Ready`. But a NATS cluster needs a quorum (2 of 3 nodes) to elect a meta leader and pass health checks. Pod 1 can never become Ready without pod 2, which can't be created until pod 1 is Ready. Classic deadlock.

**Fix:** Set `podManagementPolicy: Parallel` on the StatefulSet so all 3 pods start simultaneously.

**Gotcha:** `podManagementPolicy` can't be updated on an existing StatefulSet. You must delete and recreate it (`kubectl delete statefulset aegis-nats && kubectl apply -f`). PVCs survive the delete since they're separate resources.

**When to use Parallel:** Any clustered stateful service that needs quorum for health — NATS, etcd, CockroachDB, Consul.

#### Issue 4: DNS Deadlock with Headless Service

**Symptom:** All 3 pods running but can't route to each other. Logs show `Waiting for routing to be established...` and `no such host` for peer DNS names. `kubectl get endpoints aegis-nats` shows empty endpoints.

**Root cause:** Headless Services only register pod IPs in DNS after pods pass the readiness probe. But NATS nodes need DNS to find peers → need peers to form a cluster → need a cluster to pass the readiness probe. Circular dependency.

**Visual:**
```
Pod starts → needs DNS to find peers
                    ↓
         DNS only has Ready pods
                    ↓
         Pod isn't Ready (no cluster)
                    ↓
         DNS doesn't include pod ← deadlock
```

**Fix:** Set `publishNotReadyAddresses: true` on the headless Service. This tells K8s to add pod IPs to DNS immediately, before readiness probes pass.

**Lesson:** For any StatefulSet that uses peer DNS for cluster bootstrapping (NATS, etcd, CockroachDB, Kafka), the headless Service **must** have `publishNotReadyAddresses: true`. Single-node deployments never hit this because there's no peer discovery.

#### The Final Working Configuration

These are the key fields that make NATS clustering work in K8s:

```yaml
# service.yaml
spec:
  clusterIP: None
  publishNotReadyAddresses: true   # DNS before readiness

# statefulset.yaml
spec:
  podManagementPolicy: Parallel    # all pods start together
  template:
    spec:
      containers:
        - args: ["-c", "/etc/nats/nats.conf", "--name", "$(HOSTNAME)"]
          env:
            - name: HOSTNAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
          livenessProbe:
            initialDelaySeconds: 30  # give cluster time to form
            failureThreshold: 5
          readinessProbe:
            initialDelaySeconds: 10
            failureThreshold: 6

# nats.conf (in ConfigMap)
cluster {
  name: aegis
  port: 6222
  routes [
    nats-route://aegis-nats-0.aegis-nats.default.svc.cluster.local:6222
    nats-route://aegis-nats-1.aegis-nats.default.svc.cluster.local:6222
    nats-route://aegis-nats-2.aegis-nats.default.svc.cluster.local:6222
  ]
}
```

**Meta-lesson:** Single-node deploys are easy. Clustering is where you learn how K8s networking, DNS, probes, and StatefulSet ordering actually work together. Every issue above is well-documented — but you only internalize them by hitting them yourself.
