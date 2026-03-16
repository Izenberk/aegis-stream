package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"aegis-stream/pb"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func main() {
	// Config: flags with env var overrides, same pattern as the main server.
	natsURL := flag.String("nats-url", "nats://localhost:4222", "NATS server URL")
	subject := flag.String("subject", "aegis.trades", "NATS subject to subscribe to")
	threshold := flag.Float64("threshold", 75000, "Price alert threshold (USD)")
	flag.Parse()

	// Env vars override flags — standard pattern for K8s where you can't
	// easily pass flags, but env vars are first-class in Deployment manifests.
	if v, ok := os.LookupEnv("AEGIS_CONSUMER_NATS_URL"); ok {
		*natsURL = v
	}
	if v, ok := os.LookupEnv("AEGIS_CONSUMER_SUBJECT"); ok {
		*subject = v
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	slog.Info("consumer starting",
		"nats_url", *natsURL,
		"subject", *subject,
		"threshold", *threshold,
	)

	// Connect to NATS. The consumer is a client just like the publisher —
	// NATS doesn't distinguish between them. Both use nats.Connect().
	conn, err := nats.Connect(*natsURL)
	if err != nil {
		slog.Error("failed to connect to NATS", "error", err)
		os.Exit(1)
	}
	defer conn.Drain()

	// JetStream context from connection
	js, err := jetstream.New(conn)
	if err != nil {
		slog.Error("failed to create JetStream context", "error", err)
		os.Exit(1)
	}

	// Durable consumer on the AEGIS stream
	cons, err := js.CreateOrUpdateConsumer(context.Background(), "AEGIS", jetstream.ConsumerConfig{
		Name:  			"price-alerts",
		Durable:  		"price-alerts",
		FilterSubjects: 	[]string{*subject},
		AckPolicy:  	jetstream.AckExplicitPolicy,
	})
	if err != nil {
		slog.Error("failed to create consumer", "error", err)
		os.Exit(1)
	}

	// Counters for the stats ticker. We use atomic operations because
	// the subscription callback runs in NATS's internal goroutine pool,
	// and the stats ticker runs in main — concurrent access.
	var msgCount atomic.Int64
	var alertCount atomic.Int64

	// Subscribe to the subject. The callback fires for every message.
	// NATS manages the goroutine pool internally — we don't need to
	// create workers like the main server does.
	_, err = cons.Consume(func(msg jetstream.Msg) {
		var event pb.Event
		if err := proto.Unmarshal(msg.Data(), &event); err != nil {
			slog.Error("unmarshal failed", "error", err)
			return
		}

		msgCount.Add(1)

		switch payload := event.Payload.(type) {
		case *pb.Event_Trade:
			t := payload.Trade
			slog.Info("trade",
				"symbol", t.Symbol,
				"price", fmt.Sprintf("%.2f", t.Price),
				"volume", t.Volume,
			)

			// Price alert: this is our "business logic" — a simple
			// threshold check that demonstrates consuming events and
			// taking action. In production, this could send a Slack
			// notification, trigger an order, or update a dashboard.
			if t.Price > *threshold {
				alertCount.Add(1)
				slog.Warn("PRICE ALERT",
					"symbol", t.Symbol,
					"price", fmt.Sprintf("%.2f", t.Price),
					"threshold", *threshold,
				)
			}

		case *pb.Event_Log:
			l := payload.Log
			slog.Info("log",
				"level", l.Level,
				"message", l.Message,
				"service", l.ServiceName,
			)
		}
		msg.Ack()
	})
	if err != nil {
		slog.Error("failed to subscribe", "error", err)
		os.Exit(1)
	}

	slog.Info("subscribed", "subject", *subject)

	// Stats ticker: print a summary every 5 seconds so you can see
	// throughput at a glance without scrolling through individual events.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			slog.Info("stats",
				"messages", msgCount.Load(),
				"alerts", alertCount.Load(),
			)
		case sig := <-sigCh:
			slog.Info("shutdown signal received", "signal", sig)
			return
		}
	}
}
