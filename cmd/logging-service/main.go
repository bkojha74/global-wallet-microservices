package main

import (
	"context"
	"fmt"
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

func runLoggingServer(ctx context.Context) error {
	log.Println("[LOGGING-SERVICE] Starting up...")

	mongoURI := getEnvOrDefault("MONGO_URI", "mongodb://127.0.0.1:27017")
	rabbitURI := getEnvOrDefault("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
	httpPort := getEnvOrDefault("HTTP_PORT", "8090")
	metricsPort := getEnvOrDefault("METRICS_PORT", "9090")
	apiKey := os.Getenv("LOGGING_API_KEY") // empty → no auth (local dev)

	// Retention configuration (Phase 5).
	retentionDays, _ := strconv.Atoi(getEnvOrDefault("LOGGING_RETENTION_DAYS", "90"))
	retentionIntervalHours, _ := strconv.Atoi(getEnvOrDefault("LOGGING_RETENTION_INTERVAL_HOURS", "24"))

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var repo LogRepository
	if os.Getenv("TEST_MOCK_DB") == "true" {
		log.Println("[LOGGING-SERVICE] TEST_MOCK_DB=true — using mock repository")
		repo = &mockLogRepoImpl{}
	} else {
		// 1. Initialize MongoDB Repository
		var err error
		repo, err = NewMongoLogRepository(subCtx, mongoURI, "logging_db", "events")
		if err != nil {
			return fmt.Errorf("failed to initialize MongoDB repository: %w", err)
		}
		defer func() {
			if mongoRepo, ok := repo.(*MongoLogRepository); ok {
				if err := mongoRepo.Close(context.Background()); err != nil {
					log.Printf("[ERROR] Failed to close MongoDB connection: %v", err)
				}
			}
		}()

		// 2. Initialize RabbitMQ Consumer
		consumer, err := NewConsumer(rabbitURI, repo)
		if err != nil {
			return fmt.Errorf("failed to initialize RabbitMQ consumer: %w", err)
		}
		defer func() {
			if err := consumer.Close(); err != nil {
				log.Printf("[ERROR] Failed to close RabbitMQ consumer: %v", err)
			}
		}()

		// 3. Start Consumer in Background
		go func() {
			log.Println("[LOGGING-SERVICE] Starting RabbitMQ consumer...")
			if err := consumer.Start(subCtx); err != nil {
				log.Printf("[ERROR] Consumer stopped with error: %v", err)
			}
		}()
	}

	// 4. Start Retention Scheduler (Phase 5)
	startRetentionScheduler(subCtx, repo, retentionDays, retentionIntervalHours)
	log.Printf("[LOGGING-SERVICE] Retention scheduler: maxAgeDays=%d, intervalHours=%d", retentionDays, retentionIntervalHours)

	// 5. Setup HTTP server for health, readiness, and query API
	handler := buildHTTPHandler(repo, apiKey)
	server := &http.Server{
		Addr:              ":" + httpPort,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("[LOGGING-SERVICE] HTTP listening on :%s (health + query API)", httpPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[ERROR] HTTP server failed: %v", err)
		}
	}()

	// 6. Prometheus metrics server on a dedicated port
	metricsServer := startMetricsServer(metricsPort)

	// 7. Wait for Shutdown Signal
	<-subCtx.Done()

	log.Println("[LOGGING-SERVICE] Shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ERROR] HTTP server shutdown error: %v", err)
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ERROR] Metrics server shutdown error: %v", err)
	}

	log.Println("[LOGGING-SERVICE] Shutdown complete.")
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runLoggingServer(ctx); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
}

func buildHTTPHandler(repo LogRepository, apiKey string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := repo.Health(r.Context()); err != nil {
			http.Error(w, "MongoDB Not Ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Ready"))
	})
	registerQueryRoutes(mux, repo)
	if apiKey != "" {
		log.Printf("[LOGGING-SERVICE] API key authentication enabled on /api/v1/* routes")
	} else {
		log.Printf("[LOGGING-SERVICE] API key authentication disabled (set LOGGING_API_KEY to enable)")
	}
	return apiKeyMiddleware(apiKey, mux)
}

func startMetricsServer(metricsPort string) *http.Server {
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", observability.DefaultMetrics.Handler())
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr:              ":" + metricsPort,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("[LOGGING-SERVICE] Prometheus metrics listening on :%s/metrics", metricsPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[ERROR] Metrics server failed: %v", err)
		}
	}()
	return server
}
