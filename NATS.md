# NATS Deep Dive: Design Decisions & Implementation Notes

A companion to NOTES.md Phase 6. This covers the **"why this way"** behind each implementation choice — the things you learn by building, not just reading.

---

## 1. The Sink: Why NATS Is So Much Simpler Than PostgreSQL

### PostgresSink (complex — 256 lines)
```
Write() → lock mutex → append to buffer → check batch size → unlock
                                              ↓
                                         flush() → BEGIN tx → INSERT → COMMIT
                                              ↑
                         backgroundFlusher (goroutine) ticks every 500ms
```
PostgreSQL needs all this because **every INSERT is a network round-trip** — a TCP packet goes to Postgres, Postgres writes to disk, sends back "OK". At 5,000 events/sec, that's 5,000 round-trips. Batching reduces this to ~50 batch inserts/sec.

### NATSSink (simple — 80 lines)
```
Write() → pick subject → proto.Marshal → conn.Publish()
```
That's it. No mutex, no buffer, no background goroutine. Why?

**`conn.Publish()` is a memory copy, not a network call.** The NATS client library has an internal write buffer. When you call Publish(), it:
1. Copies your bytes into the buffer (nanoseconds)
2. Returns immediately
3. A background goroutine in the NATS library flushes the buffer to the server

So the batching exists — it's just inside the NATS client, not our code. The library authors already solved this problem.

**Lesson:** Before building complex machinery (mutexes, buffers, flushers), check if the library already handles it. NATS's client is designed for high-throughput — it would be wasteful to add our own batching on top.

---

## 2. Subject Design: `aegis.trades` and `aegis.logs`

### Why dot-separated names?

NATS subjects use dots as hierarchy separators, like file paths use slashes. This isn't just convention — it enables **wildcard subscriptions**:

```
aegis.trades     ← only trades
aegis.logs       ← only logs
aegis.*          ← both (single-level wildcard)
aegis.>          ← everything under aegis. (multi-level wildcard)
```

### Why two subjects instead of one?

We could publish everything to `aegis.events` and let consumers filter. But separate subjects are better because:

1. **Consumers subscribe to what they need.** The price alert consumer only cares about trades — why receive log events and discard them?
2. **NATS does the filtering.** The server only sends messages matching the subscription. No wasted network bandwidth.
3. **Independent scaling.** You can have 10 trade consumers and 2 log consumers.

### Why not `aegis.trades.btcusdt`? (per-symbol subjects)

You could go more granular: `aegis.trades.btcusdt`, `aegis.trades.ethusdt`. Then a BTC-only consumer subscribes to `aegis.trades.btcusdt`.

We didn't because:
- Our feed only handles 3 symbols — per-symbol subjects add complexity without benefit
- The consumer already filters by checking `trade.Symbol` in code
- If you needed this later, it's a one-line change in the sink's subject routing

**Lesson:** Start with the simplest subject design that works. You can always add hierarchy later.

---

## 3. Serialization: Why `proto.Marshal` the Full Event

### The choice

```go
// Option A: Marshal the full Event (what we do)
data, _ := proto.Marshal(event)  // includes event_id + payload

// Option B: Marshal just the Trade
data, _ := proto.Marshal(event.GetTrade())  // only the trade fields
```

### Why the full Event?

1. **One format everywhere.** The feed creates `*pb.Event`, the server processes `*pb.Event`, workers unmarshal `*pb.Event`, the sink publishes `*pb.Event`, the consumer receives `*pb.Event`. No translation layers.

2. **Consumers get `event_id`.** This is critical for deduplication and debugging. If a consumer sees a duplicate, it can check `event_id`. If you only sent the Trade, you'd lose this.

3. **Forward-compatible.** If you add fields to `Event` later (e.g., `source_ip`, `received_at`), consumers automatically get them without code changes.

**Lesson:** When choosing a wire format, prefer the one that preserves the most context. The extra bytes (event_id is ~12 bytes) are negligible compared to the debugging value.

---

## 4. Connection Lifecycle: `Drain()` vs `Close()`

### The difference

```go
conn.Close()  // Immediately severs the TCP connection.
              // Any messages still in the client's write buffer are LOST.

conn.Drain()  // 1. Stops accepting new Publish() calls
              // 2. Flushes everything in the buffer to the server
              // 3. Waits for confirmation
              // 4. Then closes the connection
```

### Why Drain?

