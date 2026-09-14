package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func getEnvOrDefault(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}

func main() {
	log.Println("[LOGGING-SERVICE] Starting up...")

	mongoURI := getEnvOrDefault("MONGO_URI", "mongodb://127.0.0.1:27017")
	rabbitURI := getEnvOrDefault("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
	httpPort := getEnvOrDefault("HTTP_PORT", "8090")

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

	// 4. Setup HTTP Server for Health & Readiness Probes
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
		// In a production setup, we'd also check RabbitMQ health here
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Ready"))
	})

	server := &http.Server{
		Addr:    ":" + httpPort,
		Handler: mux,
	}

	go func() {
		log.Printf("[LOGGING-SERVICE] HTTP listening on :%s for health checks", httpPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[FATAL] HTTP server failed: %v", err)
		}
	}()

	// 5. Wait for Shutdown Signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("[LOGGING-SERVICE] Shutting down...")
	cancel() // Stops the consumer loop

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ERROR] HTTP server shutdown error: %v", err)
	}

	log.Println("[LOGGING-SERVICE] Shutdown complete.")
}
