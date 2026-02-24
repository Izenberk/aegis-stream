package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"runtime"

	"aegis-stream/pb"

	"google.golang.org/protobuf/proto"
)

const (
	Port       = ":9000"
	WorkerPool = 100 // We can tune this later based on your CPU cores
)

func main() {
	// Optimize Go runtime to utilize all available CPU cores
	runtime.GOMAXPROCS(runtime.NumCPU())

	listener, err := net.Listen("tcp", Port)
	if err != nil {
		log.Fatalf("Failed to start Aegis Stream server: %v", err)
	}
	defer listener.Close()
	log.Printf("Aegis Stream TCP Server listening on %s", Port)

	// Create a buffered channel to act as our high-speed task queue
	jobs := make(chan []byte, 100000)

	// Ignite the worker pool
	for i := 0; i < WorkerPool; i++ {
		go worker(i, jobs)
	}

	// Continuously accept incoming TCP connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %v", err)
			continue
		}
		// Pass the connection to a handler
		go handleConnection(conn, jobs)
	}
}

func handleConnection(conn net.Conn, jobs chan<- []byte) {
	defer conn.Close()

	// For Phase 1, we use a scanner to read incoming byte streams.
	// In production, we will upgrade this to exact length-prefix framing.
	scanner := bufio.NewScanner(conn)

	for scanner.Scan() {
		// Copy the bytes so they aren't overwritten by the next scan iteration
		data := make([]byte, len(scanner.Bytes()))
		copy(data, scanner.Bytes())

		// Push the raw payload into the task queue
		jobs <- data
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Connection error: %v", err)
	}
}

func worker(id int, jobs <-chan []byte) {
	for data := range jobs {
		var event pb.Event

		// Decode the Protobuf payload
		if err := proto.Unmarshal(data, &event); err != nil {
			log.Printf("Worker %d: Failed to unmarshal: %v", id, err)
			continue
		}

		// Route the event based on its inner type
		switch payload := event.Payload.(type) {
		case *pb.Event_Trade:
			fmt.Printf("[Worker %d] Routed Trade | Symbol: %s | Price: %f\n", id, payload.Trade.Symbol, payload.Trade.Price)
		case *pb.Event_Log:
			fmt.Printf("[Worker %d] Routed Log   | Level: %s | Msg: %s\n", id, payload.Log.Level, payload.Log.Message)
		}
	}
}
