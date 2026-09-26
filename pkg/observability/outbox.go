// Package observability — Transactional Outbox (Phase 5, GAP-10)
//
// OutboxEntry records an audit event in a MongoDB collection that is written
// inside the same transaction as the business state change (wallet debit/credit
// or ledger insert).  A background OutboxRelay polls pending entries and
// publishes them to RabbitMQ, then marks them published.
//
// Usage:
//
//	// In the Mongo session callback:
//	outbox := observability.NewMongoOutbox(db, "wallet_outbox")
//	_ = outbox.Append(sessCtx, event)
//
//	// At service startup:
//	relay := observability.NewOutboxRelay(db, "wallet_outbox", publisher, logger)
//	relay.Start(ctx)
//
// Environment variables:
//
//	LOGGING_OUTBOX_ENABLED  - set to "true" to activate the outbox relay.
//	LOGGING_OUTBOX_POLL_MS  - relay poll interval in milliseconds (default 2000).
//	LOGGING_OUTBOX_BATCH    - max events per relay poll cycle (default 100).

package observability

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// OutboxStatus values for the outbox entry state machine.
const (
	OutboxStatusPending   = "pending"
	OutboxStatusPublished = "published"
	OutboxStatusFailed    = "failed"
)

// OutboxEntry is the document stored in the MongoDB outbox collection.
// It mirrors the Event envelope and adds relay lifecycle fields.
type OutboxEntry struct {
	ID          primitive.ObjectID `bson:"_id,omitempty"`
	Status      string             `bson:"status"`     // pending | published | failed
	Attempts    int                `bson:"attempts"`   // relay publish attempt count
	CreatedAt   time.Time          `bson:"created_at"` // set at insertion time
	PublishedAt *time.Time         `bson:"published_at,omitempty"`
	Event       Event              `bson:"event"` // the full event envelope
}

// MongoOutbox writes audit events to a MongoDB outbox collection inside the
// caller's session context (i.e. inside the same MongoDB transaction as the
// business state change).
type MongoOutbox struct {
	col *mongo.Collection
}

// NewMongoOutbox creates a MongoOutbox that writes to db.collectionName.
func NewMongoOutbox(db *mongo.Database, collectionName string) *MongoOutbox {
	return &MongoOutbox{col: db.Collection(collectionName)}
}

// Append inserts an OutboxEntry with status=pending into the outbox collection.
// Call this inside a mongo.WithSession / sessCtx to make it part of the
// enclosing business transaction.
func (o *MongoOutbox) Append(ctx context.Context, event Event) error {
	entry := OutboxEntry{
		ID:        primitive.NewObjectID(),
		Status:    OutboxStatusPending,
		Attempts:  0,
		CreatedAt: time.Now().UTC(),
		Event:     event,
	}
	_, err := o.col.InsertOne(ctx, entry)
	return err
}

