package db

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// ConnectWithRetry connects to MongoDB with exponential backoff to handle replica set initialization
func ConnectWithRetry(ctx context.Context, uri string, maxRetries int) (*mongo.Client, error) {
	if maxRetries <= 0 {
		return nil, fmt.Errorf("maxRetries must be greater than zero")
	}

	opts := options.Client().ApplyURI(uri)
	opts.SetServerSelectionTimeout(3 * time.Second)

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
