package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	k8sclient "aegis-stream/dashboard/api/k8s"
	promclient "aegis-stream/dashboard/api/prometheus"
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

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, r *http.Request) {
		metrics, err := prom.FetchMetrics()
		if err != nil {
			log.Printf("ERROR: fetching metrics: %v", err)
			http.Error(w, "failed to fetch metrics", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(metrics)
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