During shutdown, there may be messages that `Publish()` accepted (returned nil) but haven't been sent to the server yet (still in the client buffer). `Close()` would lose them silently. `Drain()` guarantees delivery of everything that was accepted.

This mirrors our PostgresSink pattern: `Close()` does a final flush before closing the pool. Same guarantee, different mechanism.

**Lesson:** Always check how your client library handles buffered data on shutdown. "Fire and forget" during operation doesn't mean "lose data on exit."

---

## 5. The Consumer: Design Decisions

### Why `atomic.Int64` instead of `sync.Mutex`?

```go
// What we do (atomic)
var msgCount atomic.Int64
msgCount.Add(1)        // in subscription callback (NATS goroutine)
msgCount.Load()        // in stats ticker (main goroutine)

// Alternative (mutex)
var mu sync.Mutex
var msgCount int64
mu.Lock()
msgCount++
mu.Unlock()
```

Both are thread-safe. But atomics are better here because:
- We're doing a **single operation** (increment or read), not protecting a multi-step transaction
- Atomics are lock-free — they use CPU instructions directly, no goroutine scheduling
- A mutex would work, but it's like using a sledgehammer to hang a picture

**When to use what:**
- `atomic` — single counter/flag accessed from multiple goroutines
- `sync.Mutex` — protecting a group of related operations (like PostgresSink's buffer append + size check)

### Why a callback-based subscription?

```go
conn.Subscribe(subject, func(msg *nats.Msg) {
    // called for every message
})
```

In the main server, we built our own worker pool (100 goroutines pulling from a channel). Why not do the same here?

Because **NATS already has a goroutine pool internally.** When messages arrive, NATS dispatches them to worker goroutines that call your callback. Building another pool on top would add unnecessary complexity and indirection.

**Lesson:** Understand what your dependencies already provide before building your own version.

### Why log stats every 5 seconds?

At high throughput, logging every message floods the terminal and is unreadable. The stats ticker gives you a **summary view**: "379 messages, 379 alerts" tells you everything is working without drowning in individual events.

In production, you'd replace this with Prometheus metrics (like the main server does). The ticker is a lightweight alternative for a service that doesn't need a full metrics endpoint yet.

---

## 6. Fan-Out: The Whole Point

### What we proved

```
Terminal 1: ./bin/server -sink nats -nats-url nats://localhost:4222
Terminal 2: ./bin/consumer -nats-url nats://localhost:4222
Terminal 3: ./bin/feed
```

Events flow: **feed → server → NATS → consumer**

### What you should try next

Run two consumers simultaneously:
```
Terminal 2a: ./bin/consumer -nats-url nats://localhost:4222 -threshold 70000
Terminal 2b: ./bin/consumer -nats-url nats://localhost:4222 -threshold 80000
```

**Both** receive all messages. Consumer 2a alerts on BTC > $70K, consumer 2b only on BTC > $80K. This is fan-out: one publish, multiple independent consumers.

The server published once. It has no idea two consumers exist. That's the decoupling that pub/sub gives you.

---

## 7. What's Different from PostgreSQL (Comparison Table)

| Aspect | PostgresSink | NATSSink |
|---|---|---|
| **Lines of code** | ~256 | ~80 |
| **Thread safety** | Mutex + background goroutine | None needed (library handles it) |
| **Batching** | Manual (100 events or 500ms) | Automatic (library internal buffer) |
| **Write latency** | ~1ms (network round-trip) | ~100ns (memory copy) |
| **Shutdown** | Manual final flush | `Drain()` handles it |
| **Data persistence** | Always (it's a database) | Only with JetStream enabled |
| **Consumer coupling** | Direct SQL queries | Zero — pub/sub decoupling |
| **Failure mode** | Lost connection = lost batch | Auto-reconnect built into client |

---

## 8. NATS vs Kafka — When to Choose What

We chose NATS. Here's when you'd choose differently:

**Choose NATS when:**
- You need simple pub/sub with low latency
- Your team knows Go (NATS is Go-native)
- You want minimal operational overhead (single binary, minimal config)
- Message ordering per-subject is sufficient

**Choose Kafka when:**
- You need guaranteed message ordering across partitions
- You need to replay the entire event history (event sourcing)
- You need exactly-once delivery semantics
- Your organization already runs Kafka

**The real answer:** Start with NATS. If you hit its limits, you'll know exactly why you need Kafka — and migrating is straightforward because the sink interface abstracts the destination.
