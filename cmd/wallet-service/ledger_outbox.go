package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"wallet-system/pkg/observability"
	ledgerv1 "wallet-system/proto/ledger"
)

const (
	LedgerTaskStatusPending   = "pending"
	LedgerTaskStatusCompleted = "completed"
	LedgerTaskStatusFailed    = "failed"
)

// LedgerTask represents an immutable ledger dispatch record stored atomically inside
// the wallet debit/credit MongoDB transaction.
type LedgerTask struct {
	ID                  primitive.ObjectID `bson:"_id"`
	TransactionID       string             `bson:"transaction_id"`
	IdempotencyKey      string             `bson:"idempotency_key"`
	SourceWalletID      string             `bson:"source_wallet_id"`
	DestinationWalletID string             `bson:"destination_wallet_id"`
	Amount              int64              `bson:"amount"`
	Currency            string             `bson:"currency"`
	DestinationAmount   int64              `bson:"destination_amount,omitempty"`
	DestinationCurrency string             `bson:"destination_currency,omitempty"`
	ExchangeRate        float64            `bson:"exchange_rate,omitempty"`
	FXQuoteID           string             `bson:"fx_quote_id,omitempty"`
	Region              string             `bson:"region"`
	Status              string             `bson:"status"` // pending | completed | failed
	CreatedAt           time.Time          `bson:"created_at"`
	ProcessedAt         *time.Time         `bson:"processed_at,omitempty"`
	Error               string             `bson:"error,omitempty"`
	Retries             int                `bson:"retries"`
}

// AppendLedgerTask writes a pending ledger dispatch task inside an active MongoDB session context.
func AppendLedgerTask(sessCtx mongo.SessionContext, col *mongo.Collection, task LedgerTask) error {
	_, err := col.InsertOne(sessCtx, task)
	return err
}

type LedgerTaskStore interface {
	FindPending(ctx context.Context, limit int64) ([]LedgerTask, error)
	MarkCompleted(ctx context.Context, id primitive.ObjectID, processedAt time.Time) error
	MarkFailed(ctx context.Context, id primitive.ObjectID, errMsg string) error
}

type mongoLedgerTaskStore struct {
	col *mongo.Collection
}

func (m *mongoLedgerTaskStore) FindPending(ctx context.Context, limit int64) ([]LedgerTask, error) {
	if m.col == nil {
		return nil, nil
	}
	opts := options.Find().
		SetSort(bson.D{bson.E{Key: "created_at", Value: 1}}).
		SetLimit(limit)
	cursor, err := m.col.Find(ctx, bson.M{"status": LedgerTaskStatusPending}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var tasks []LedgerTask
	if err := cursor.All(ctx, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (m *mongoLedgerTaskStore) MarkCompleted(ctx context.Context, id primitive.ObjectID, processedAt time.Time) error {
	if m.col == nil {
		return nil
	}
	_, err := m.col.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{
			"$set": bson.M{
				"status":       LedgerTaskStatusCompleted,
				"processed_at": processedAt,
				"error":        "",
			},
		},
	)
	return err
}

func (m *mongoLedgerTaskStore) MarkFailed(ctx context.Context, id primitive.ObjectID, errMsg string) error {
	if m.col == nil {
		return nil
	}
	_, err := m.col.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{
			"$inc": bson.M{"retries": 1},
			"$set": bson.M{"error": errMsg},
		},
	)
	return err
}

// LeaseManager defines the interface for distributed leader lease acquisition.
type LeaseManager interface {
	TryAcquireOrRenew(ctx context.Context) (bool, error)
	Release(ctx context.Context) error
	HolderID() string
}

const (
	// DefaultRelayLeaseDocID is the singleton document ID in MongoDB for outbox relay leadership.
	DefaultRelayLeaseDocID = "ledger_outbox_relay"
	// DefaultLeaseTTL is the default validity duration for a leadership lease.
	DefaultLeaseTTL = 10 * time.Second
)

// RelayLeaseDoc represents the MongoDB document used for distributed leader leasing.
type RelayLeaseDoc struct {
	ID         string    `bson:"_id"`
	HolderID   string    `bson:"holder_id"`
	AcquiredAt time.Time `bson:"acquired_at"`
	ExpiresAt  time.Time `bson:"expires_at"`
}

// MongoRelayLeaseManager implements LeaseManager using atomic MongoDB FindOneAndUpdate operations.
type MongoRelayLeaseManager struct {
	col      *mongo.Collection
	holderID string
	ttl      time.Duration
	leaseID  string
	initOnce sync.Once
}

// NewMongoRelayLeaseManager creates a lease manager backed by MongoDB collection "ledger_relay_lease".
func NewMongoRelayLeaseManager(db *mongo.Database, holderID string, ttl time.Duration) *MongoRelayLeaseManager {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	if holderID == "" {
		holderID = observability.ResolveInstanceID()
	}
	var col *mongo.Collection
	if db != nil {
		col = db.Collection("ledger_relay_lease")
	}
	return &MongoRelayLeaseManager{
		col:      col,
		holderID: holderID,
		ttl:      ttl,
		leaseID:  DefaultRelayLeaseDocID,
	}
}

// HolderID returns the instance identifier of this lease candidate.
func (m *MongoRelayLeaseManager) HolderID() string {
	return m.holderID
}

// ensureDocExists guarantees that the singleton lease document exists before running FindOneAndUpdate.
func (m *MongoRelayLeaseManager) ensureDocExists(ctx context.Context) error {
	if m.col == nil {
		return nil
	}
	_, err := m.col.UpdateOne(ctx,
		bson.M{"_id": m.leaseID},
		bson.M{
			"$setOnInsert": bson.M{
				"holder_id":   "",
				"acquired_at": time.Unix(0, 0).UTC(),
				"expires_at":  time.Unix(0, 0).UTC(),
			},
		},
		options.Update().SetUpsert(true),
	)
	return err
}

// TryAcquireOrRenew attempts to acquire leadership if unheld/expired, or renew it if currently held.
func (m *MongoRelayLeaseManager) TryAcquireOrRenew(ctx context.Context) (bool, error) {
	if m.col == nil {
		return true, nil
	}

	m.initOnce.Do(func() {
		initCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_ = m.ensureDocExists(initCtx)
	})

	now := time.Now().UTC()
	expiresAt := now.Add(m.ttl)

	filter := bson.M{
		"_id": m.leaseID,
		"$or": []bson.M{
			{"holder_id": m.holderID},
			{"expires_at": bson.M{"$lt": now}},
		},
	}
	update := bson.M{
		"$set": bson.M{
			"holder_id":   m.holderID,
			"acquired_at": now,
			"expires_at":  expiresAt,
		},
	}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)

	var result RelayLeaseDoc
	err := m.col.FindOneAndUpdate(ctx, filter, update, opts).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			// Another active pod holds a valid unexpired lease
			return false, nil
		}
		return false, fmt.Errorf("lease election query failed: %w", err)
	}

	return result.HolderID == m.holderID, nil
}

