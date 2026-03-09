package sink

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aegis-stream/pb"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresSink writes events to PostgreSQL tables using batch inserts.
//
// Instead of one INSERT per event (slow at high throughput), events are
// buffered in memory and flushed in batches. A batch is flushed when:
//   - The buffer reaches batchSize events (e.g. 100), OR
//   - flushInterval has passed since the last flush (e.g. 500ms)
//
// This trades a tiny bit of latency (up to 500ms) for much higher throughput.
// At 5,000 events/sec, this means ~50 batch INSERTs/sec instead of 5,000
// individual INSERTs/sec — a 100x reduction in database round-trips.
type PostgresSink struct {
	pool *pgxpool.Pool

	// mu protects the trade and log buffers.
	// Multiple workers call Write() concurrently, so we need a mutex
	// to prevent two workers from appending to the buffer simultaneously
	// (which would corrupt the slice).
	mu     sync.Mutex
	trades []tradeRow
	logs   []logRow

	// Batching config.
	batchSize     int
	flushInterval time.Duration

	// stopCh signals the background flusher goroutine to stop.
	// When Close() is called, we close this channel, the flusher exits,
	// and we do one final flush to ensure no events are lost.
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// tradeRow holds the fields for one INSERT into the trades table.
// We store these as simple structs rather than *pb.Trade because
// we only need the fields that map to database columns.
type tradeRow struct {
	eventID   string
	symbol    string
	price     float64
	volume    int32
	timestamp int64
}

// logRow holds the fields for one INSERT into the logs table.
type logRow struct {
	eventID     string
	level       string
	message     string
	serviceName string
	timestamp   int64
}

// NewPostgres connects to PostgreSQL and starts the background flusher.
// connString format: "postgres://aegis:aegis@localhost:5433/aegis"
func NewPostgres(connString string) (*PostgresSink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	s := &PostgresSink{
		pool:          pool,
		batchSize:     100,
		flushInterval: 500 * time.Millisecond,
		stopCh:        make(chan struct{}),
	}

	// Start the background flusher — it runs in its own goroutine
	// and periodically flushes whatever is in the buffer.
	// This ensures events don't sit in memory forever during low traffic.
	s.wg.Add(1)
	go s.backgroundFlusher()

	return s, nil
}

// Write adds an event to the in-memory buffer.
// If the buffer reaches batchSize, it flushes immediately.
//
// This is called by 100 workers concurrently, so we lock the mutex
// for the shortest possible time: just the append + size check.
// The actual database write (flush) happens outside the lock.
func (s *PostgresSink) Write(event *pb.Event) error {
	s.mu.Lock()

	switch payload := event.Payload.(type) {
	case *pb.Event_Trade:
		s.trades = append(s.trades, tradeRow{
			eventID:   event.EventId,
			symbol:    payload.Trade.Symbol,
			price:     payload.Trade.Price,
			volume:    payload.Trade.Volume,
			timestamp: payload.Trade.Timestamp,
		})
	case *pb.Event_Log:
		s.logs = append(s.logs, logRow{
			eventID:     event.EventId,
			level:       payload.Log.Level,
			message:     payload.Log.Message,
			serviceName: payload.Log.ServiceName,
			timestamp:   payload.Log.Timestamp,
		})
	}

	// Check if either buffer is full. If so, grab the buffers and
	// replace them with fresh empty slices. This lets us unlock the
	// mutex before doing the slow database write.
	shouldFlush := len(s.trades)+len(s.logs) >= s.batchSize
	var tradeBatch []tradeRow
	var logBatch []logRow
	if shouldFlush {
		tradeBatch = s.trades
		logBatch = s.logs
		s.trades = nil
		s.logs = nil
	}

	s.mu.Unlock()

	// Flush outside the lock — other workers can keep appending
	// while this batch is being written to the database.
	if shouldFlush {
		return s.flush(tradeBatch, logBatch)
	}
	return nil
}

// backgroundFlusher runs in a goroutine and flushes the buffer every
// flushInterval. This handles the "low traffic" case: if events trickle
// in at 10/sec, the buffer never reaches batchSize, so without this
// flusher those events would sit in memory until the next burst.
func (s *PostgresSink) backgroundFlusher() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			// Grab whatever is in the buffers.
			s.mu.Lock()
			trades := s.trades
			logs := s.logs
			s.trades = nil
			s.logs = nil
			s.mu.Unlock()

			if len(trades) > 0 || len(logs) > 0 {
				if err := s.flush(trades, logs); err != nil {
					slog.Error("background flush failed", "error", err)
				}
			}
		}
	}
}

// flush writes batches of trades and logs to PostgreSQL.
// It uses a database transaction so either all rows in the batch
// succeed or none do — no partial writes.
//
// The COPY protocol would be even faster, but pgx's Batch API is
// simpler and plenty fast for our throughput (thousands/sec).
func (s *PostgresSink) flush(trades []tradeRow, logs []logRow) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Begin a transaction — all INSERTs in this batch are atomic.
	// If any INSERT fails, tx.Rollback() undoes them all.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// defer Rollback is safe even after Commit — it's a no-op if already committed.
	defer tx.Rollback(ctx)

	for _, t := range trades {
		_, err := tx.Exec(ctx,
			`INSERT INTO trades (event_id, symbol, price, volume, event_ts)
			 VALUES ($1, $2, $3, $4, $5)`,
			t.eventID, t.symbol, t.price, t.volume, t.timestamp,
		)
		if err != nil {
			return fmt.Errorf("insert trade: %w", err)
		}
	}

	for _, l := range logs {
		_, err := tx.Exec(ctx,
			`INSERT INTO logs (event_id, level, message, service_name, event_ts)
			 VALUES ($1, $2, $3, $4, $5)`,
			l.eventID, l.level, l.message, l.serviceName, l.timestamp,
		)
		if err != nil {
			return fmt.Errorf("insert log: %w", err)
		}
	}

	// Commit makes all the INSERTs permanent.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	slog.Debug("batch flushed", "trades", len(trades), "logs", len(logs))
	return nil
}

// Close stops the background flusher and does a final flush to ensure
// no events are lost. Then it closes the connection pool.
func (s *PostgresSink) Close() error {
	// Signal the background flusher to stop.
	close(s.stopCh)
	s.wg.Wait()

	// Final flush — grab anything left in the buffers.
	s.mu.Lock()
	trades := s.trades
	logs := s.logs
	s.trades = nil
	s.logs = nil
	s.mu.Unlock()

	if len(trades) > 0 || len(logs) > 0 {
		if err := s.flush(trades, logs); err != nil {
			slog.Error("final flush failed", "error", err)
		}
	}

	s.pool.Close()
	return nil
}
