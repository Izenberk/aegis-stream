package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"aegis-stream/internal/config"
	"aegis-stream/internal/frame"
	"aegis-stream/internal/metrics"
	"aegis-stream/internal/sink"
	"aegis-stream/pb"
	"google.golang.org/protobuf/proto"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("bad config", "error", err)
		os.Exit(1)
	}

	metrics.Register()

	go func() {
		slog.Info("metrics server starting", "addr", cfg.MetricsPort)
		if err := metrics.Serve(cfg.MetricsPort); err != nil {
			slog.Error("metrics server failed", "error", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listener, err := net.Listen("tcp", cfg.Port)
	if err != nil {
		slog.Error("failed to start server", "error", err)
		os.Exit(1)
	}
	slog.Info("server started",
		"port", cfg.Port,
		"workers", cfg.Workers,
		"queue_depth", cfg.QueueDepth,
		"max_conns", cfg.MaxConns,
		"read_timeout", cfg.ReadTimeout,
		"process_delay", cfg.ProcessDelay,
	)

	// Create the event sink based on config.
	// The sink is where processed events end up — stdout for dev, postgres for production.
	// Using the Sink interface means workers don't know or care which one they're writing to.
	var eventSink sink.Sink
	switch cfg.SinkType {
	case "postgres":
		s, err := sink.NewPostgres(cfg.PostgresURL)
		if err != nil {
			slog.Error("failed to connect to postgres", "error", err)
			os.Exit(1)
		}
		eventSink = s
	case "nats":
		s, err := sink.NewNATS(cfg.NATSURL)
		if err != nil {
			slog.Error("failed to connect to nats", "error", err)
			os.Exit(1)
		}
		eventSink = s
	default:
		eventSink = sink.NewStdout()
	}
	defer eventSink.Close()

	slog.Info("sink configured", "type", cfg.SinkType)

	jobs := make(chan []byte, cfg.QueueDepth)
	var wg sync.WaitGroup

	// connSem is a counting semaphore that limits simultaneous connections.
	// A buffered channel of empty structs acts as a semaphore:
	//   - Send to acquire a slot (blocks if full = at max connections)
	//   - Receive to release a slot
	// This prevents file descriptor exhaustion under heavy load.
	connSem := make(chan struct{}, cfg.MaxConns)

	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go worker(&wg, i, jobs, cfg.ProcessDelay, eventSink)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					slog.Warn("failed to accept connection", "error", err)
				}
				continue
			}

			// Try to acquire a connection slot. If the semaphore is full,
			// we've hit MaxConns — reject the client immediately.
			select {
			case connSem <- struct{}{}:
				// Slot acquired, handle the connection
				go handleConnection(ctx, conn, jobs, connSem, cfg.ReadTimeout)
			default:
				// At capacity — close the connection so the client knows to retry.
				// Better to reject fast than to let the OS queue connections silently.
				slog.Warn("connection rejected, at max capacity",
					"remote", conn.RemoteAddr().String(),
					"max_conns", cfg.MaxConns,
				)
				conn.Close()
			}
		}
	}()

	<-sigCh
	slog.Info("shutdown signal received")

	metrics.SetShutdown()

	listener.Close()
	cancel()
	close(jobs)

	wg.Wait()
	slog.Info("all workers drained, server stopped")
}

func handleConnection(ctx context.Context, conn net.Conn, jobs chan<- []byte, connSem <-chan struct{}, readTimeout time.Duration) {
	defer conn.Close()
	defer func() { <-connSem }() // Release the semaphore slot when this connection ends

	metrics.ActiveConnections.Inc()
	defer metrics.ActiveConnections.Dec()

	remote := conn.RemoteAddr().String()
	slog.Info("client connected", "remote", remote)
	defer slog.Info("client disconnected", "remote", remote)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// SetReadDeadline tells the OS: "if no data arrives within this duration,
		// return a timeout error." This prevents a slow or stalled client from
		// holding a goroutine (and a connection slot) forever.
		// The deadline resets on every iteration — each frame gets a fresh window.
		conn.SetReadDeadline(time.Now().Add(readTimeout))

		data, err := frame.Read(conn)
		if err != nil {
			// Don't log timeout errors as warnings — they're expected when
			// idle clients get cleaned up. Only unexpected errors matter.
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				slog.Info("client timed out", "remote", remote, "timeout", readTimeout)
			}
			return
		}

		// Backpressure warning: if the queue is more than 80% full, log a warning.
		// This is an early signal that workers can't keep up — time to scale in K8s.
		queueLen := len(jobs)
		queueCap := cap(jobs)
		metrics.QueueDepth.Set(float64(queueLen))
		if queueLen > queueCap*80/100 {
			slog.Warn("queue backpressure",
				"depth", queueLen,
				"capacity", queueCap,
				"percent", queueLen*100/queueCap,
			)
		}

		jobs <- data
	}
}

func worker(wg *sync.WaitGroup, id int, jobs <-chan []byte, processDelay time.Duration, s sink.Sink) {
	defer wg.Done()

	// recover() catches panics inside this goroutine.
	// Without this, a panic kills the worker silently — the WaitGroup
	// never decrements, and the server hangs on shutdown.
	// With this, the worker logs the panic and exits cleanly.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("worker panic recovered", "worker", id, "panic", r)
		}
	}()

	for data := range jobs {
		start := time.Now()

		var event pb.Event

		if err := proto.Unmarshal(data, &event); err != nil {
			slog.Error("unmarshal failed", "worker", id, "error", err)
			metrics.EventErrors.Inc()
			continue
		}

		// Update Prometheus metrics based on event type.
		switch event.Payload.(type) {
		case *pb.Event_Trade:
			metrics.EventsProcessed.WithLabelValues("trade").Inc()
		case *pb.Event_Log:
			metrics.EventsProcessed.WithLabelValues("log").Inc()
		}

		// Route the event to the configured sink (stdout, PostgreSQL, etc.).
		if err := s.Write(&event); err != nil {
			slog.Error("sink write failed", "worker", id, "error", err)
			metrics.EventErrors.Inc()
		}

		// Simulate real-world processing time (e.g. DB write, HTTP call).
		// Set AEGIS_PROCESS_DELAY=5ms to create artificial backpressure for HPA testing.
		if processDelay > 0 {
			time.Sleep(processDelay)
		}

		metrics.ProcessingDuration.Observe(time.Since(start).Seconds())
	}
}
