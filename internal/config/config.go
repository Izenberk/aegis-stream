package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all tunable server parameters.
// In K8s, these are set via env vars in the Deployment manifest.
// Locally, you can override them with command-line flags.
type Config struct {
	Port        string        // TCP listen address, e.g. ":9000"
	MetricsPort string        // HTTP address for Prometheus /metrics endpoint
	Workers     int           // Number of worker goroutines in the pool
	QueueDepth  int           // Buffered channel capacity (backpressure threshold)
	MaxConns     int           // Maximum simultaneous TCP connections (0 = unlimited)
	ReadTimeout  time.Duration // How long to wait for data before dropping a slow client
	ProcessDelay time.Duration // Artificial per-event delay to simulate real I/O (0 = none)

	// Sink configuration: where processed events are routed.
	// "stdout" = log to console (default, zero dependencies)
	// "postgres" = write to PostgreSQL (requires AEGIS_POSTGRES_URL)
	SinkType    string // "stdout" or "postgres"
	PostgresURL string // e.g. "postgres://aegis:aegis@localhost:5433/aegis"
}

// Load reads configuration with this priority (highest wins):
//   1. Defaults (hardcoded safe values)
//   2. Command-line flags (local dev convenience)
//   3. Environment variables (K8s / Docker override)
//
// Why env vars override flags? In K8s, you can't pass flags to a container
// easily, but env vars are first-class in Deployment manifests.
func Load() (*Config, error) {
	cfg := &Config{}

	// Step 1: Define flags with sensible defaults.
	flag.StringVar(&cfg.Port, "port", ":9000", "TCP listen address")
	flag.StringVar(&cfg.MetricsPort, "metrics-port", ":2112", "HTTP address for /metrics")
	flag.IntVar(&cfg.Workers, "workers", 100, "Number of worker goroutines")
	flag.IntVar(&cfg.QueueDepth, "queue-depth", 100000, "Job channel buffer size")
	flag.IntVar(&cfg.MaxConns, "max-conns", 1000, "Max simultaneous TCP connections")
	flag.DurationVar(&cfg.ReadTimeout, "read-timeout", 30*time.Second, "TCP read deadline per frame")
	flag.DurationVar(&cfg.ProcessDelay, "process-delay", 0, "Per-event processing delay to simulate I/O (e.g. 5ms)")
	flag.StringVar(&cfg.SinkType, "sink", "stdout", "Event sink type: stdout or postgres")
	flag.StringVar(&cfg.PostgresURL, "postgres-url", "", "PostgreSQL connection string (required when sink=postgres)")
	flag.Parse()

	// Step 2: If an env var is set, it overrides the flag value.
	// os.LookupEnv returns (value, exists) — we only override when exists is true,
	// so an unset env var doesn't wipe out the flag/default.
	if v, ok := os.LookupEnv("AEGIS_PORT"); ok {
		cfg.Port = v
	}
	if v, ok := os.LookupEnv("AEGIS_METRICS_PORT"); ok {
		cfg.MetricsPort = v
	}
	if v, ok := os.LookupEnv("AEGIS_WORKERS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid AEGIS_WORKERS %q: %w", v, err)
		}
		cfg.Workers = n
	}
	if v, ok := os.LookupEnv("AEGIS_QUEUE_DEPTH"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid AEGIS_QUEUE_DEPTH %q: %w", v, err)
		}
		cfg.QueueDepth = n
	}
	if v, ok := os.LookupEnv("AEGIS_MAX_CONNS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid AEGIS_MAX_CONNS %q: %w", v, err)
		}
		cfg.MaxConns = n
	}
	if v, ok := os.LookupEnv("AEGIS_READ_TIMEOUT"); ok {
		d, err := time.ParseDuration(v) // e.g. "30s", "1m", "500ms"
		if err != nil {
			return nil, fmt.Errorf("invalid AEGIS_READ_TIMEOUT %q: %w", v, err)
		}
		cfg.ReadTimeout = d
	}
	if v, ok := os.LookupEnv("AEGIS_PROCESS_DELAY"); ok {
		d, err := time.ParseDuration(v) // e.g. "5ms", "10ms"
		if err != nil {
			return nil, fmt.Errorf("invalid AEGIS_PROCESS_DELAY %q: %w", v, err)
		}
		cfg.ProcessDelay = d
	}
	// Sink config: AEGIS_SINK selects the destination, AEGIS_POSTGRES_URL provides the connection.
	// In K8s, you'd set these in the Deployment manifest or the AegisPipeline CR.
	if v, ok := os.LookupEnv("AEGIS_SINK"); ok {
		cfg.SinkType = v
	}
	if v, ok := os.LookupEnv("AEGIS_POSTGRES_URL"); ok {
		cfg.PostgresURL = v
	}

	// Step 3: Validate — catch bad config before the server starts,
	// not minutes later when something silently breaks.
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validate checks that config values are within sane ranges.
func (c *Config) validate() error {
	if c.Workers <= 0 {
		return fmt.Errorf("workers must be positive, got %d", c.Workers)
	}
	if c.QueueDepth <= 0 {
		return fmt.Errorf("queue-depth must be positive, got %d", c.QueueDepth)
	}
	if c.MaxConns < 0 {
		return fmt.Errorf("max-conns must be non-negative, got %d", c.MaxConns)
	}
	if c.ReadTimeout < 0 {
		return fmt.Errorf("read-timeout must be non-negative, got %v", c.ReadTimeout)
	}
	// Validate sink: only "stdout" and "postgres" are supported.
	// If postgres is selected, a connection URL is required.
	switch c.SinkType {
	case "stdout":
		// No extra config needed.
	case "postgres":
		if c.PostgresURL == "" {
			return fmt.Errorf("sink=postgres requires AEGIS_POSTGRES_URL or -postgres-url")
		}
	default:
		return fmt.Errorf("unknown sink type %q (supported: stdout, postgres)", c.SinkType)
	}
	return nil
}
