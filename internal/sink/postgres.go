package sink

import (
	"context"
	"fmt"
	"time"

	"aegis-stream/pb"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresSink writes events to PostgreSQL tables.
// Trades go to the "trades" table, Logs go to the "logs" table.
//
// It uses pgxpool.Pool — a connection pool that manages multiple
// database connections automatically. This is important because:
//   - 100 workers call Write() concurrently
//   - Each Write() needs a DB connection
//   - Without pooling, we'd open/close connections constantly (slow)
//   - The pool reuses connections, limiting total open connections
type PostgresSink struct {
	pool *pgxpool.Pool
}

// NewPostgres connects to PostgreSQL and returns a ready-to-use sink.
// connString format: "postgres://aegis:aegis@localhost:5433/aegis"
//
// pgxpool.New() does three things:
//  1. Parses the connection string
//  2. Opens the initial connections
//  3. Returns a pool that auto-scales connections as needed
func NewPostgres(connString string) (*PostgresSink, error) {
	// context.Background() is used here because this runs at startup,
	// not inside a request. We add a 5-second timeout so the server
	// doesn't hang forever if the database is unreachable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Ping verifies the connection actually works.
	// pgxpool.New() can succeed even with a bad password — it's lazy.
	// Ping forces a real round-trip to the database.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &PostgresSink{pool: pool}, nil
}

// Write inserts a single event into the appropriate table.
// It switches on the event type (Trade vs Log) and runs the matching INSERT.
//
// pool.Exec() grabs a connection from the pool, runs the query,
// and returns the connection to the pool — all automatically.
// The $1, $2, $3 placeholders prevent SQL injection (never use fmt.Sprintf for queries).
func (s *PostgresSink) Write(event *pb.Event) error {
	ctx := context.Background()

	switch payload := event.Payload.(type) {
	case *pb.Event_Trade:
		_, err := s.pool.Exec(ctx,
			`INSERT INTO trades (event_id, symbol, price, volume, event_ts)
			 VALUES ($1, $2, $3, $4, $5)`,
			event.EventId,
			payload.Trade.Symbol,
			payload.Trade.Price,
			payload.Trade.Volume,
			payload.Trade.Timestamp,
		)
		if err != nil {
			return fmt.Errorf("insert trade: %w", err)
		}

	case *pb.Event_Log:
		_, err := s.pool.Exec(ctx,
			`INSERT INTO logs (event_id, level, message, service_name, event_ts)
			 VALUES ($1, $2, $3, $4, $5)`,
			event.EventId,
			payload.Log.Level,
			payload.Log.Message,
			payload.Log.ServiceName,
			payload.Log.Timestamp,
		)
		if err != nil {
			return fmt.Errorf("insert log: %w", err)
		}
	}

	return nil
}

// Close shuts down the connection pool.
// Called during server shutdown — waits for in-flight queries to finish,
// then closes all connections to PostgreSQL.
func (s *PostgresSink) Close() error {
	s.pool.Close()
	return nil
}
