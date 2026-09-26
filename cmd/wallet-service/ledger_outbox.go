package main

import (
	"context"
	"fmt"
	"log"
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

// LedgerRelay polls pending ledger tasks and guarantees reliable asynchronous delivery
// to the Ledger Service.
type LedgerRelay struct {
	col          *mongo.Collection
	store        LedgerTaskStore
	ledgerClient ledgerv1.LedgerServiceClient
	pollInterval time.Duration
	batchSize    int64
	logger       observability.Logger
	cancel       context.CancelFunc
}

func NewLedgerRelay(db *mongo.Database, client ledgerv1.LedgerServiceClient, pollInterval time.Duration, batchSize int64, logger observability.Logger) *LedgerRelay {
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
	return &LedgerRelay{
		col:          col,
		store:        &mongoLedgerTaskStore{col: col},
		ledgerClient: client,
		pollInterval: pollInterval,
		batchSize:    batchSize,
		logger:       logger,
	}
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

	go func() {
		log.Printf("[LEDGER-RELAY] Background outbox relay started (interval: %v, batch: %d)", r.pollInterval, r.batchSize)
		ticker := time.NewTicker(r.pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if count, err := r.ProcessBatch(relayCtx); err != nil {
					log.Printf("[LEDGER-RELAY] Error processing batch: %v", err)
				} else if count > 0 {
					log.Printf("[LEDGER-RELAY] Relayed %d pending ledger entries successfully", count)
				}
			case <-relayCtx.Done():
				log.Println("[LEDGER-RELAY] Outbox relay shutting down...")
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