// Release voluntarily relinquishes leadership so standby pods can acquire the lease immediately.
func (m *MongoRelayLeaseManager) Release(ctx context.Context) error {
	if m.col == nil {
		return nil
	}
	_, err := m.col.UpdateOne(ctx,
		bson.M{
			"_id":       m.leaseID,
			"holder_id": m.holderID,
		},
		bson.M{
			"$set": bson.M{
				"holder_id":  "",
				"expires_at": time.Unix(0, 0).UTC(),
			},
		},
	)
	return err
}

// RelayOption defines functional options for LedgerRelay.
type RelayOption func(*LedgerRelay)

// WithLeaseManager configures a distributed leadership lease manager for the relay worker.
func WithLeaseManager(lm LeaseManager) RelayOption {
	return func(r *LedgerRelay) {
		r.leaseManager = lm
	}
}

// LedgerRelay polls pending ledger tasks and guarantees reliable asynchronous delivery
// to the Ledger Service. When running with a LeaseManager, only the elected leader pod processes outbox tasks.
type LedgerRelay struct {
	col          *mongo.Collection
	store        LedgerTaskStore
	ledgerClient ledgerv1.LedgerServiceClient
	pollInterval time.Duration
	batchSize    int64
	logger       observability.Logger
	cancel       context.CancelFunc
	leaseManager LeaseManager
	isLeader     bool
}

