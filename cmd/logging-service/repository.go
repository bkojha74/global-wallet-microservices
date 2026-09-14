package main

import (
	"context"
	"fmt"
	"time"

	"wallet-system/pkg/observability"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// LogRepository defines the interface for persisting and querying log events.
type LogRepository interface {
	Save(ctx context.Context, event observability.Event) error
	Health(ctx context.Context) error
}

type MongoLogRepository struct {
	client     *mongo.Client
	collection *mongo.Collection
}

// NewMongoLogRepository creates a new MongoDB-backed LogRepository.
func NewMongoLogRepository(ctx context.Context, uri, dbName, collectionName string) (*MongoLogRepository, error) {
	clientOptions := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to mongodb: %w", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping mongodb: %w", err)
	}

	collection := client.Database(dbName).Collection(collectionName)
	repo := &MongoLogRepository{
		client:     client,
		collection: collection,
	}

	if err := repo.createIndexes(ctx); err != nil {
		return nil, fmt.Errorf("failed to create indexes: %w", err)
	}

	return repo, nil
}

func (r *MongoLogRepository) createIndexes(ctx context.Context) error {
	indexes := []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "event_id", Value: 1}},
			Options: options.Index().SetUnique(true), // Deduplication
		},
		{
			Keys:    bson.D{{Key: "transaction_id", Value: 1}},
			Options: options.Index().SetSparse(true),
		},
		{
			Keys: bson.D{{Key: "association_id", Value: 1}},
		},
		{
			Keys: bson.D{{Key: "occurred_at", Value: 1}},
		},
		{
			Keys: bson.D{
				{Key: "service", Value: 1},
				{Key: "level", Value: 1},
				{Key: "occurred_at", Value: -1},
			},
		},
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := r.collection.Indexes().CreateMany(ctx, indexes)
	return err
}

func (r *MongoLogRepository) Save(ctx context.Context, event observability.Event) error {
	_, err := r.collection.InsertOne(ctx, event)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			// Idempotency: Ignore duplicate event IDs
			return nil
		}
		return err
	}
	return nil
}

func (r *MongoLogRepository) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return r.client.Ping(ctx, nil)
}

func (r *MongoLogRepository) Close(ctx context.Context) error {
	return r.client.Disconnect(ctx)
}
