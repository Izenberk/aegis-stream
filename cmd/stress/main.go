package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aegis-stream/internal/frame"
	"aegis-stream/pb"
	"google.golang.org/protobuf/proto"
)

// --- Configuration ---

type config struct {
	Server          string
	MetricsURL      string
	Connections     int
	Rate            int           // events/sec total across all connections
	Duration        time.Duration // sustained phase length
	SpikeMultiplier int           // spike rate = rate × this
	SpikeDuration   time.Duration // spike phase length
}

// --- Atomic counters shared across sender goroutines ---

type results struct {
	sent   atomic.Int64
	errors atomic.Int64
}

// --- Payload data pools for varied, realistic events ---

var (
	tradeSymbols = []string{"BTC-USD", "ETH-USD", "SOL-USD", "AAPL", "TSLA", "NVDA", "AMZN"}
	tradePrices  = []float64{72500.0, 2100.0, 90.0, 185.0, 250.0, 900.0, 190.0}
	logLevels    = []string{"INFO", "WARN", "ERROR", "DEBUG"}
	logMessages  = []string{
		"request processed successfully",
		"high latency detected on upstream",
		"connection pool exhausted",
		"cache miss for user session",
		"retry attempt 3 of 5",
		"health check passed",
	}
	logServices = []string{"api-gateway", "matching-engine", "risk-service", "order-router"}
)

