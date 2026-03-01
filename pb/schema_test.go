package pb_test

import (
	"testing"
	"time"

	"aegis-stream/pb"
	"google.golang.org/protobuf/proto"
)

// TestTradeRoundTrip: marshal a Trade event to bytes, unmarshal it back,
// verify every field survives the round trip.
func TestTradeRoundTrip(t *testing.T) {
	original := &pb.Event{
		EventId: "trade-001",
		Payload: &pb.Event_Trade{
			Trade: &pb.Trade{
				Symbol:    "BTC-USD",
				Price:     64230.50,
				Volume:    5,
				Timestamp: time.Now().UnixNano(),
			},
		},
	}

	// Marshal: Go struct → binary bytes
	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Unmarshal: binary bytes → Go struct
	var got pb.Event
	if err := proto.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify the payload is a Trade (not a Log)
	trade, ok := got.Payload.(*pb.Event_Trade)
	if !ok {
		t.Fatalf("expected Event_Trade, got %T", got.Payload)
	}

	// Check each field individually — gives clear error messages on failure
	if got.EventId != original.EventId {
		t.Errorf("EventId: got %q, want %q", got.EventId, original.EventId)
	}
	if trade.Trade.Symbol != "BTC-USD" {
		t.Errorf("Symbol: got %q, want %q", trade.Trade.Symbol, "BTC-USD")
	}
	if trade.Trade.Price != 64230.50 {
		t.Errorf("Price: got %f, want %f", trade.Trade.Price, 64230.50)
	}
	if trade.Trade.Volume != 5 {
		t.Errorf("Volume: got %d, want %d", trade.Trade.Volume, 5)
	}
}

// TestLogRoundTrip: same pattern as Trade, but with a Log payload.
func TestLogRoundTrip(t *testing.T) {
	original := &pb.Event{
		EventId: "log-001",
		Payload: &pb.Event_Log{
			Log: &pb.Log{
				Level:       "WARN",
				Message:     "High latency detected",
				ServiceName: "matching-engine",
				Timestamp:   time.Now().UnixNano(),
			},
		},
	}

	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var got pb.Event
	if err := proto.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	logPayload, ok := got.Payload.(*pb.Event_Log)
	if !ok {
		t.Fatalf("expected Event_Log, got %T", got.Payload)
	}

	if logPayload.Log.Level != "WARN" {
		t.Errorf("Level: got %q, want %q", logPayload.Log.Level, "WARN")
	}
	if logPayload.Log.Message != "High latency detected" {
		t.Errorf("Message: got %q, want %q", logPayload.Log.Message, "High latency detected")
	}
	if logPayload.Log.ServiceName != "matching-engine" {
		t.Errorf("ServiceName: got %q, want %q", logPayload.Log.ServiceName, "matching-engine")
	}
}

// TestEventPreservesType: the oneof field must distinguish Trade from Log.
// Marshal a Trade, unmarshal it, confirm the type switch lands on Event_Trade.
func TestEventPreservesType(t *testing.T) {
	tradeEvent := &pb.Event{
		EventId: "type-check",
		Payload: &pb.Event_Trade{
			Trade: &pb.Trade{Symbol: "ETH-USD", Price: 3100.0, Volume: 10, Timestamp: 0},
		},
	}

	data, err := proto.Marshal(tradeEvent)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var got pb.Event
	if err := proto.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// This is the same type switch pattern used in the server's worker function
	switch got.Payload.(type) {
	case *pb.Event_Trade:
		// correct — do nothing
	case *pb.Event_Log:
		t.Error("expected Trade payload, but got Log")
	default:
		t.Errorf("unexpected payload type: %T", got.Payload)
	}
}
