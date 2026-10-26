package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"wallet-system/pkg/db"
	walletv1 "wallet-system/proto/wallet"
)

const (
	KYCTier1 = "TIER_1" // Basic/Unverified: Single: $1,000 (100,000 units), Daily 24h: $5,000 (500,000 units), Max: 10 txns/hr
	KYCTier2 = "TIER_2" // Standard/Verified: Single: $10,000 (1,000,000 units), Daily 24h: $50,000 (5,000,000 units), Max: 50 txns/hr
	KYCTier3 = "TIER_3" // Enterprise/VIP: Single: $100,000 (10,000,000 units), Daily 24h: $500,000 (50,000,000 units), Max: 200 txns/hr
)

// KYCTierConfig holds the regulatory velocity thresholds for a given KYC tier.
type KYCTierConfig struct {
	Tier                   string
	SingleTxnLimitUnits    int64 // In minor units (cents), e.g. 100000 = $1,000.00
	DailyRollingLimitUnits int64 // In minor units (cents), e.g. 500000 = $5,000.00
	HourlyRateLimitCount   int32 // Max transactions permitted in a rolling 1-hour window
}

var TierConfigs = map[string]KYCTierConfig{
	KYCTier1: {
		Tier:                   KYCTier1,
		SingleTxnLimitUnits:    100000,
		DailyRollingLimitUnits: 500000,
		HourlyRateLimitCount:   10,
	},
	KYCTier2: {
		Tier:                   KYCTier2,
		SingleTxnLimitUnits:    1000000,
		DailyRollingLimitUnits: 5000000,
		HourlyRateLimitCount:   50,
	},
	KYCTier3: {
		Tier:                   KYCTier3,
		SingleTxnLimitUnits:    10000000,
		DailyRollingLimitUnits: 50000000,
		HourlyRateLimitCount:   200,
	},
}

// GetTierConfig returns the regulatory parameters for the requested tier, defaulting to TIER_1.
func GetTierConfig(tier string) KYCTierConfig {
	cfg, ok := TierConfigs[tier]
	if !ok {
		return TierConfigs[KYCTier1]
	}
	return cfg
}

// VelocityRecord logs individual transactions to calculate rolling compliance limits.
type VelocityRecord struct {
	ID           primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	WalletID     string             `bson:"wallet_id" json:"wallet_id"`
	DestWalletID string             `bson:"dest_wallet_id,omitempty" json:"dest_wallet_id,omitempty"`
	Amount       int64              `bson:"amount" json:"amount"`
	Currency     string             `bson:"currency" json:"currency"`
	USDUnits     int64              `bson:"usd_units" json:"usd_units"`
	TxnType      string             `bson:"txn_type" json:"txn_type"`
	CreatedAt    time.Time          `bson:"created_at" json:"created_at"`
}

// normalizeToUSDUnits converts an amount in any supported currency to USD minor units (cents)
// using standard midpoint conversion for equitable volume limit calculations.
func normalizeToUSDUnits(amount int64, currency string) int64 {
	curr := db.NormalizeCurrency(currency)
	if curr == "USD" || curr == "" {
		return amount
	}
	// Fixed institutional exchange rates to USD for uniform velocity checks
	ratesToUSD := map[string]float64{
		"EUR": 1.0870,
		"GBP": 1.2820,
		"JPY": 0.0064,
		"CHF": 1.1210,
		"CAD": 0.7350,
		"AUD": 0.6550,
		"SGD": 0.7450,
		"INR": 0.0120,
		"BHD": 2.6525,
	}
	rate, ok := ratesToUSD[curr]
	if !ok {
		return amount
	}
	return db.ConvertUnitsScaleBankers(amount, curr, "USD", rate)
}

