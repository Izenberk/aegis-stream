package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aegis-stream/internal/frame"
	"aegis-stream/pb"
	"google.golang.org/protobuf/proto"
)

const (
	TargetEvents = 100000
	ServerAddr   = "localhost:9000"
	MetricsAddr  = "http://localhost:2112/metrics"
)

func main() {
	fmt.Println("Pre-generating binary payload...")
	ev := &pb.Event{
		EventId: "benchmark-evt",
		Payload: &pb.Event_Trade{
			Trade: &pb.Trade{Symbol: "ETH-USD", Price: 3100.50, Volume: 10, Timestamp: time.Now().UnixNano()},
		},
	}

	data, err := proto.Marshal(ev)
	if err != nil {
		log.Fatalf("Failed to marshal event: %v", err)
	}

	var buf bytes.Buffer
	if err := frame.Write(&buf, data); err != nil {
		log.Fatalf("Failed to build frame: %v", err)
	}
	rawFrame := buf.Bytes()

	// Snapshot the server's processed count BEFORE sending, so we can
	// calculate exactly how many events the server processed during our run.
	beforeCount := queryProcessedCount()

	fmt.Println("Connecting to server...")
	conn, err := net.Dial("tcp", ServerAddr)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}

	fmt.Printf("Blasting %d events...\n", TargetEvents)
	start := time.Now()

	for i := 0; i < TargetEvents; i++ {
		_, err := conn.Write(rawFrame)
		if err != nil {
			log.Fatalf("Write failed at event %d: %v", i, err)
		}
	}

	// Close the connection so the server finishes reading all buffered frames.
	// Without this, frames may sit in the OS TCP buffer unprocessed.
	conn.Close()

	sendElapsed := time.Since(start)
	sendRate := float64(TargetEvents) / sendElapsed.Seconds()

	// Wait for the server to finish processing the queue.
	// Poll the metrics endpoint until the count stops changing.
	fmt.Println("Waiting for server to drain queue...")
	afterCount := waitForDrain(beforeCount, TargetEvents)

	totalElapsed := time.Since(start)
	serverProcessed := afterCount - beforeCount
	serverRate := float64(serverProcessed) / totalElapsed.Seconds()

	fmt.Printf("\n--- Benchmark Results ---\n")
	fmt.Printf("Events sent:       %d\n", TargetEvents)
	fmt.Printf("Send time:         %v (%.0f events/sec)\n", sendElapsed, sendRate)
	fmt.Printf("Server processed:  %d\n", serverProcessed)
	fmt.Printf("Total time:        %v (%.0f events/sec end-to-end)\n", totalElapsed, serverRate)

	if serverProcessed != int64(TargetEvents) {
		fmt.Printf("WARNING: server processed %d, expected %d (%.1f%% loss)\n",
			serverProcessed, TargetEvents,
			float64(TargetEvents-int(serverProcessed))/float64(TargetEvents)*100,
		)
	}
}

// queryProcessedCount scrapes the Prometheus /metrics endpoint and returns
// the total of aegis_events_processed_total across all labels.
func queryProcessedCount() int64 {
	resp, err := http.Get(MetricsAddr)
	if err != nil {
		log.Fatalf("Failed to query metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read metrics: %v", err)
	}

	// Parse the Prometheus text format line by line.
	// We look for lines like: aegis_events_processed_total{type="trade"} 42
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
// all expected events, or until a timeout (10 seconds).
func waitForDrain(beforeCount int64, expected int) int64 {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current := queryProcessedCount()
		if current-beforeCount >= int64(expected) {
			return current
		}
		time.Sleep(100 * time.Millisecond) // poll every 100ms
	}
	// Timed out — return whatever count we have
	return queryProcessedCount()
}
