package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"aegis-stream/internal/frame"
	"aegis-stream/pb"
)

// aggTrade represents one message from Binance's aggregate trade stream.
// Binance sends JSON like:
//
//	{"e":"aggTrade","E":1672515782136,"s":"BTCUSDT","p":"42500.50","q":"2.5",...}
//
// The "p" (price) and "q" (quantity) fields are strings because Binance
// uses arbitrary-precision decimals. We parse them to float64.
type aggTrade struct {
	EventType string `json:"e"` // "aggTrade"
	Symbol    string `json:"s"` // e.g. "BTCUSDT"
	Price     string `json:"p"` // decimal string, e.g. "42500.50"
	Quantity  string `json:"q"` // decimal string, e.g. "2.5"
	Time      int64  `json:"E"` // event time in milliseconds since epoch
}

// streamMsg wraps the combined stream format that Binance uses when you
// subscribe to multiple symbols at once. The outer envelope looks like:
//
//	{"stream":"btcusdt@aggTrade","data":{...the actual trade...}}
type streamMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// stats tracks counters across goroutines using atomic operations,
// so the stats printer and the main loop can read/write safely.
type stats struct {
	forwarded atomic.Int64
	errors    atomic.Int64
}

func main() {
	// --- Config: flags with env var overrides (same pattern as internal/config) ---
	server := flag.String("server", "localhost:9000", "aegis-stream TCP address")
	symbols := flag.String("symbols", "btcusdt,ethusdt,solusdt", "comma-separated Binance symbols")
	logInterval := flag.Int("log-interval", 100, "send a synthetic Log event every N trades")
	flag.Parse()

	// Env vars override flags — same pattern used everywhere in this project.
	if v, ok := os.LookupEnv("AEGIS_FEED_SERVER"); ok {
		*server = v
	}
	if v, ok := os.LookupEnv("AEGIS_FEED_SYMBOLS"); ok {
		*symbols = v
	}
	if v, ok := os.LookupEnv("AEGIS_FEED_LOG_INTERVAL"); ok {
		n, err := strconv.Atoi(v)
		if err == nil {
			*logInterval = n
		}
	}

	symbolList := strings.Split(*symbols, ",")
	wsURL := buildWSURL(symbolList)

	// --- Connect to aegis-stream server via TCP ---
	log.Printf("Connecting to aegis-stream at %s", *server)
	tcpConn, err := net.Dial("tcp", *server)
	if err != nil {
		log.Fatalf("Failed to connect to server: %v", err)
	}
	defer tcpConn.Close()
	log.Println("Connected to aegis-stream")

	// --- Graceful shutdown ---
	// context.WithCancel gives us a "cancel" function. When we call cancel(),
	// ctx.Done() closes, and every goroutine watching it knows to stop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Catch SIGINT (Ctrl+C) and SIGTERM (K8s pod shutdown).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("Received %v, shutting down...", sig)
		cancel()
	}()

	// --- Stats printer: shows progress every 5 seconds ---
	var s stats
	go printStats(ctx, &s)

	// --- Main loop: connect to Binance and stream trades ---
	log.Printf("Subscribing to Binance streams: %v", symbolList)
	connectAndStream(ctx, wsURL, tcpConn, &s, *logInterval)

	log.Printf("Feed stopped. Forwarded: %d, Errors: %d",
		s.forwarded.Load(), s.errors.Load())
}

// buildWSURL constructs the Binance combined stream URL.
// For symbols ["btcusdt", "ethusdt"], it produces:
//
//	wss://stream.binance.com:9443/stream?streams=btcusdt@aggTrade/ethusdt@aggTrade
func buildWSURL(symbols []string) string {
	streams := make([]string, len(symbols))
	for i, sym := range symbols {
		streams[i] = strings.ToLower(strings.TrimSpace(sym)) + "@aggTrade"
	}
	return "wss://stream.binance.com:9443/stream?streams=" + strings.Join(streams, "/")
}