func NewLedgerRelay(db *mongo.Database, client ledgerv1.LedgerServiceClient, pollInterval time.Duration, batchSize int64, logger observability.Logger, opts ...RelayOption) *LedgerRelay {
	if pollInterval <= 0 {
		pollInterval = 1 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	var col *mongo.Collection
	if db != nil {
		col = db.Collection("ledger_tasks")
	}
	r := &LedgerRelay{
		col:          col,
		store:        &mongoLedgerTaskStore{col: col},
		ledgerClient: client,
		pollInterval: pollInterval,
		batchSize:    batchSize,
		logger:       logger,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

// IsLeader reports whether this relay instance currently holds the active leadership lease.
func (r *LedgerRelay) IsLeader() bool {
	return r.isLeader || r.leaseManager == nil
}

// DispatchImmediate attempts a synchronous RPC to ledger-service immediately after transaction commit.
// If it succeeds, the task is marked completed immediately, avoiding relay polling latency.
func (r *LedgerRelay) DispatchImmediate(ctx context.Context, task LedgerTask) error {
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	resp, err := r.ledgerClient.RecordTransaction(callCtx, &ledgerv1.RecordTransactionRequest{
		TransactionId:       task.TransactionID,
		IdempotencyKey:      task.IdempotencyKey,
		SourceWalletId:      task.SourceWalletID,
		DestinationWalletId: task.DestinationWalletID,
		Amount:              task.Amount,
		Currency:            task.Currency,
		Region:              task.Region,
		DestinationAmount:   task.DestinationAmount,
		DestinationCurrency: task.DestinationCurrency,
		ExchangeRate:        task.ExchangeRate,
		FxQuoteId:           task.FXQuoteID,
	})
	if err != nil || resp == nil || !resp.Success {
		errMsg := "unknown error"
		if err != nil {
			errMsg = err.Error()
		} else if resp != nil {
			errMsg = resp.ErrorMessage
		}
		return fmt.Errorf("ledger sync dispatch failed: %s", errMsg)
	}

	now := time.Now().UTC()
	if r.store != nil {
		return r.store.MarkCompleted(ctx, task.ID, now)
	}
	return nil
}

// Start begins the background relay loop until context cancellation.
func (r *LedgerRelay) Start(ctx context.Context) {
	relayCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	// #nosec G118 -- intentional background worker goroutine for relay loop; shutdown uses detached context for lease release
	go func() {
		log.Printf("[LEDGER-RELAY] Background outbox relay started (interval: %v, batch: %d)", r.pollInterval, r.batchSize)
		ticker := time.NewTicker(r.pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if r.leaseManager != nil {
					hasLease, err := r.leaseManager.TryAcquireOrRenew(relayCtx)
					if err != nil {
						log.Printf("[LEDGER-RELAY] Warning: lease election check error: %v", err)
						continue
					}
					if !hasLease {
						if r.isLeader {
							log.Printf("[LEDGER-RELAY] Lost leadership lease (stepped down to standby)")
							r.isLeader = false
						}
						continue
					}
					if !r.isLeader {
						log.Printf("[LEDGER-RELAY] Acquired leadership lease as %s (active worker)", r.leaseManager.HolderID())
						r.isLeader = true
					}
				}

				if count, err := r.ProcessBatch(relayCtx); err != nil {
					log.Printf("[LEDGER-RELAY] Error processing batch: %v", err)
				} else if count > 0 {
					log.Printf("[LEDGER-RELAY] Relayed %d pending ledger entries successfully", count)
				}
			case <-relayCtx.Done():
				log.Println("[LEDGER-RELAY] Outbox relay shutting down...")
				if r.leaseManager != nil && r.isLeader {
					// #nosec G118 -- shutdown lease release requires detached context since relayCtx is already canceled
					shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
					_ = r.leaseManager.Release(shutdownCtx)
					shutdownCancel()
				}
				return
			}
		}
	}()
}

// Stop signals the background relay loop to terminate.
func (r *LedgerRelay) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
}

// ProcessBatch finds and dispatches up to batchSize pending ledger tasks.
func (r *LedgerRelay) ProcessBatch(ctx context.Context) (int, error) {
	if r.store == nil {
		return 0, nil
	}

	tasks, err := r.store.FindPending(ctx, r.batchSize)
	if err != nil {
		return 0, fmt.Errorf("failed to query pending ledger tasks: %w", err)
	}

	processedCount := 0
	for _, task := range tasks {
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := r.ledgerClient.RecordTransaction(callCtx, &ledgerv1.RecordTransactionRequest{
			TransactionId:       task.TransactionID,
			IdempotencyKey:      task.IdempotencyKey,
			SourceWalletId:      task.SourceWalletID,
			DestinationWalletId: task.DestinationWalletID,
			Amount:              task.Amount,
			Currency:            task.Currency,
			Region:              task.Region,
			DestinationAmount:   task.DestinationAmount,
			DestinationCurrency: task.DestinationCurrency,
			ExchangeRate:        task.ExchangeRate,
			FxQuoteId:           task.FXQuoteID,
		})
		cancel()

		now := time.Now().UTC()
		if err != nil || resp == nil || !resp.Success {
			errMsg := "unknown failure"
			if err != nil {
				errMsg = err.Error()
			} else if resp != nil {
				errMsg = resp.ErrorMessage
			}
			log.Printf("[LEDGER-RELAY] Failed to dispatch task %s (idemp: %s): %s", task.ID.Hex(), task.IdempotencyKey, errMsg)

			_ = r.store.MarkFailed(ctx, task.ID, errMsg)
			continue
		}

		updateErr := r.store.MarkCompleted(ctx, task.ID, now)
		if updateErr == nil {
			processedCount++
		}
	}

	return processedCount, nil
}
