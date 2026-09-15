package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"wallet-system/pkg/observability"
)

func getEnvOrDefault(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}

// apiKeyMiddleware protects /api/v1/* routes behind a Bearer token when
// LOGGING_API_KEY is set. Health, readiness, and metrics endpoints are
// always unauthenticated.
func apiKeyMiddleware(apiKey string, next http.Handler) http.Handler {
	if apiKey == "" {
		// No key configured — allow all traffic (local dev mode).
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only enforce auth on query API routes.
		if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/api/v1/" {
			bearer := r.Header.Get("Authorization")
			expected := "Bearer " + apiKey
			if bearer != expected {
				http.Error(w, "Unauthorized: valid Authorization: Bearer <token> header required", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// startRetentionScheduler runs a log retention pass every intervalHours hours.
// It deletes non-AUDIT events older than maxAgeDays (AUDIT events are preserved).
func startRetentionScheduler(ctx context.Context, repo LogRepository, maxAgeDays, intervalHours int) {
	if maxAgeDays <= 0 {
		maxAgeDays = 90
	}
	if intervalHours <= 0 {
		intervalHours = 24
	}
	ticker := time.NewTicker(time.Duration(intervalHours) * time.Hour)
	go func() {
		defer ticker.Stop()
		// Run once immediately at startup to catch any stale data.
		runRetention(ctx, repo, maxAgeDays)
		for {
			select {
			case <-ticker.C:
				runRetention(ctx, repo, maxAgeDays)
			case <-ctx.Done():
				log.Println("[LOGGING-SERVICE] Retention scheduler stopped.")
				return
			}
		}
	}()
}

func runRetention(ctx context.Context, repo LogRepository, maxAgeDays int) {
	rCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	deleted, err := repo.Retention(rCtx, maxAgeDays)
	if err != nil {
		log.Printf("[LOGGING-SERVICE] Retention run failed: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("[LOGGING-SERVICE] Retention: deleted %d events older than %d days", deleted, maxAgeDays)
	}
}

func main() {
	log.Println("[LOGGING-SERVICE] Starting up...")

	mongoURI := getEnvOrDefault("MONGO_URI", "mongodb://127.0.0.1:27017")
	rabbitURI := getEnvOrDefault("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
	httpPort := getEnvOrDefault("HTTP_PORT", "8090")
	metricsPort := getEnvOrDefault("METRICS_PORT", "9090")
	apiKey := os.Getenv("LOGGING_API_KEY") // empty → no auth (local dev)

	// Retention configuration (Phase 5).
	retentionDays, _ := strconv.Atoi(getEnvOrDefault("LOGGING_RETENTION_DAYS", "90"))
	retentionIntervalHours, _ := strconv.Atoi(getEnvOrDefault("LOGGING_RETENTION_INTERVAL_HOURS", "24"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialize MongoDB Repository
	repo, err := NewMongoLogRepository(ctx, mongoURI, "logging_db", "events")
	if err != nil {
		log.Fatalf("[FATAL] Failed to initialize MongoDB repository: %v", err)
	}
	defer func() {
		if err := repo.Close(context.Background()); err != nil {
			log.Printf("[ERROR] Failed to close MongoDB connection: %v", err)
		}
	}()

	// 2. Initialize RabbitMQ Consumer
	consumer, err := NewConsumer(rabbitURI, repo)
	if err != nil {
		log.Fatalf("[FATAL] Failed to initialize RabbitMQ consumer: %v", err)
	}
	defer func() {
		if err := consumer.Close(); err != nil {
			log.Printf("[ERROR] Failed to close RabbitMQ consumer: %v", err)
		}
	}()

	// 3. Start Consumer in Background
	go func() {
		log.Println("[LOGGING-SERVICE] Starting RabbitMQ consumer...")
		if err := consumer.Start(ctx); err != nil {
			log.Printf("[ERROR] Consumer stopped with error: %v", err)
		}
	}()

	// 4. Start Retention Scheduler (Phase 5)
	startRetentionScheduler(ctx, repo, retentionDays, retentionIntervalHours)
	log.Printf("[LOGGING-SERVICE] Retention scheduler: maxAgeDays=%d, intervalHours=%d", retentionDays, retentionIntervalHours)

	// 5. Setup HTTP mux for health, readiness, and query API
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := repo.Health(r.Context()); err != nil {
			http.Error(w, "MongoDB Not Ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Ready"))
	})

	// Phase 4: Query API endpoints (gated by API key middleware).
	registerQueryRoutes(mux, repo)

	// Wrap the whole mux with the API key middleware (only /api/v1/* is gated).
	handler := apiKeyMiddleware(apiKey, mux)
	if apiKey != "" {
		log.Printf("[LOGGING-SERVICE] API key authentication enabled on /api/v1/* routes")
	} else {
		log.Printf("[LOGGING-SERVICE] API key authentication disabled (set LOGGING_API_KEY to enable)")
	}

	server := &http.Server{
		Addr:    ":" + httpPort,
		Handler: handler,
	}

	go func() {
		log.Printf("[LOGGING-SERVICE] HTTP listening on :%s (health + query API)", httpPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[FATAL] HTTP server failed: %v", err)
		}
	}()

	// 6. Prometheus metrics server on a dedicated port (Phase 5 / GAP-07).
	// Serve the SDK's MetricsHandler so that Prometheus can scrape all 6 counters.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", observability.DefaultMetrics.Handler())
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	metricsServer := &http.Server{
		Addr:    ":" + metricsPort,
		Handler: metricsMux,
	}
	go func() {
		log.Printf("[LOGGING-SERVICE] Prometheus metrics listening on :%s/metrics", metricsPort)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[ERROR] Metrics server failed: %v", err)
		}
	}()

	// 7. Wait for Shutdown Signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("[LOGGING-SERVICE] Shutting down...")
	cancel() // Stops the consumer loop and retention scheduler.

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ERROR] HTTP server shutdown error: %v", err)
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ERROR] Metrics server shutdown error: %v", err)
	}

	log.Println("[LOGGING-SERVICE] Shutdown complete.")
}