// connectAndStream connects to the Binance WebSocket and reads trades in a loop.
// On disconnect, it reconnects with exponential backoff (1s, 2s, 4s, ... max 30s).
// It exits when the context is cancelled (SIGINT/SIGTERM).
func connectAndStream(ctx context.Context, wsURL string, tcpConn net.Conn, s *stats, logInterval int) {
	var tradeCount int64
	backoff := time.Second // Start at 1 second, double on each failure.

	for {
		// Check if we should stop before attempting to connect.
		if ctx.Err() != nil {
			return
		}

		log.Printf("Connecting to Binance WebSocket...")
		ws, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		if err != nil {
			log.Printf("WebSocket dial failed: %v (retrying in %v)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			// Exponential backoff: 1s → 2s → 4s → 8s → ... → 30s max.
			backoff = time.Duration(math.Min(float64(backoff*2), float64(30*time.Second)))
			continue
		}
		log.Println("Connected to Binance WebSocket")
		backoff = time.Second // Reset backoff on successful connection.

		// Read messages until error or shutdown.
		err = readLoop(ctx, ws, tcpConn, s, logInterval, &tradeCount)
		ws.Close()

		if ctx.Err() != nil {
			return // Shutdown requested, don't reconnect.
		}
		log.Printf("WebSocket disconnected: %v (reconnecting in %v)", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = time.Duration(math.Min(float64(backoff*2), float64(30*time.Second)))
	}
}

// readLoop reads messages from the WebSocket, converts them to protobuf,
// and sends them to the aegis-stream server. Returns on error or context cancel.
func readLoop(ctx context.Context, ws *websocket.Conn, tcpConn net.Conn, s *stats, logInterval int, tradeCount *int64) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// ReadMessage blocks until a message arrives or the connection breaks.
		_, msgBytes, err := ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		// Parse the combined stream envelope.
		var envelope streamMsg
		if err := json.Unmarshal(msgBytes, &envelope); err != nil {
			s.errors.Add(1)
			continue
		}

		// Parse the inner aggTrade data.
		var trade aggTrade
		if err := json.Unmarshal(envelope.Data, &trade); err != nil {
			s.errors.Add(1)
			continue
		}

		// Convert Binance strings to numbers.
		price, err := strconv.ParseFloat(trade.Price, 64)
		if err != nil {
			s.errors.Add(1)
			continue
		}
		qty, err := strconv.ParseFloat(trade.Quantity, 64)
		if err != nil {
			s.errors.Add(1)
			continue
		}

		// Build the protobuf Event — same structure your server expects.
		*tradeCount++
		event := &pb.Event{
			EventId: fmt.Sprintf("feed-%d", *tradeCount),
			Payload: &pb.Event_Trade{
				Trade: &pb.Trade{
					Symbol:    trade.Symbol,
					Price:     price,
					Volume:    int32(qty),
					Timestamp: trade.Time * 1_000_000, // Binance ms → proto ns
				},
			},
		}

		if err := sendEvent(tcpConn, event); err != nil {
			s.errors.Add(1)
			return fmt.Errorf("tcp send: %w", err)
		}
		s.forwarded.Add(1)

		// Every N trades, also send a synthetic Log event to demonstrate
		// mixed payloads flowing through the pipeline.
		if logInterval > 0 && *tradeCount%int64(logInterval) == 0 {
			logEvent := &pb.Event{
				EventId: fmt.Sprintf("feed-log-%d", *tradeCount),
				Payload: &pb.Event_Log{
					Log: &pb.Log{
						Level:       "INFO",
						Message:     fmt.Sprintf("processed %d trades from %s", *tradeCount, trade.Symbol),
						ServiceName: "binance-feed",
						Timestamp:   time.Now().UnixNano(),
					},
				},
			}
			if err := sendEvent(tcpConn, logEvent); err != nil {
				s.errors.Add(1)
				return fmt.Errorf("tcp send log: %w", err)
			}
			s.forwarded.Add(1)
		}
	}
}

// sendEvent marshals a protobuf Event and sends it over TCP using
// the project's standard 4-byte length-prefix framing.
func sendEvent(conn net.Conn, event *pb.Event) error {
	data, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return frame.Write(conn, data)
}

// printStats prints forwarding progress every 5 seconds.
// It calculates the rate from the delta since the last print.
func printStats(ctx context.Context, s *stats) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastForwarded int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := s.forwarded.Load()
			delta := current - lastForwarded
			rate := float64(delta) / 5.0
			lastForwarded = current

			log.Printf("[feed] forwarded=%d errors=%d rate=%.0f/s",
				current, s.errors.Load(), rate)
		}
	}
}
