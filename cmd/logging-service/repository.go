package main

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"wallet-system/pkg/observability"
)

// QueryFilter holds optional filter criteria for log queries.
// All fields are optional; zero values are ignored when building the MongoDB filter.
type QueryFilter struct {
	TransactionID string
	AssociationID string
	Service       string
	Level         string
	From          time.Time
	To            time.Time
	Limit         int64
	Offset        int64
}

// LogRepository defines the interface for persisting and querying log events.
type LogRepository interface {
	Save(ctx context.Context, event observability.Event) error
	Find(ctx context.Context, filter QueryFilter) ([]observability.Event, error)
	// Retention deletes events older than maxAgeDays and returns the deleted count.
	Retention(ctx context.Context, maxAgeDays int) (int64, error)
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

// Find queries the events collection using any combination of filter fields.
// Results are sorted by occurred_at ascending (natural trace order).
func (r *MongoLogRepository) Find(ctx context.Context, filter QueryFilter) ([]observability.Event, error) {
	mongoFilter := bson.D{}

	if filter.TransactionID != "" {
		mongoFilter = append(mongoFilter, bson.E{Key: "transaction_id", Value: filter.TransactionID})
	}
	if filter.AssociationID != "" {
		mongoFilter = append(mongoFilter, bson.E{Key: "association_id", Value: filter.AssociationID})
	}
	if filter.Service != "" {
		mongoFilter = append(mongoFilter, bson.E{Key: "service", Value: filter.Service})
	}
	if filter.Level != "" {
		mongoFilter = append(mongoFilter, bson.E{Key: "level", Value: filter.Level})
	}

	if !filter.From.IsZero() || !filter.To.IsZero() {
		timeFilter := bson.D{}
		if !filter.From.IsZero() {
			timeFilter = append(timeFilter, bson.E{Key: "$gte", Value: filter.From})
		}
		if !filter.To.IsZero() {
			timeFilter = append(timeFilter, bson.E{Key: "$lte", Value: filter.To})
		}
		mongoFilter = append(mongoFilter, bson.E{Key: "occurred_at", Value: timeFilter})
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	findOptions := options.Find().
		SetSort(bson.D{{Key: "occurred_at", Value: 1}}).
		SetLimit(limit).
		SetSkip(filter.Offset)

	cursor, err := r.collection.Find(ctx, mongoFilter, findOptions)
	if err != nil {
		return nil, fmt.Errorf("find query failed: %w", err)
	}
	defer cursor.Close(ctx)

	var events []observability.Event
	if err := cursor.All(ctx, &events); err != nil {
		return nil, fmt.Errorf("cursor decode failed: %w", err)
	}
	return events, nil
}

func (r *MongoLogRepository) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return r.client.Ping(ctx, nil)
}

// Retention deletes log events whose occurred_at is older than maxAgeDays.
// It leverages the { occurred_at: 1 } index for an efficient range delete.
// AUDIT-level events are never deleted to preserve the mandatory audit trail.
// Returns the number of documents deleted.
func (r *MongoLogRepository) Retention(ctx context.Context, maxAgeDays int) (int64, error) {
	if maxAgeDays <= 0 {
		maxAgeDays = 90
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -maxAgeDays)
	// Preserve AUDIT events regardless of age.
	filter := bson.D{
		{Key: "occurred_at", Value: bson.D{{Key: "$lt", Value: cutoff}}},
		{Key: "level", Value: bson.D{{Key: "$ne", Value: "AUDIT"}}},
	}
	result, err := r.collection.DeleteMany(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("retention delete failed: %w", err)
	}
	return result.DeletedCount, nil
}

func (r *MongoLogRepository) Close(ctx context.Context) error {
	return r.client.Disconnect(ctx)
}
