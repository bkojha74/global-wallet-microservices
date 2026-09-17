package db

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// DefaultMongoURI returns MONGO_URI from env if set.
// If unset, it checks if "mongodb" resolves (inside Docker). If not, it falls back to 127.0.0.1:27017 for native host execution.
func DefaultMongoURI() string {
	if uri := os.Getenv("MONGO_URI"); uri != "" {
		return uri
	}
	if _, err := net.LookupHost("mongodb"); err == nil {
		return "mongodb://mongodb:27017/?replicaSet=rs0&directConnection=true"
	}
	return "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
}

// ConnectWithRetry connects to MongoDB with exponential backoff to handle replica set initialization
func ConnectWithRetry(ctx context.Context, uri string, maxRetries int) (*mongo.Client, error) {
	if maxRetries <= 0 {
		return nil, fmt.Errorf("maxRetries must be greater than zero")
	}

	opts := options.Client().ApplyURI(uri)
	opts.SetServerSelectionTimeout(3 * time.Second)
	opts.SetMaxPoolSize(100)
	opts.SetMinPoolSize(10)
	opts.SetMaxConnIdleTime(5 * time.Minute)

	var client *mongo.Client
	var err error

	for i := 1; i <= maxRetries; i++ {
		log.Printf("[DB] Attempting to connect to MongoDB (%d/%d): %s", i, maxRetries, uri)
		client, err = mongo.Connect(ctx, opts)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = client.Ping(pingCtx, readpref.Primary())
			cancel()
			if err == nil {
				log.Println("[DB] Successfully connected to MongoDB Primary replica!")
				return client, nil
			}
		}

		log.Printf("[DB] Connection failed (%v). Retrying in 2s...", err)
		time.Sleep(2 * time.Second)
	}

	return nil, fmt.Errorf("failed to connect to MongoDB after %d attempts: %w", maxRetries, err)
}
