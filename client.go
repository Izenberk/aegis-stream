package main

import (
	"encoding/binary"
	"log"
	"net"
	"time"

	"aegis-stream/pb"
	"google.golang.org/protobuf/proto"
)

func main() {
	// Connect to the local TCP server
	conn, err := net.Dial("tcp", "localhost:9000")
	if err != nil {
		log.Fatalf("Failed to connect to Aegis Stream: %v", err)
	}
	defer conn.Close()

	// Create a slice of dummy events
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

	// Send them one by one
	for _, ev := range events {
		// 1. Serialize the Protobuf struct to binary
		data, err := proto.Marshal(ev)
		if err != nil {
			log.Fatalf("Failed to marshal event: %v", err)
		}

		// 2. Create the 4-byte length prefix
		lengthBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lengthBuf, uint32(len(data)))

		// 3. Write prefix, then payload
		conn.Write(lengthBuf)
		conn.Write(data)

		log.Printf("Sent event: %s", ev.EventId)

		// Pause for 1 second so we can read the server output easily
		time.Sleep(1 * time.Second)
	}
}