package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	TargetPrimary = "PRIMARY"
	TargetStandby = "STANDBY"
)

var (
	ErrInvalidTarget = errors.New("invalid target: must be PRIMARY or STANDBY")
)

// FailoverCoordinator coordinates the active cluster routing target across distributed services.
type FailoverCoordinator interface {
	GetActiveTarget(ctx context.Context) (string, error)
	SetActiveTarget(ctx context.Context, target string) error
	Close() error
}

// MemoryFailoverCoordinator provides an in-memory thread-safe coordinator for testing or standalone mode.
type MemoryFailoverCoordinator struct {
	mu     sync.RWMutex
	target string
}

// NewMemoryFailoverCoordinator creates a new in-memory coordinator defaulted to PRIMARY.
func NewMemoryFailoverCoordinator(initialTarget string) *MemoryFailoverCoordinator {
	if initialTarget == "" {
		initialTarget = TargetPrimary
	}
	return &MemoryFailoverCoordinator{
		target: initialTarget,
	}
}

func (m *MemoryFailoverCoordinator) GetActiveTarget(ctx context.Context) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.target, nil
}

func (m *MemoryFailoverCoordinator) SetActiveTarget(ctx context.Context, target string) error {
	if target != TargetPrimary && target != TargetStandby {
		return fmt.Errorf("%w: %s", ErrInvalidTarget, target)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.target = target
	return nil
}

func (m *MemoryFailoverCoordinator) Close() error {
	return nil
}

type clusterRoutingDoc struct {
	ID        string    `bson:"_id"`
	Target    string    `bson:"target"`
	UpdatedAt time.Time `bson:"updated_at"`
}

// MongoFailoverCoordinator provides a distributed coordinator backed by a MongoDB replica set.
type MongoFailoverCoordinator struct {
	collection *mongo.Collection
	cacheTTL   time.Duration
	mu         sync.RWMutex
	cachedVal  string
	cacheUntil time.Time
}

// NewMongoFailoverCoordinator creates a coordinator backed by MongoDB collection cluster_state.
func NewMongoFailoverCoordinator(db *mongo.Database, cacheTTL time.Duration) *MongoFailoverCoordinator {
	if cacheTTL <= 0 {
		cacheTTL = 1 * time.Second
	}
	return &MongoFailoverCoordinator{
		collection: db.Collection("cluster_state"),
		cacheTTL:   cacheTTL,
	}
}

func (m *MongoFailoverCoordinator) GetActiveTarget(ctx context.Context) (string, error) {
	m.mu.RLock()
	if time.Now().Before(m.cacheUntil) && m.cachedVal != "" {
		cached := m.cachedVal
		m.mu.RUnlock()
		return cached, nil
	}
	m.mu.RUnlock()

	var doc clusterRoutingDoc
	err := m.collection.FindOne(ctx, bson.M{"_id": "cluster_routing"}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			// Initialize default record
			now := time.Now().UTC()
			initDoc := clusterRoutingDoc{
				ID:        "cluster_routing",
				Target:    TargetPrimary,
				UpdatedAt: now,
			}
			opts := options.Update().SetUpsert(true)
			_, _ = m.collection.UpdateOne(ctx, bson.M{"_id": "cluster_routing"}, bson.M{"$setOnInsert": initDoc}, opts)
			m.updateCache(TargetPrimary)
			return TargetPrimary, nil
		}
		return "", fmt.Errorf("failed to fetch cluster routing target: %w", err)
	}

	m.updateCache(doc.Target)
	return doc.Target, nil
}

func (m *MongoFailoverCoordinator) SetActiveTarget(ctx context.Context, target string) error {
	if target != TargetPrimary && target != TargetStandby {
		return fmt.Errorf("%w: %s", ErrInvalidTarget, target)
	}

	filter := bson.M{"_id": "cluster_routing"}
	update := bson.M{
		"$set": bson.M{
			"target":     target,
			"updated_at": time.Now().UTC(),
		},
	}
	opts := options.Update().SetUpsert(true)

	if _, err := m.collection.UpdateOne(ctx, filter, update, opts); err != nil {
		return fmt.Errorf("failed to update cluster routing state in MongoDB: %w", err)
	}

	m.updateCache(target)
	return nil
}

func (m *MongoFailoverCoordinator) updateCache(target string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cachedVal = target
	m.cacheUntil = time.Now().Add(m.cacheTTL)
}

func (m *MongoFailoverCoordinator) Close() error {
	return nil
}
