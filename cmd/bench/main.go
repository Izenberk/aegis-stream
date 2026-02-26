package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"time"

	"aegis-stream/pb"
	"google.golang.org/protobuf/proto"
)

const (
	TargetEvents = 100000
	ServerAddr   = "localhost:9000"
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

	lengthBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBuf, uint32(len(data)))
	frame := append(lengthBuf, data...)

	fmt.Println("Connecting to server...")
	conn, err := net.Dial("tcp", ServerAddr)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	fmt.Printf("Blasting %d events...\n", TargetEvents)
	start := time.Now()

	for i := 0; i < TargetEvents; i++ {
		_, err := conn.Write(frame)
		if err != nil {
			log.Fatalf("Write failed at event %d: %v", i, err)
		}
	}

	elapsed := time.Since(start)
	eventsPerSec := float64(TargetEvents) / elapsed.Seconds()

	fmt.Printf("\n--- Benchmark Results ---\n")
	fmt.Printf("Total Time:   %v\n", elapsed)
	fmt.Printf("Throughput:   %.2f events/sec\n", eventsPerSec)
}