func main() {
	cfg := parseConfig()

	// --- Step 1: Pre-generate varied payloads ---
	// We build 100 different framed payloads BEFORE the timed test.
	// This way, the send loop does zero allocation — just writes pre-built bytes.
	// Same idea as cmd/bench (pre-generate one payload), but with variety.
	fmt.Println("Pre-generating 100 varied payloads...")
	payloads := generatePayloads(100)

	// --- Step 2: Snapshot server state before the test ---
	fmt.Printf("Querying server metrics at %s...\n", cfg.MetricsURL)
	beforeCount := queryProcessedCount(cfg.MetricsURL)

	// --- Step 3: Open N TCP connections ---
	fmt.Printf("Opening %d connections to %s...\n", cfg.Connections, cfg.Server)
	conns := make([]net.Conn, cfg.Connections)
	for i := range conns {
		conn, err := net.Dial("tcp", cfg.Server)
		if err != nil {
			log.Fatalf("Failed to open connection %d: %v", i, err)
		}
		conns[i] = conn
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// --- Step 4: Start live progress printer ---
	var res results
	stopProgress := make(chan struct{})
	go printProgress(&res, cfg.MetricsURL, stopProgress)

	start := time.Now()

	// --- Step 5: Sustained phase ---
	fmt.Printf("\n=== Sustained Phase: %d events/sec for %v ===\n", cfg.Rate, cfg.Duration)
	runPhase(conns, payloads, cfg.Rate, cfg.Duration, &res)

	// --- Step 6: Spike phase ---
	spikeRate := cfg.Rate * cfg.SpikeMultiplier
	fmt.Printf("\n=== Spike Phase: %d events/sec for %v (%dx) ===\n",
		spikeRate, cfg.SpikeDuration, cfg.SpikeMultiplier)
	runPhase(conns, payloads, spikeRate, cfg.SpikeDuration, &res)

	// --- Step 7: Close connections and wait for server to drain ---
	fmt.Println("\nClosing connections...")
	for _, c := range conns {
		c.Close()
	}

	fmt.Println("Waiting for server to drain queue...")
	totalSent := res.sent.Load()
	afterCount := waitForDrain(cfg.MetricsURL, beforeCount, totalSent)
	totalElapsed := time.Since(start)

	// Stop the progress printer.
	close(stopProgress)

	// --- Step 8: Final report ---
	serverProcessed := afterCount - beforeCount
	loss := float64(totalSent-serverProcessed) / float64(totalSent) * 100
	if totalSent == 0 {
		loss = 0
	}

	fmt.Printf("\n--- Stress Test Report ---\n")
	fmt.Printf("Sustained:        %v at %d events/sec\n", cfg.Duration, cfg.Rate)
	fmt.Printf("Spike:            %v at %d events/sec (%dx)\n",
		cfg.SpikeDuration, spikeRate, cfg.SpikeMultiplier)
	fmt.Printf("Connections:      %d\n", cfg.Connections)
	fmt.Printf("\n")
	fmt.Printf("Total sent:       %d\n", totalSent)
	fmt.Printf("Server processed: %d\n", serverProcessed)
	fmt.Printf("Loss:             %.2f%%\n", loss)
	fmt.Printf("Errors:           %d\n", res.errors.Load())
	fmt.Printf("Total duration:   %v\n", totalElapsed.Round(time.Millisecond))
	if totalElapsed.Seconds() > 0 {
		fmt.Printf("Avg throughput:   %.0f events/sec (server-side)\n",
			float64(serverProcessed)/totalElapsed.Seconds())
	}
}

// parseConfig reads flags and env var overrides.
func parseConfig() config {
	cfg := config{}
	flag.StringVar(&cfg.Server, "server", "localhost:9000", "aegis-stream TCP address")
	flag.StringVar(&cfg.MetricsURL, "metrics-url", "http://localhost:2112/metrics", "Prometheus metrics endpoint")
	flag.IntVar(&cfg.Connections, "connections", 10, "number of parallel TCP connections")
	flag.IntVar(&cfg.Rate, "rate", 5000, "events/sec during sustained phase")
	flag.DurationVar(&cfg.Duration, "duration", 30*time.Second, "sustained phase length")
	flag.IntVar(&cfg.SpikeMultiplier, "spike-multiplier", 5, "spike rate = rate × this")
	flag.DurationVar(&cfg.SpikeDuration, "spike-duration", 10*time.Second, "spike phase length")
	flag.Parse()

	// Env var overrides.
	if v, ok := os.LookupEnv("AEGIS_STRESS_SERVER"); ok {
		cfg.Server = v
	}
	if v, ok := os.LookupEnv("AEGIS_STRESS_METRICS"); ok {
		cfg.MetricsURL = v
	}
	if v, ok := os.LookupEnv("AEGIS_STRESS_CONNS"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Connections = n
		}
	}
	if v, ok := os.LookupEnv("AEGIS_STRESS_RATE"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Rate = n
		}
	}
	if v, ok := os.LookupEnv("AEGIS_STRESS_DURATION"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Duration = d
		}
	}
	if v, ok := os.LookupEnv("AEGIS_STRESS_SPIKE_MULT"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.SpikeMultiplier = n
		}
	}
	if v, ok := os.LookupEnv("AEGIS_STRESS_SPIKE_DUR"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.SpikeDuration = d
		}
	}

	return cfg
}

// generatePayloads creates N pre-framed binary payloads with varied data.
// 70% are Trade events, 30% are Log events — mimicking a real mixed workload.
// Each payload is already marshaled + framed, ready to write directly to TCP.
func generatePayloads(n int) [][]byte {
	payloads := make([][]byte, n)

	for i := range n {
		var event *pb.Event

		if i%10 < 7 {
			// 70% Trade events with random symbols and realistic prices.
			idx := rand.Intn(len(tradeSymbols))
			// Add ±5% price jitter to make each payload unique.
			jitter := 0.95 + rand.Float64()*0.10
			event = &pb.Event{
				EventId: fmt.Sprintf("stress-%d", i),
				Payload: &pb.Event_Trade{
					Trade: &pb.Trade{
						Symbol:    tradeSymbols[idx],
						Price:     tradePrices[idx] * jitter,
						Volume:    int32(1 + rand.Intn(1000)),
						Timestamp: time.Now().UnixNano(),
					},
				},
			}
		} else {
			// 30% Log events with varied levels, messages, and services.
			event = &pb.Event{
				EventId: fmt.Sprintf("stress-log-%d", i),
				Payload: &pb.Event_Log{
					Log: &pb.Log{
						Level:       logLevels[rand.Intn(len(logLevels))],
						Message:     logMessages[rand.Intn(len(logMessages))],
						ServiceName: logServices[rand.Intn(len(logServices))],
						Timestamp:   time.Now().UnixNano(),
					},
				},
			}
		}

		data, err := proto.Marshal(event)
		if err != nil {
			log.Fatalf("Failed to marshal payload %d: %v", i, err)
		}

		var buf bytes.Buffer
		if err := frame.Write(&buf, data); err != nil {
			log.Fatalf("Failed to frame payload %d: %v", i, err)
		}
		payloads[i] = buf.Bytes()
	}

	return payloads
}

