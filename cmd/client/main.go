package main

import (
	"log"
	"net"
	"time"

	"aegis-stream/internal/frame"
	"aegis-stream/pb"
	"google.golang.org/protobuf/proto"
)

func main() {
	conn, err := net.Dial("tcp", "localhost:9000")
	if err != nil {
		log.Fatalf("Failed to connect to Aegis Stream: %v", err)
	}
	defer conn.Close()

	events := []*pb.Event{
		{
			EventId: "evt-001",
			Payload: &pb.Event_Trade{
				Trade: &pb.Trade{Symbol: "BTC-USD", Price: 64230.50, Volume: 5, Timestamp: time.Now().UnixNano()},
			},
		},
		{
			EventId: "evt-002",
			Payload: &pb.Event_Log{
				Log: &pb.Log{Level: "WARN", Message: "High latency detected", ServiceName: "matching-engine", Timestamp: time.Now().UnixNano()},
			},
		},
		{
			EventId: "evt-003",
			Payload: &pb.Event_Trade{
				Trade: &pb.Trade{Symbol: "AAPL", Price: 185.20, Volume: 100, Timestamp: time.Now().UnixNano()},
			},
		},
	}

	for _, ev := range events {
		data, err := proto.Marshal(ev)
		if err != nil {
			log.Fatalf("Failed to marshal event: %v", err)
		}

		// Use the shared frame package instead of inline encoding/binary calls
		if err := frame.Write(conn, data); err != nil {
			log.Fatalf("Failed to send frame: %v", err)
		}

		log.Printf("Sent event: %s", ev.EventId)
		time.Sleep(1 * time.Second)
	}
}
