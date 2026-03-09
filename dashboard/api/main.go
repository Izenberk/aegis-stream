package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	k8sclient "aegis-stream/dashboard/api/k8s"
	promclient "aegis-stream/dashboard/api/prometheus"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	prometheusURL := envOr("PROMETHEUS_URL", "http://prometheus:9090")
	pipelineName := envOr("AEGIS_PIPELINE_NAME", "aegis-pipeline")
	namespace := envOr("AEGIS_NAMESPACE", "default")
	listenAddr := envOr("LISTEN_ADDR", ":8080")

	prom := promclient.NewClient(prometheusURL)

	// K8s client — may fail outside a cluster, that's OK for local dev.
	k8s, err := k8sclient.NewClient(namespace, pipelineName)
	if err != nil {
		log.Printf("WARNING: K8s client init failed (expected outside cluster): %v", err)
	}

	// PostgreSQL connection — optional. If AEGIS_POSTGRES_URL is set,
	// the /api/trades endpoint returns recent trades from the database.
	// If not set, /api/trades returns an empty array (dashboard still works).
	var dbPool *pgxpool.Pool
	if pgURL := envOr("AEGIS_POSTGRES_URL", ""); pgURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pool, err := pgxpool.New(ctx, pgURL)
		if err != nil {
			log.Printf("WARNING: PostgreSQL connection failed: %v", err)
		} else if err := pool.Ping(ctx); err != nil {
			log.Printf("WARNING: PostgreSQL ping failed: %v", err)
			pool.Close()
		} else {
			dbPool = pool
			defer dbPool.Close()
			log.Printf("  PostgreSQL: connected")
		}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("DEBUG: /api/metrics called")
		metrics, err := prom.FetchMetrics()
		if err != nil {
			log.Printf("ERROR: fetching metrics: %v", err)
			http.Error(w, "failed to fetch metrics", http.StatusBadGateway)
			return
		}
		log.Printf("DEBUG: metrics fetched OK, totalEvents=%.0f", metrics.TotalEvents)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(metrics); err != nil {
			log.Printf("ERROR: encoding metrics JSON: %v", err)
		}
		log.Printf("DEBUG: response written")
	})

	mux.HandleFunc("GET /api/pipeline", func(w http.ResponseWriter, r *http.Request) {
		if k8s == nil {
			// Return mock data when running outside a cluster.
			mock := k8sclient.PipelineInfo{
				Name: pipelineName,
				Spec: k8sclient.PipelineSpec{
					Replicas:       2,
					Workers:        50,
					QueueDepth:     100000,
					Port:           9000,
					MetricsPort:    2112,
					CostPerPodHour: "0.05",
					Image:          "aegis-stream:latest",
				},
				Status: k8sclient.PipelineStatus{
					ReadyReplicas: 2,
					Phase:         "Running",
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mock)
			return
		}

		info, err := k8s.FetchPipeline()
		if err != nil {
			log.Printf("ERROR: fetching pipeline: %v", err)
			http.Error(w, "failed to fetch pipeline", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})

	// Recent trades from PostgreSQL.
	// Returns the last 50 trades ordered by newest first.
	// If PostgreSQL isn't configured, returns an empty array so
	// the dashboard still works (just without the trades table).
	mux.HandleFunc("GET /api/trades", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if dbPool == nil {
			json.NewEncoder(w).Encode([]struct{}{})
			return
		}

		rows, err := dbPool.Query(r.Context(),
			`SELECT event_id, symbol, price, volume, event_ts, created_at
			 FROM trades ORDER BY created_at DESC LIMIT 50`)
		if err != nil {
			log.Printf("ERROR: querying trades: %v", err)
			http.Error(w, "failed to query trades", http.StatusBadGateway)
			return
		}
		defer rows.Close()

		type trade struct {
			EventID   string    `json:"eventId"`
			Symbol    string    `json:"symbol"`
			Price     float64   `json:"price"`
			Volume    int32     `json:"volume"`
			EventTS   int64     `json:"eventTs"`
			CreatedAt time.Time `json:"createdAt"`
		}

		var trades []trade
		for rows.Next() {
			var t trade
			if err := rows.Scan(&t.EventID, &t.Symbol, &t.Price, &t.Volume, &t.EventTS, &t.CreatedAt); err != nil {
				log.Printf("ERROR: scanning trade row: %v", err)
				continue
			}
			trades = append(trades, t)
		}

		if trades == nil {
			trades = []trade{} // Return [] not null in JSON.
		}
		json.NewEncoder(w).Encode(trades)
	})

	// CORS middleware for local dev (Vite runs on :5173).
	handler := corsMiddleware(mux)

	log.Printf("Dashboard API listening on %s", listenAddr)
	log.Printf("  Prometheus: %s", prometheusURL)
	log.Printf("  Pipeline:   %s/%s", namespace, pipelineName)
	log.Fatal(http.ListenAndServe(listenAddr, handler))
}

func corsMiddleware(next http.Handler) http.Handler {
	allowedOrigin := envOr("CORS_ORIGIN", "")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only set CORS headers when an explicit origin is configured (local dev).
		// In production, Nginx serves both the frontend and proxies /api, so
		// requests are same-origin and CORS headers are unnecessary.
		if allowedOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
