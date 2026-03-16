package sink

import (
	"context"
	"fmt"

	"aegis-stream/pb"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// NATSSink publishes events to NATS subjects.
//
// Unlike PostgresSink, no batching is needed here. NATS Publish() writes
// to an internal buffer and the client flushes it asynchronously — so each
// call is just a memory copy, not a network round-trip.
//
// Events are routed to subjects by type:
//   - *pb.Event_Trade → "aegis.trades"
//   - *pb.Event_Log   → "aegis.logs"
//
// This enables fan-out: multiple independent consumers can subscribe to
// different subjects without the publisher knowing about them.
type NATSSink struct {
	conn *nats.Conn
	js	jetstream.JetStream
}

// NewNATS connects to a NATS server and returns a ready-to-use sink.
// url format: "nats://localhost:4222" or "nats://aegis-nats:4222" in K8s.
func NewNATS(url string) (*NATSSink, error) {
	conn, err := nats.Connect(url)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS at %s: %w", url, err)
	}

	// Verify we're actually connected (not in a reconnecting state).
	if conn.Status() != nats.CONNECTED {
		conn.Close()
		return nil, fmt.Errorf("NATS connection not ready, status: %v", conn.Status())
	}

	// Create a JetStream context from the connection.
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create JetStream context: %w", err)
	}

	// CreateOrUpdateStream is idempotent — safe to call on every startup.
    // This ensures the AEGIS stream exists and captures both subjects.
	_, err = js.CreateOrUpdateStream(context.Background(), jetstream.StreamConfig{
		Name:		"AEGIS",
		Subjects:	[]string{"aegis.trades", "aegis.logs"},
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}

	return &NATSSink{conn: conn, js: js}, nil
}

// Write serializes the event with protobuf and publishes it to a
// subject determined by the event type. Consumers unmarshal the
// same *pb.Event on the other end.
func (s *NATSSink) Write(event *pb.Event) error {
	// Pick the subject based on event type.
	var subject string
	switch event.Payload.(type) {
	case *pb.Event_Trade:
		subject = "aegis.trades"
	case *pb.Event_Log:
		subject = "aegis.logs"
	default:
		return fmt.Errorf("unknown event type: %T", event.Payload)
	}

	// Serialize the full Event (not just the inner Trade/Log).
	// This way consumers get event_id, timestamps, and the payload
	// in one message — same format workers use internally.
	data, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	// JetStream Publish returns a PubAck — the server confirms the
	// message was persisted to the stream before this returns.
	// Core NATS Publish is fire-and-forget; this is guaranteed delivery.
	_, err = s.js.Publish(context.Background(), subject, data)
	if err != nil {
		return fmt.Errorf("failed to publish to %s: %w", subject, err)
	}

	return nil
}

// Close drains the NATS connection: flushes any buffered messages to the
// server, then closes the connection. This ensures no events are lost
// during shutdown — same guarantee as PostgresSink's final flush.
func (s *NATSSink) Close() error {
	return s.conn.Drain()
}
