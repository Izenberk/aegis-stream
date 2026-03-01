package server_test

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aegis-stream/internal/frame"
	"aegis-stream/pb"
	"google.golang.org/protobuf/proto"
)

// TestFullPipeline is an integration test that wires up the complete data path:
// client → TCP → frame.Read → proto.Unmarshal → worker
//
// It verifies that events sent over a real TCP connection are correctly
// framed, deserialized, and dispatched to workers.
func TestFullPipeline(t *testing.T) {
	// --- SERVER SIDE ---

	// Listen on port 0 — the OS picks a random available port.
	// This avoids conflicts with a running server or parallel tests.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Close()

	// atomic.Int64 is a thread-safe counter — multiple workers can increment
	// it concurrently without a mutex. We use this to verify how many events
	// were actually processed.
	var processed atomic.Int64

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan []byte, 100)
	var wg sync.WaitGroup

	// Spawn 4 workers (enough for a test, no need for 100)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for data := range jobs {
				var event pb.Event
				if err := proto.Unmarshal(data, &event); err != nil {
					t.Errorf("Unmarshal failed: %v", err)
					continue
				}

				// Verify the payload is a recognized type
				switch event.Payload.(type) {
				case *pb.Event_Trade, *pb.Event_Log:
					processed.Add(1)
				default:
					t.Errorf("unknown payload type: %T", event.Payload)
				}
			}
		}()
	}

	// Accept one connection and read frames from it
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			data, err := frame.Read(conn)
			if err != nil {
				return // client closed the connection
			}
			jobs <- data
		}
	}()

	// --- CLIENT SIDE ---

	// Connect to the test server using the random port
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}

	// Send 3 events: 2 trades + 1 log
	events := []*pb.Event{
		{
			EventId: "int-001",
			Payload: &pb.Event_Trade{
				Trade: &pb.Trade{Symbol: "BTC-USD", Price: 64000.0, Volume: 1, Timestamp: time.Now().UnixNano()},
			},
		},
		{
			EventId: "int-002",
			Payload: &pb.Event_Log{
				Log: &pb.Log{Level: "ERROR", Message: "test error", ServiceName: "test-svc", Timestamp: time.Now().UnixNano()},
			},
		},
		{
			EventId: "int-003",
			Payload: &pb.Event_Trade{
				Trade: &pb.Trade{Symbol: "ETH-USD", Price: 3100.0, Volume: 10, Timestamp: time.Now().UnixNano()},
			},
		},
	}

	for _, ev := range events {
		data, err := proto.Marshal(ev)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}
		if err := frame.Write(conn, data); err != nil {
			t.Fatalf("Frame write failed: %v", err)
		}
	}

	// Close the client connection — signals the server's frame.Read to return
	conn.Close()

	// Give workers time to drain the channel
	time.Sleep(100 * time.Millisecond)

	// Shut down: cancel context, close jobs channel, wait for workers
	cancel()
	close(jobs)
	wg.Wait()

	// --- ASSERT ---
	if got := processed.Load(); got != 3 {
		t.Errorf("processed %d events, want 3", got)
	}
}
