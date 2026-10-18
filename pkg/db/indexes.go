package db

import (
	"context"
	"fmt"
	"log"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// EnsureLedgerIndexes creates critical performance and idempotency indexes for the ledger service.
func EnsureLedgerIndexes(ctx context.Context, db *mongo.Database) error {
	col := db.Collection("ledger_entries")

	indexes := []mongo.IndexModel{
		{
			// Absolute uniqueness for transfer idempotency at the database engine level
			Keys:    bson.D{bson.E{Key: "idempotency_key", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("idx_ledger_idempotency_unique"),
		},
		{
			// Query optimization for source wallet transaction history (newest first)
			Keys: bson.D{
				bson.E{Key: "source_wallet_id", Value: 1},
				bson.E{Key: "timestamp", Value: -1},
			},
			Options: options.Index().SetName("idx_ledger_source_wallet_time"),
		},
		{
			// Query optimization for destination wallet transaction history (newest first)
			Keys: bson.D{
				bson.E{Key: "destination_wallet_id", Value: 1},
				bson.E{Key: "timestamp", Value: -1},
			},
			Options: options.Index().SetName("idx_ledger_dest_wallet_time"),
		},
		{
			// Sequential query optimization for cryptographic hash chain audits
			Keys:    bson.D{bson.E{Key: "sequence_number", Value: 1}},
			Options: options.Index().SetUnique(true).SetSparse(true).SetName("idx_ledger_sequence_number"),
		},
	}

	names, err := col.Indexes().CreateMany(ctx, indexes)
	if err != nil {
		return fmt.Errorf("failed to create ledger_entries indexes: %w", err)
	}

	log.Printf("[DB-INDEX] Ensured ledger_entries indexes: %v", names)
	return nil
}

// EnsureWalletIndexes creates TTL and transactional outbox indexes for the wallet service.
func EnsureWalletIndexes(ctx context.Context, db *mongo.Database) error {
	// 1. Idempotency records with 30-day automatic TTL expiration
	idempCol := db.Collection("idempotency_records")
	ttlSeconds := int32(30 * 24 * 60 * 60) // 30 days
	idempIndexes := []mongo.IndexModel{
		{
			Keys:    bson.D{bson.E{Key: "created_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(ttlSeconds).SetName("idx_idempotency_ttl_30d"),
		},
	}
	idempNames, err := idempCol.Indexes().CreateMany(ctx, idempIndexes)
	if err != nil {
		return fmt.Errorf("failed to create idempotency_records indexes: %w", err)
	}
	log.Printf("[DB-INDEX] Ensured idempotency_records indexes: %v", idempNames)

	// 2. Transactional outbox collection (ledger_tasks)
	tasksCol := db.Collection("ledger_tasks")
	taskIndexes := []mongo.IndexModel{
		{
			// Polling index for pending ledger relay dispatches ordered by creation time
			Keys: bson.D{
				bson.E{Key: "status", Value: 1},
				bson.E{Key: "created_at", Value: 1},
			},
			Options: options.Index().SetName("idx_ledger_tasks_status_time"),
		},
		{
			// Unique idempotency constraint per transfer task
			Keys:    bson.D{bson.E{Key: "idempotency_key", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("idx_ledger_tasks_idempotency_unique"),
		},
	}
	taskNames, err := tasksCol.Indexes().CreateMany(ctx, taskIndexes)
	if err != nil {
		return fmt.Errorf("failed to create ledger_tasks indexes: %w", err)
	}
	log.Printf("[DB-INDEX] Ensured ledger_tasks indexes: %v", taskNames)

	// 3. Compliance velocity tracking collection (wallet_velocity_records)
	velocityCol := db.Collection("wallet_velocity_records")
	velocityTTLSeconds := int32(3 * 24 * 60 * 60) // 3 days
	velocityIndexes := []mongo.IndexModel{
		{
			Keys: bson.D{
				bson.E{Key: "wallet_id", Value: 1},
				bson.E{Key: "created_at", Value: -1},
			},
			Options: options.Index().SetName("idx_wallet_velocity_wallet_time"),
		},
		{
			Keys:    bson.D{bson.E{Key: "created_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(velocityTTLSeconds).SetName("idx_wallet_velocity_ttl_3d"),
		},
	}
	velocityNames, err := velocityCol.Indexes().CreateMany(ctx, velocityIndexes)
	if err != nil {
		return fmt.Errorf("failed to create wallet_velocity_records indexes: %w", err)
	}
	log.Printf("[DB-INDEX] Ensured wallet_velocity_records indexes: %v", velocityNames)

	return nil
}

// EnsureFXIndexes creates production indexes for the FX Engine collections:
//   - fx_rates: unique index on pair (BASE/TARGET) + updated_at index
//   - fx_quotes: TTL index on expires_at (auto-purges after 24h) + client_id index
//   - fx_conversions: unique index on idempotency_key + quote_id lookup index
func EnsureFXIndexes(ctx context.Context, database *mongo.Database) error {
	if database == nil {
		return nil
	}

	ratesCol := database.Collection("fx_rates")
	rateIndexes := []mongo.IndexModel{
		{
			Keys:    bson.D{bson.E{Key: "base_currency", Value: 1}, bson.E{Key: "target_currency", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("idx_fx_rates_pair_unique"),
		},
		{
			Keys:    bson.D{bson.E{Key: "updated_at", Value: -1}},
			Options: options.Index().SetName("idx_fx_rates_updated_at"),
		},
	}
	if _, err := ratesCol.Indexes().CreateMany(ctx, rateIndexes); err != nil {
		return fmt.Errorf("failed to create fx_rates indexes: %w", err)
	}

	quotesCol := database.Collection("fx_quotes")
	quoteIndexes := []mongo.IndexModel{
		{
			// Retain expired quotes for 24h for audit/troubleshooting before MongoDB TTL cleanup
			Keys:    bson.D{bson.E{Key: "expires_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(86400).SetName("idx_fx_quotes_ttl_24h"),
		},
		{
			Keys:    bson.D{bson.E{Key: "client_id", Value: 1}, bson.E{Key: "created_at", Value: -1}},
			Options: options.Index().SetName("idx_fx_quotes_client_time"),
		},
	}
	if _, err := quotesCol.Indexes().CreateMany(ctx, quoteIndexes); err != nil {
		return fmt.Errorf("failed to create fx_quotes indexes: %w", err)
	}

	convCol := database.Collection("fx_conversions")
	convIndexes := []mongo.IndexModel{
		{
			Keys:    bson.D{bson.E{Key: "idempotency_key", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("idx_fx_conversions_idempotency_unique"),
		},
		{
			Keys:    bson.D{bson.E{Key: "quote_id", Value: 1}},
			Options: options.Index().SetName("idx_fx_conversions_quote_id"),
		},
	}
	if _, err := convCol.Indexes().CreateMany(ctx, convIndexes); err != nil {
		return fmt.Errorf("failed to create fx_conversions indexes: %w", err)
	}

	log.Printf("[DB-INDEX] Ensured FX Engine indexes on fx_rates, fx_quotes, and fx_conversions")
	return nil
}

// EnsureWebhookIndexes creates indexes for outbound partner webhook subscriptions and delivery audit tracking:
//   - webhook_subscriptions: lookup index on is_active + events, owner index
//   - webhook_deliveries: polling index on status + next_retry_at, TTL index on created_at (30 days)
func EnsureWebhookIndexes(ctx context.Context, database *mongo.Database) error {
	if database == nil {
		return nil
	}

	subsCol := database.Collection("webhook_subscriptions")
	subIndexes := []mongo.IndexModel{
		{
			Keys:    bson.D{bson.E{Key: "is_active", Value: 1}, bson.E{Key: "events", Value: 1}},
			Options: options.Index().SetName("idx_webhook_subs_active_events"),
		},
		{
			Keys:    bson.D{bson.E{Key: "owner_id", Value: 1}},
			Options: options.Index().SetName("idx_webhook_subs_owner"),
		},
	}
	if _, err := subsCol.Indexes().CreateMany(ctx, subIndexes); err != nil {
		return fmt.Errorf("failed to create webhook_subscriptions indexes: %w", err)
	}

	delCol := database.Collection("webhook_deliveries")
	delTTLSeconds := int32(30 * 24 * 60 * 60) // 30 days
	delIndexes := []mongo.IndexModel{
		{
			Keys: bson.D{
				bson.E{Key: "status", Value: 1},
				bson.E{Key: "next_retry_at", Value: 1},
			},
			Options: options.Index().SetName("idx_webhook_del_status_retry"),
		},
		{
			Keys:    bson.D{bson.E{Key: "created_at", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(delTTLSeconds).SetName("idx_webhook_del_ttl_30d"),
		},
	}
	if _, err := delCol.Indexes().CreateMany(ctx, delIndexes); err != nil {
		return fmt.Errorf("failed to create webhook_deliveries indexes: %w", err)
	}

	log.Printf("[DB-INDEX] Ensured webhook indexes on webhook_subscriptions and webhook_deliveries")
	return nil
}