// GetWalletVelocityUsage computes rolling 24-hour spent volume and 1-hour transaction rate.
func (s *server) GetWalletVelocityUsage(ctx context.Context, walletID string) (dailySpent int64, hourlyCount int32, err error) {
	if s.db() == nil {
		return 0, 0, nil
	}
	col := s.db().Collection("wallet_velocity_records")
	now := time.Now().UTC()
	oneDayAgo := now.Add(-24 * time.Hour)
	oneHourAgo := now.Add(-1 * time.Hour)

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"wallet_id":  walletID,
			"created_at": bson.M{"$gte": oneDayAgo},
		}}},
		{{Key: "$group", Value: bson.M{
			"_id":         nil,
			"daily_spent": bson.M{"$sum": "$usd_units"},
			"hourly_count": bson.M{
				"$sum": bson.M{
					"$cond": bson.A{
						bson.M{"$gte": bson.A{"$created_at", oneHourAgo}},
						1,
						0,
					},
				},
			},
		}}},
	}

	cursor, err := col.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, 0, err
	}
	defer cursor.Close(ctx)

	var results []struct {
		DailySpent  int64 `bson:"daily_spent"`
		HourlyCount int32 `bson:"hourly_count"`
	}
	if err := cursor.All(ctx, &results); err != nil {
		return 0, 0, err
	}
	if len(results) > 0 {
		return results[0].DailySpent, results[0].HourlyCount, nil
	}
	return 0, 0, nil
}

// CheckVelocityLimits validates an outbound transfer or currency exchange against KYC caps.
func (s *server) CheckVelocityLimits(ctx context.Context, wallet WalletModel, amount int64, currency string) error {
	tier := wallet.EffectiveKYCTier()
	cfg := GetTierConfig(tier)

	usdAmount := normalizeToUSDUnits(amount, currency)

	// 1. Single transaction cap check
	if usdAmount > cfg.SingleTxnLimitUnits {
		return fmt.Errorf("transaction amount (%d USD cents) exceeds KYC %s single transaction limit (%d USD cents)",
			usdAmount, tier, cfg.SingleTxnLimitUnits)
	}

	dailySpent, hourlyCount, err := s.GetWalletVelocityUsage(ctx, wallet.ID)
	if err != nil {
		log.Printf("[COMPLIANCE] Warning: failed to fetch velocity usage for wallet %s: %v", wallet.ID, err)
		return nil // Fail-open on non-critical analytics aggregation errors
	}

	// 2. Hourly transaction rate limit
	if hourlyCount >= cfg.HourlyRateLimitCount {
		return fmt.Errorf("wallet %s exceeded KYC %s hourly velocity limit (%d/%d transactions in past hour)",
			wallet.ID, tier, hourlyCount, cfg.HourlyRateLimitCount)
	}

	// 3. 24-hour rolling volume limit
	if dailySpent+usdAmount > cfg.DailyRollingLimitUnits {
		return fmt.Errorf("transaction would exceed KYC %s 24-hour rolling limit: spent %d + current %d > max %d USD cents",
			tier, dailySpent, usdAmount, cfg.DailyRollingLimitUnits)
	}

	return nil
}

// RecordVelocityUsage appends a completed transaction to the velocity log collection.
func (s *server) RecordVelocityUsage(sessCtx mongo.SessionContext, walletID string, amount int64, currency, txnType string) error {
	return s.RecordVelocityUsageWithDest(sessCtx, walletID, "", amount, currency, txnType)
}

// RecordVelocityUsageWithDest appends a completed transaction with destination wallet to the velocity log collection.
func (s *server) RecordVelocityUsageWithDest(sessCtx mongo.SessionContext, walletID, destWalletID string, amount int64, currency, txnType string) error {
	if s.db() == nil {
		return nil
	}
	col := s.db().Collection("wallet_velocity_records")
	rec := VelocityRecord{
		ID:           primitive.NewObjectID(),
		WalletID:     walletID,
		DestWalletID: destWalletID,
		Amount:       amount,
		Currency:     db.NormalizeCurrency(currency),
		USDUnits:     normalizeToUSDUnits(amount, currency),
		TxnType:      txnType,
		CreatedAt:    time.Now().UTC(),
	}
	_, err := col.InsertOne(sessCtx, rec)
	return err
}