// EnsureIndexes creates the indexes needed for efficient outbox relay queries.
// Call once at service startup (outside a transaction).
func (o *MongoOutbox) EnsureIndexes(ctx context.Context) error {
	indexes := []mongo.IndexModel{
		{
			// Primary relay query: fetch pending entries in creation order.
			Keys: bson.D{
				bson.E{Key: "status", Value: 1},
				bson.E{Key: "created_at", Value: 1},
			},
		},
		{
			// Deduplication: if event_id was already published, skip.
			Keys:    bson.D{bson.E{Key: "event.event_id", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
	}
	_, err := o.col.Indexes().CreateMany(ctx, indexes)
	return err
}

// ─── OutboxRelay ─────────────────────────────────────────────────────────────

// OutboxRelay polls the outbox collection for pending entries and publishes
// them to RabbitMQ via the provided EventPublisher, then marks them published.
// Failed entries are retried up to MaxAttempts times before being marked failed.
type OutboxRelay struct {
	col          *mongo.Collection
	publisher    EventPublisher
	pollInterval time.Duration
	batchSize    int64
	maxAttempts  int
}

// NewOutboxRelay creates an OutboxRelay for the given MongoDB collection.
//
//   - db:             the *mongo.Database that owns the outbox collection.
//   - collectionName: e.g. "wallet_outbox" or "ledger_outbox".
//   - publisher:      an EventPublisher (e.g. *RabbitPublisher).
func NewOutboxRelay(db *mongo.Database, collectionName string, publisher EventPublisher) *OutboxRelay {
	pollMS, _ := strconv.Atoi(os.Getenv("LOGGING_OUTBOX_POLL_MS"))
	if pollMS <= 0 {
		pollMS = 2000
	}
	batch, _ := strconv.ParseInt(os.Getenv("LOGGING_OUTBOX_BATCH"), 10, 64)
	if batch <= 0 {
		batch = 100
	}
	return &OutboxRelay{
		col:          db.Collection(collectionName),
		publisher:    publisher,
		pollInterval: time.Duration(pollMS) * time.Millisecond,
		batchSize:    batch,
		maxAttempts:  5,
	}
}

// Start launches the relay goroutine. It runs until ctx is cancelled.
// Call this once at service startup when LOGGING_OUTBOX_ENABLED=true.
func (r *OutboxRelay) Start(ctx context.Context) {
	ticker := time.NewTicker(r.pollInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := r.relay(ctx); err != nil {
					log.Printf("[OUTBOX-RELAY] relay cycle error: %v", err)
				}
			case <-ctx.Done():
				log.Println("[OUTBOX-RELAY] stopping.")
				return
			}
		}
	}()
}

// relay fetches a batch of pending entries and publishes them.
func (r *OutboxRelay) relay(ctx context.Context) error {
	filter := bson.D{
		bson.E{Key: "status", Value: OutboxStatusPending},
		bson.E{Key: "attempts", Value: bson.D{bson.E{Key: "$lt", Value: r.maxAttempts}}},
	}
	sort := bson.D{bson.E{Key: "created_at", Value: 1}}
	findOpts := options.Find().SetSort(sort).SetLimit(r.batchSize)

	cursor, err := r.col.Find(ctx, filter, findOpts)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var entries []OutboxEntry
	if err := cursor.All(ctx, &entries); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	published := 0
	for _, entry := range entries {
		pubErr := r.publisher.Publish(ctx, entry.Event)
		now := time.Now().UTC()
		if pubErr != nil {
			// Increment attempt counter; mark failed if maxAttempts reached.
			attempts := entry.Attempts + 1
			newStatus := OutboxStatusPending
			if attempts >= r.maxAttempts {
				newStatus = OutboxStatusFailed
				log.Printf("[OUTBOX-RELAY] event %s failed after %d attempts: %v", entry.Event.EventID, attempts, pubErr)
			}
			_, _ = r.col.UpdateOne(ctx,
				bson.D{bson.E{Key: "_id", Value: entry.ID}},
				bson.D{bson.E{Key: "$set", Value: bson.D{
					bson.E{Key: "status", Value: newStatus},
					bson.E{Key: "attempts", Value: attempts},
				}}},
			)
			continue
		}
		// Mark published.
		_, _ = r.col.UpdateOne(ctx,
			bson.D{bson.E{Key: "_id", Value: entry.ID}},
			bson.D{bson.E{Key: "$set", Value: bson.D{
				bson.E{Key: "status", Value: OutboxStatusPublished},
				bson.E{Key: "attempts", Value: entry.Attempts + 1},
				bson.E{Key: "published_at", Value: now},
			}}},
		)
		published++
	}
	if published > 0 {
		log.Printf("[OUTBOX-RELAY] published %d/%d events from outbox", published, len(entries))
	}
	return nil
}

// OutboxEnabled returns true when LOGGING_OUTBOX_ENABLED=true.
// Services use this to conditionally activate the outbox path at startup.
func OutboxEnabled() bool {
	v := os.Getenv("LOGGING_OUTBOX_ENABLED")
	return v == "true" || v == "1"
}
