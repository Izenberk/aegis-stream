package main

import (
	"context"
	"encoding/binary"
	// "fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"

	"aegis-stream/pb"
	"google.golang.org/protobuf/proto"
)

const (
	Port       = ":9000"
	WorkerPool = 100
)

// Job wraps the payload and its underlying buffer pointer
type Job struct {
	Data []byte
	Buf  *[]byte
}

// bufferPool holds pre-allocated byte slices to prevent GC spikes
var bufferPool = sync.Pool{
	New: func() any {
		// Pre-allocate a generous 4KB buffer for each incoming event
		b := make([]byte, 4096)
		return &b
	},
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listener, err := net.Listen("tcp", Port)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
	log.Printf("Aegis Stream listening on %s (Zero-Copy Enabled)", Port)

	// Update the channel to use our new Job struct
	jobs := make(chan Job, 100000)
	var wg sync.WaitGroup

	for i := 0; i < WorkerPool; i++ {
		wg.Add(1)
		go worker(&wg, i, jobs)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					log.Printf("Failed to accept connection: %v", err)
				}
				continue
			}
			go handleConnection(ctx, conn, jobs)
		}
	}()

	<-sigCh
	log.Println("\nShutdown signal received! Initiating graceful shutdown...")

	listener.Close()
	cancel()
	close(jobs)

	wg.Wait()
	log.Println("All workers drained. Server safely stopped.")
}

func handleConnection(ctx context.Context, conn net.Conn, jobs chan<- Job) {
	defer conn.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		lengthBuf := make([]byte, 4)
		if _, err := io.ReadFull(conn, lengthBuf); err != nil {
			return
		}
		msgLen := binary.BigEndian.Uint32(lengthBuf)

		// 1. Borrow a buffer from the pool
		bufPtr := bufferPool.Get().(*[]byte)

		// 2. Slice it to the exact length of the incoming message
		msgBuf := (*bufPtr)[:msgLen]
		if _, err := io.ReadFull(conn, msgBuf); err != nil {
			log.Printf("Failed to read complete payload: %v", err)
			return
		}

		// 3. Send the job, including the pointer so the worker can return it
		jobs <- Job{Data: msgBuf, Buf: bufPtr}
	}
}

func worker(wg *sync.WaitGroup, id int, jobs <-chan Job) {
	defer wg.Done()

	for job := range jobs {
		var event pb.Event

		if err := proto.Unmarshal(job.Data, &event); err != nil {
			log.Printf("Worker %d: Failed to unmarshal: %v", id, err)
			// Ensure we put the buffer back even if it fails
			bufferPool.Put(job.Buf)
			continue
		}

		switch payload := event.Payload.(type) {
		case *pb.Event_Trade:
			_ = payload
			// fmt.Printf("[Worker %d] Routed Trade | Symbol: %s | Price: %f\n", id, payload.Trade.Symbol, payload.Trade.Price)
		case *pb.Event_Log:
			_ = payload
			// fmt.Printf("[Worker %d] Routed Log   | Level: %s | Msg: %s\n", id, payload.Log.Level, payload.Log.Message)
		}

		// 4. Return the buffer to the pool for the next connection to use
		bufferPool.Put(job.Buf)
	}
}