// UpdateWalletKYC implements the gRPC method to modify a wallet's KYC tier.
func (s *server) UpdateWalletKYC(ctx context.Context, req *walletv1.UpdateWalletKYCRequest) (*walletv1.UpdateWalletKYCResponse, error) {
	if req == nil || req.WalletId == "" || req.KycTier == "" {
		return &walletv1.UpdateWalletKYCResponse{
			Success: false,
			Message: "wallet_id and kyc_tier are required",
		}, nil
	}

	tier := req.KycTier
	if _, ok := TierConfigs[tier]; !ok {
		return &walletv1.UpdateWalletKYCResponse{
			Success: false,
			Message: fmt.Sprintf("invalid kyc_tier: %s (supported: TIER_1, TIER_2, TIER_3)", tier),
		}, nil
	}

	if s.db() == nil {
		return &walletv1.UpdateWalletKYCResponse{
			Success: false,
			Message: "database unavailable",
		}, nil
	}

	walletsCol := s.db().Collection("wallets")
	var existing WalletModel
	if err := walletsCol.FindOne(ctx, bson.M{"_id": req.WalletId}).Decode(&existing); err != nil {
		return &walletv1.UpdateWalletKYCResponse{
			Success: false,
			Message: fmt.Sprintf("wallet %s not found", req.WalletId),
		}, nil
	}

	prevTier := existing.EffectiveKYCTier()
	update := bson.M{
		"$set": bson.M{
			"kyc_tier":   tier,
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		},
	}
	if _, err := walletsCol.UpdateOne(ctx, bson.M{"_id": req.WalletId}, update); err != nil {
		return &walletv1.UpdateWalletKYCResponse{
			Success: false,
			Message: fmt.Sprintf("failed to update KYC tier: %v", err),
		}, nil
	}

	// Invalidate balance cache if active
	if c := s.cache(); c != nil {
		c.Invalidate(req.WalletId)
	}

	log.Printf("[COMPLIANCE] Wallet %s KYC tier updated: %s -> %s", req.WalletId, prevTier, tier)
	return &walletv1.UpdateWalletKYCResponse{
		Success:      true,
		WalletId:     req.WalletId,
		PreviousTier: prevTier,
		CurrentTier:  tier,
		Message:      fmt.Sprintf("KYC tier updated successfully to %s", tier),
	}, nil
}

// GetWalletLimits returns the current limits and remaining allowances for a wallet.
func (s *server) GetWalletLimits(ctx context.Context, req *walletv1.GetWalletLimitsRequest) (*walletv1.GetWalletLimitsResponse, error) {
	if req == nil || req.WalletId == "" {
		return nil, fmt.Errorf("wallet_id is required")
	}

	if err := s.verifyWalletAccessByWalletID(ctx, req.WalletId); err != nil {
		return nil, err
	}

	if s.db() == nil {
		return nil, fmt.Errorf("database unavailable")
	}

	walletsCol := s.db().Collection("wallets")
	var wallet WalletModel
	if err := walletsCol.FindOne(ctx, bson.M{"_id": req.WalletId}).Decode(&wallet); err != nil {
		return nil, fmt.Errorf("wallet %s not found", req.WalletId)
	}

	tier := wallet.EffectiveKYCTier()
	cfg := GetTierConfig(tier)

	dailySpent, hourlySpent, err := s.GetWalletVelocityUsage(ctx, req.WalletId)
	if err != nil {
		log.Printf("[COMPLIANCE] Error getting velocity usage for %s: %v", req.WalletId, err)
	}

	dailyRemaining := cfg.DailyRollingLimitUnits - dailySpent
	if dailyRemaining < 0 {
		dailyRemaining = 0
	}
	hourlyRemaining := cfg.HourlyRateLimitCount - hourlySpent
	if hourlyRemaining < 0 {
		hourlyRemaining = 0
	}

	return &walletv1.GetWalletLimitsResponse{
		WalletId:               req.WalletId,
		KycTier:                tier,
		SingleTransactionLimit: cfg.SingleTxnLimitUnits,
		DailyRollingLimit:      cfg.DailyRollingLimitUnits,
		DailyRollingSpent:      dailySpent,
		DailyRollingRemaining:  dailyRemaining,
		HourlyRateLimit:        cfg.HourlyRateLimitCount,
		HourlyRateSpent:        hourlySpent,
		HourlyRateRemaining:    hourlyRemaining,
	}, nil
}