// runPhase distributes the target rate evenly across all connections,
// spawns a sender goroutine per connection, and waits for the duration to elapse.
func runPhase(conns []net.Conn, payloads [][]byte, rate int, duration time.Duration, res *results) {
	// Each connection is responsible for an equal share of the total rate.
	perConn := rate / len(conns)
	if perConn < 1 {
		perConn = 1
	}

	var wg sync.WaitGroup
	for i, conn := range conns {
		wg.Add(1)
		go func(id int, conn net.Conn) {
			defer wg.Done()
			sender(conn, payloads, perConn, duration, res)
		}(i, conn)
	}
	wg.Wait()
}

// sender is the per-connection goroutine. It sends events at a fixed rate
// using a time.Ticker for pacing. Each tick, it picks a random pre-built
// payload and writes it to the TCP connection.
func sender(conn net.Conn, payloads [][]byte, ratePerConn int, duration time.Duration, res *results) {
	// time.Ticker fires at a regular interval to pace our sends.
	// For 500 events/sec: interval = 1s / 500 = 2ms between sends.
	interval := time.Second / time.Duration(ratePerConn)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	deadline := time.Now().Add(duration)
	i := 0

	for time.Now().Before(deadline) {
		<-ticker.C // Wait for the next tick.

		// Pick a payload from the pre-built pool (round-robin).
		payload := payloads[i%len(payloads)]
		i++

		_, err := conn.Write(payload)
		if err != nil {
			res.errors.Add(1)
			return // Connection broken, stop this sender.
		}
		res.sent.Add(1)
	}
}

// printProgress shows live stats every 2 seconds until stopCh is closed.
func printProgress(res *results, metricsURL string, stopCh <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastSent int64

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			currentSent := res.sent.Load()
			delta := currentSent - lastSent
			rate := float64(delta) / 2.0
			lastSent = currentSent

			// Also check server-side count.
			serverCount := queryProcessedCount(metricsURL)

			fmt.Printf("[stress] sent=%d (%.0f/s) | server=%d | errors=%d\n",
				currentSent, rate, serverCount, res.errors.Load())
		}
	}
}

// queryProcessedCount scrapes the Prometheus /metrics endpoint and returns
// the total aegis_events_processed_total across all labels.
// Same logic as cmd/bench, but with a configurable URL.
func queryProcessedCount(metricsURL string) int64 {
	resp, err := http.Get(metricsURL)
	if err != nil {
		return 0 // Don't crash — metrics may be temporarily unavailable.
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0
	}

	var total int64
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "aegis_events_processed_total{") {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				n, err := strconv.ParseFloat(parts[1], 64)
				if err == nil {
					total += int64(n)
				}
			}
		}
	}
	return total
}

// waitForDrain polls the metrics endpoint until the server has processed
// all expected events, or until a timeout (15 seconds).
func waitForDrain(metricsURL string, beforeCount, expected int64) int64 {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current := queryProcessedCount(metricsURL)
		if current-beforeCount >= expected {
			return current
		}
		time.Sleep(200 * time.Millisecond)
	}
	return queryProcessedCount(metricsURL)
}
