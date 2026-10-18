package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"wallet-system/pkg/ai"
	"wallet-system/pkg/coordinator"
	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	"wallet-system/pkg/resilience"
	"wallet-system/pkg/tlsutil"
	fxv1 "wallet-system/proto/fx"
	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

type server struct {
	walletv1.UnimplementedWalletServiceServer
	mongoClient  *mongo.Client
	ledgerClient ledgerv1.LedgerServiceClient
	fxClient     fxv1.FXServiceClient
	region       string
	isActive     bool
	targetRole   string // "PRIMARY" or "STANDBY"
	coordinator  coordinator.FailoverCoordinator
	logger       observability.Logger
	dbName       string
	// Phase 5 (GAP-10): outbox writes AUDIT events inside the MongoDB transaction.
	outbox *observability.MongoOutbox // nil when outbox is disabled
	// Phase 1 (GAP-FIN-01): transactional outbox relay for decoupled ledger entries.
	ledgerRelay *LedgerRelay
	// Phase AI-01: AI-powered fraud detection — nil when GEMINI_API_KEY is not set.
	aiClient      *ai.Client
	fraudDetector *ai.FraudDetector
	// In-memory read-through balance cache with immediate write invalidation
	balanceCache *BalanceCache
}

func (s *server) cache() *BalanceCache {
	return s.balanceCache
}

func (s *server) db() *mongo.Database {
	if s.mongoClient == nil {
		return nil
	}
	if s.dbName != "" {
		return s.mongoClient.Database(s.dbName)
	}
	return s.mongoClient.Database("banking_db")
}

func (s *server) isWritable(ctx context.Context) bool {
	if s.coordinator != nil {
		if target, err := s.coordinator.GetActiveTarget(ctx); err == nil && target != "" {
			if s.targetRole != "" {
				return target == s.targetRole
			}
		}
	}
	return s.isActive
}

func (s *server) emit(ctx context.Context, eventType, level, message string, attributes map[string]any) {
	s.emitFull(ctx, eventType, level, message, 0, nil, attributes)
}

func (s *server) emitTerminal(ctx context.Context, eventType, level, message string, durationMS int64, success bool, attributes map[string]any) {
	s.emitFull(ctx, eventType, level, message, durationMS, &success, attributes)
}

func (s *server) emitFull(ctx context.Context, eventType, level, message string, durationMS int64, success *bool, attributes map[string]any) {
	if s.logger == nil {
		return
	}
	correlation := observability.FromContext(ctx)
	s.logger.Emit(ctx, observability.Event{
		Level:          level,
		EventType:      eventType,
		Message:        message,
		AssociationID:  correlation.AssociationID,
		TransactionID:  correlation.TransactionID,
		IdempotencyKey: correlation.IdempotencyKey,
		DurationMS:     durationMS,
		Success:        success,
		Attributes:     observability.RedactAttributes(attributes),
	})
}

const (
	WalletStatusActive = "ACTIVE"
	WalletStatusFrozen = "FROZEN"
	WalletStatusClosed = "CLOSED"
)

type WalletModel struct {
	ID        string           `bson:"_id" json:"id"`
	Currency  string           `bson:"currency,omitempty" json:"currency,omitempty"`
	Balance   int64            `bson:"balance,omitempty" json:"balance,omitempty"`
	Balances  map[string]int64 `bson:"balances,omitempty" json:"balances,omitempty"`
	Status    string           `bson:"status" json:"status"`
	KYCTier   string           `bson:"kyc_tier,omitempty" json:"kyc_tier,omitempty"`
	UpdatedAt string           `bson:"updated_at" json:"updated_at"`
}

func (w *WalletModel) EffectiveStatus() string {
	if w.Status == "" {
		return WalletStatusActive
	}
	return w.Status
}

func (w *WalletModel) EffectiveKYCTier() string {
	if w.KYCTier == "" {
		return KYCTier1
	}
	return w.KYCTier
}

func (w *WalletModel) GetAllBalances() []*walletv1.Money {
	if len(w.Balances) > 0 {
		out := make([]*walletv1.Money, 0, len(w.Balances))
		for curr, units := range w.Balances {
			out = append(out, &walletv1.Money{
				Currency: curr,
				Units:    units,
			})
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].Currency < out[j].Currency
		})
		return out
	}
	if w.Currency != "" {
		return []*walletv1.Money{
			{Currency: w.Currency, Units: w.Balance},
		}
	}
	return []*walletv1.Money{}
}

func (w *WalletModel) GetBalance(curr string) int64 {
	curr = db.NormalizeCurrency(curr)
	if len(w.Balances) > 0 {
		return w.Balances[curr]
	}
	if db.NormalizeCurrency(w.Currency) == curr {
		return w.Balance
	}
	return 0
}

func (w *WalletModel) HasSufficientBalance(curr string, amount int64) bool {
	if amount <= 0 {
		return false
	}
	return w.GetBalance(curr) >= amount
}

type AdminWalletStatusRequest struct {
	WalletID string `json:"wallet_id"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}

type AdminWalletStatusResponse struct {
	Success  bool   `json:"success"`
	WalletID string `json:"wallet_id"`
	Status   string `json:"status,omitempty"`
	Message  string `json:"message,omitempty"`
}

type IdempotencyRecord struct {
	ID            string    `bson:"_id"` // Idempotency key
	TransactionID string    `bson:"transaction_id"`
	CreatedAt     time.Time `bson:"created_at"`
}

func (s *server) HealthCheck(ctx context.Context, req *walletv1.HealthRequest) (*walletv1.HealthResponse, error) {
	active := s.isWritable(ctx)
	statusStr := "STANDBY"
	if active {
		statusStr = "ACTIVE"
	}
	return &walletv1.HealthResponse{
		Status:   statusStr,
		Region:   s.region,
		IsActive: active,
	}, nil
}

const (
	walletServiceName         = "wallet-service"
	eventWalletTransferFailed = "wallet.transfer.failed"
	errWalletIDRequired       = "wallet_id is required"
	errStandbyReplicaFmt      = "instance is standby replica; write operations rejected (region: %s)"
)

func (s *server) CreateWallet(ctx context.Context, req *walletv1.CreateWalletRequest) (*walletv1.CreateWalletResponse, error) {
	if req == nil || strings.TrimSpace(req.WalletId) == "" {
		return nil, status.Errorf(codes.InvalidArgument, errWalletIDRequired)
	}
	correlation := observability.FromIncomingContext(ctx)
	ctx = observability.WithCorrelation(ctx, correlation)
	s.emit(ctx, "wallet.create.request_received", observability.LevelInfo, "Create wallet request received", map[string]any{"wallet_id": req.WalletId})
	log.Printf("[WALLET] CreateWallet proto received: wallet_id=%s currency=%s initial_balance=%d region=%s", req.WalletId, req.Currency, req.InitialBalance, s.region)

	if !db.IsValidCurrency(req.Currency) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid or unsupported currency: %s", req.Currency)
	}
	if req.InitialBalance < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "initial balance cannot be negative: %d", req.InitialBalance)
	}

	if !s.isWritable(ctx) {
		return nil, status.Errorf(codes.FailedPrecondition, errStandbyReplicaFmt, s.region)
	}

	col := s.db().Collection("wallets")

	curr := db.NormalizeCurrency(req.Currency)
	if curr == "" {
		curr = "USD"
	}

	kycTier := strings.TrimSpace(req.KycTier)
	if kycTier == "" {
		kycTier = KYCTier1
	}
	if _, ok := TierConfigs[kycTier]; !ok {
		return nil, status.Errorf(codes.InvalidArgument, "invalid kyc_tier: %s (must be TIER_1, TIER_2, or TIER_3)", kycTier)
	}

	model := WalletModel{
		ID:        req.WalletId,
		Currency:  curr,
		Balance:   req.InitialBalance,
		Balances:  map[string]int64{curr: req.InitialBalance},
		Status:    WalletStatusActive,
		KYCTier:   kycTier,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	_, err := col.InsertOne(ctx, model)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, status.Errorf(codes.AlreadyExists, "wallet %s already exists", req.WalletId)
		}
		return nil, status.Errorf(codes.Internal, "failed to create wallet: %v", err)
	}

	if c := s.cache(); c != nil {
		c.Invalidate(req.WalletId)
	}

	log.Printf("[WALLET] Wallet initialized: ID=%s, Currency=%s, Balance=%d (Region: %s)",
		req.WalletId, curr, req.InitialBalance, s.region)

	return &walletv1.CreateWalletResponse{
		Success:         true,
		Message:         fmt.Sprintf("Wallet %s created successfully", req.WalletId),
		HandledByRegion: s.region,
	}, nil
}

func (s *server) GetBalance(ctx context.Context, req *walletv1.GetBalanceRequest) (*walletv1.GetBalanceResponse, error) {
	if req == nil || strings.TrimSpace(req.WalletId) == "" {
		return nil, status.Errorf(codes.InvalidArgument, errWalletIDRequired)
	}

	walletID := strings.TrimSpace(req.WalletId)

	// Check high-speed in-memory read-through cache first
	if c := s.cache(); c != nil {
		if cached, hit := c.Get(walletID); hit && cached != nil {
			return cached, nil
		}
	}

	correlation := observability.FromIncomingContext(ctx)
	ctx = observability.WithCorrelation(ctx, correlation)
	s.emit(ctx, "wallet.balance.request_received", observability.LevelInfo, "Get balance request received", map[string]any{"wallet_id": walletID})
	log.Printf("[WALLET] GetBalance proto received (cache-miss): wallet_id=%s region=%s", walletID, s.region)
	col := s.db().Collection("wallets")

	var wallet WalletModel
	err := col.FindOne(ctx, bson.M{"_id": walletID}).Decode(&wallet)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, status.Errorf(codes.NotFound, "wallet %s not found", walletID)
		}
		return nil, status.Errorf(codes.Internal, "db error: %v", err)
	}

	resp := &walletv1.GetBalanceResponse{
		WalletId:        walletID,
		Balances:        wallet.GetAllBalances(),
		HandledByRegion: s.region,
		Status:          wallet.EffectiveStatus(),
	}

	// Populate in-memory cache for subsequent sub-millisecond reads
	if c := s.cache(); c != nil {
		c.Set(walletID, resp)
	}

	return resp, nil
}

type transferExecutionState struct {
	finalTxnID      string
	outboxTaskID    primitive.ObjectID
	outboxTask      LedgerTask
	txnStatus       walletv1.TransferFundsResponse_Status
	txnErrMsg       string
	sourceDebited   bool
	destCurrency    string
	destUnits       int64
	exchangeRate    float64
	fxQuoteID       string
	isCrossCurrency bool
}

var fallbackBaselineUSDRates = map[string]float64{
	"USD": 1.0000,
	"EUR": 0.9200,
	"GBP": 0.7900,
	"CAD": 1.3600,
	"AUD": 1.5200,
	"JPY": 150.0000,
	"CHF": 0.8800,
	"SGD": 1.3400,
	"INR": 83.5000,
	"BHD": 0.3760,
}

func computeFallbackFX(base, target string, sourceUnits int64) (int64, float64, string) {
	base = db.NormalizeCurrency(base)
	target = db.NormalizeCurrency(target)
	if base == target {
		return sourceUnits, 1.0, ""
	}
	bRate := fallbackBaselineUSDRates[base]
	tRate := fallbackBaselineUSDRates[target]
	if bRate <= 0 || tRate <= 0 {
		return sourceUnits, 1.0, ""
	}
	midRate := tRate / bRate
	effectiveRate := midRate * (1.0 - 0.0025) // 25 bps standard spread
	targetUnits := db.ConvertUnitsScaleBankers(sourceUnits, base, target, effectiveRate)
	return targetUnits, effectiveRate, "fxq_local_fallback"
}

func (s *server) prepareFXConversion(ctx context.Context, req *walletv1.TransferFundsRequest, state *transferExecutionState) error {
	state.destCurrency = db.NormalizeCurrency(req.Amount.Currency)
	state.destUnits = req.Amount.Units
	state.exchangeRate = 1.0
	state.fxQuoteID = strings.TrimSpace(req.FxQuoteId)

	if s.db() == nil {
		return nil
	}
	walletsCol := s.db().Collection("wallets")
	var dstWallet WalletModel
	if err := walletsCol.FindOne(ctx, bson.M{"_id": req.DestinationWalletId}).Decode(&dstWallet); err != nil {
		// Let validateWalletsOperational inside the transaction handle missing destination wallet
		return nil
	}

	srcCurr := db.NormalizeCurrency(req.Amount.Currency)
	dstCurr := db.NormalizeCurrency(dstWallet.Currency)
	if srcCurr == dstCurr && state.fxQuoteID == "" {
		return nil
	}
	if s.fxClient == nil {
		return nil
	}

	state.isCrossCurrency = srcCurr != dstCurr
	state.destCurrency = dstCurr

	// Call standalone FX Engine microservice BEFORE opening the MongoDB ACID transaction (preserving GAP-FIN-01)
	if s.fxClient != nil {
		callCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		convResp, err := s.fxClient.ConvertCurrency(callCtx, &fxv1.ConvertCurrencyRequest{
			IdempotencyKey: "wallet_fx_" + req.IdempotencyKey,
			QuoteId:        state.fxQuoteID,
			BaseCurrency:   srcCurr,
			TargetCurrency: dstCurr,
			SourceAmount:   req.Amount.Units,
			ClientId:       walletServiceName,
			ReferenceId:    state.finalTxnID,
		})
		if err == nil && convResp != nil {
			state.destUnits = convResp.TargetAmount
			state.exchangeRate = convResp.EffectiveRate
			state.fxQuoteID = convResp.QuoteId
			s.emit(ctx, "wallet.transfer.fx_converted", observability.LevelInfo, "Cross-currency FX conversion resolved via fx-service", map[string]any{
				"source_currency": srcCurr,
				"target_currency": dstCurr,
				"source_amount":   req.Amount.Units,
				"target_amount":   state.destUnits,
				"exchange_rate":   state.exchangeRate,
				"fx_quote_id":     state.fxQuoteID,
			})
			return nil
		}
		if state.fxQuoteID != "" && err != nil {
			state.txnStatus = walletv1.TransferFundsResponse_INTERNAL_ERROR
			state.txnErrMsg = fmt.Sprintf("FX quote validation failed: %v", err)
			return err
		}
		log.Printf("[WALLET-FX] fx-service unreachable (%v); falling back to local institutional FX rate book", err)
	}

	targetUnits, effectiveRate, fallbackQuoteID := computeFallbackFX(srcCurr, dstCurr, req.Amount.Units)
	state.destUnits = targetUnits
	state.exchangeRate = effectiveRate
	if state.fxQuoteID == "" {
		state.fxQuoteID = fallbackQuoteID
	}
	return nil
}

func (s *server) validateTransferRequest(ctx context.Context, req *walletv1.TransferFundsRequest, startTime time.Time) (*walletv1.TransferFundsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "transfer request is required")
	}
	traceID := req.IdempotencyKey
	if req.IdempotencyKey == "" || req.SourceWalletId == "" || req.DestinationWalletId == "" || req.Amount == nil {
		log.Printf("[WALLET-TX] trace_id=%s step=validation_failed reason=missing_fields", traceID)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, eventWalletTransferFailed, observability.LevelError, "Validation failed", durationMS, false, map[string]any{
			"error_code":    "invalid_transfer_request",
			"error_message": "idempotency_key, source_wallet_id, destination_wallet_id, and amount are required",
			"step":          "validation",
		})
		return nil, status.Error(codes.InvalidArgument, "idempotency_key, source_wallet_id, destination_wallet_id, and amount are required")
	}
	if err := db.ValidateAmount(req.Amount.Units, req.Amount.Currency); err != nil {
		log.Printf("[WALLET-TX] trace_id=%s step=validation_failed reason=%v", traceID, err)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, eventWalletTransferFailed, observability.LevelError, "Validation failed", durationMS, false, map[string]any{
			"error_code":    "invalid_amount",
			"error_message": err.Error(),
			"step":          "validation",
		})
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if req.SourceWalletId == req.DestinationWalletId {
		log.Printf("[WALLET-TX] trace_id=%s step=validation_failed reason=identical_wallets", traceID)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, eventWalletTransferFailed, observability.LevelError, "Identical wallets", durationMS, false, map[string]any{
			"error_code":    "identical_wallets",
			"error_message": "source and destination wallets cannot be identical",
			"step":          "validation",
		})
		return &walletv1.TransferFundsResponse{
			Status:          walletv1.TransferFundsResponse_INTERNAL_ERROR,
			ErrorMessage:    "source and destination wallets cannot be identical",
			HandledByRegion: s.region,
		}, nil
	}
	if !s.isWritable(ctx) {
		log.Printf("[WALLET-TX] trace_id=%s step=standby_write_rejected region=%s", traceID, s.region)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, eventWalletTransferFailed, observability.LevelWarn, "Standby write rejected", durationMS, false, map[string]any{
			"error_code": "standby_write_rejected",
			"region":     s.region,
		})
		return &walletv1.TransferFundsResponse{
			Status:          walletv1.TransferFundsResponse_INTERNAL_ERROR,
			ErrorMessage:    fmt.Sprintf(errStandbyReplicaFmt, s.region),
			HandledByRegion: s.region,
		}, status.Errorf(codes.FailedPrecondition, errStandbyReplicaFmt, s.region)
	}
	// ── AI Fraud Detection (Phase AI-01) ─────────────────────────────────────
	// Runs last so all business validations take priority.
	// Fail-open: any AI error returns ALLOW — AI is never a payment blocker.
	if s.fraudDetector != nil && s.fraudDetector.IsEnabled() {
		signals := ai.FraudSignals{
			SourceWalletID:      req.SourceWalletId,
			DestinationWalletID: req.DestinationWalletId,
			AmountUnits:         req.Amount.Units,
			Currency:            req.Amount.Currency,
			Region:              s.region,
			Timestamp:           time.Now().UTC(),
			IsRoundNumber:       req.Amount.Units%10000 == 0,
		}
		decision, _ := s.fraudDetector.Score(ctx, signals)
		s.emit(ctx, "wallet.transfer.fraud_scored", observability.LevelInfo,
			"AI fraud score computed", map[string]any{
				"risk_score":      decision.RiskScore,
				"decision":        decision.Decision,
				"reason":          decision.Reason,
				"idempotency_key": req.IdempotencyKey,
			})
		if decision.Decision == "BLOCK" {
			durationMS := time.Since(startTime).Milliseconds()
			s.emitTerminal(ctx, eventWalletTransferFailed, observability.LevelWarn,
				"Transfer blocked by AI fraud detection", durationMS, false, map[string]any{
					"risk_score": decision.RiskScore,
					"reason":     decision.Reason,
				})
			return &walletv1.TransferFundsResponse{
				Status:          walletv1.TransferFundsResponse_INTERNAL_ERROR,
				ErrorMessage:    "Transfer blocked: " + decision.Reason,
				HandledByRegion: s.region,
			}, status.Errorf(codes.PermissionDenied, "blocked by fraud detection: %s", decision.Reason)
		}
	}
	// ── End AI Fraud Detection ────────────────────────────────────────────────
	return nil, nil
}

func (s *server) checkIdempotentTransfer(sessCtx mongo.SessionContext, ctx context.Context, idempCol *mongo.Collection, req *walletv1.TransferFundsRequest, state *transferExecutionState) bool {
	var existingIdemp IdempotencyRecord
	log.Printf("[WALLET-TX] trace_id=%s step=idempotency_check", req.IdempotencyKey)
	if err := idempCol.FindOne(sessCtx, bson.M{"_id": req.IdempotencyKey}).Decode(&existingIdemp); err == nil {
		log.Printf("[WALLET-TX] Idempotent request detected. Returning existing TX: %s", existingIdemp.TransactionID)
		state.finalTxnID = existingIdemp.TransactionID
		state.txnStatus = walletv1.TransferFundsResponse_REJECTED_DUPLICATE
		state.txnErrMsg = "Transaction already processed"
		s.emit(ctx, "wallet.transfer.idempotency_checked", observability.LevelDebug, "Duplicate transfer detected", map[string]any{
			"idempotency_key": req.IdempotencyKey,
			"is_replay":       true,
			"transaction_id":  state.finalTxnID,
		})
		return true
	}
	s.emit(ctx, "wallet.transfer.idempotency_checked", observability.LevelDebug, "Idempotency check passed", map[string]any{
		"idempotency_key": req.IdempotencyKey,
		"is_replay":       false,
	})
	return false
}

func checkWalletStatus(w WalletModel, role string) (string, error) {
	if w.EffectiveStatus() == WalletStatusFrozen {
		return fmt.Sprintf("%s wallet %s is FROZEN; transfers prohibited", role, w.ID), fmt.Errorf("%s wallet %s is FROZEN", role, w.ID)
	}
	if w.EffectiveStatus() == WalletStatusClosed {
		return fmt.Sprintf("%s wallet %s is CLOSED; transfers prohibited", role, w.ID), fmt.Errorf("%s wallet %s is CLOSED", role, w.ID)
	}
	return "", nil
}

func (s *server) validateWalletsOperational(sessCtx mongo.SessionContext, walletsCol *mongo.Collection, req *walletv1.TransferFundsRequest, state *transferExecutionState) error {
	var srcWallet WalletModel
	if err := walletsCol.FindOne(sessCtx, bson.M{"_id": req.SourceWalletId}).Decode(&srcWallet); err != nil {
		if err == mongo.ErrNoDocuments {
			state.txnStatus = walletv1.TransferFundsResponse_FAILED_INSUFFICIENT_FUNDS
			state.txnErrMsg = fmt.Sprintf("Source wallet %s not found", req.SourceWalletId)
			return fmt.Errorf("source wallet not found")
		}
		return err
	}
	if msg, err := checkWalletStatus(srcWallet, "source"); err != nil {
		state.txnStatus = walletv1.TransferFundsResponse_INTERNAL_ERROR
		state.txnErrMsg = msg
		return err
	}

	if err := s.CheckVelocityLimits(sessCtx, srcWallet, req.Amount.Units, req.Amount.Currency); err != nil {
		state.txnStatus = walletv1.TransferFundsResponse_FAILED_VELOCITY_LIMIT_EXCEEDED
		state.txnErrMsg = err.Error()
		log.Printf("[COMPLIANCE-TX] trace_id=%s step=velocity_limit_exceeded wallet_id=%s reason=%v",
			req.IdempotencyKey, req.SourceWalletId, err)
		return err
	}

	var dstWallet WalletModel
	if err := walletsCol.FindOne(sessCtx, bson.M{"_id": req.DestinationWalletId}).Decode(&dstWallet); err != nil {
		if err == mongo.ErrNoDocuments {
			state.txnStatus = walletv1.TransferFundsResponse_INTERNAL_ERROR
			state.txnErrMsg = fmt.Sprintf("Destination wallet %s not found", req.DestinationWalletId)
			return fmt.Errorf("destination wallet not found")
		}
		return err
	}
	if msg, err := checkWalletStatus(dstWallet, "destination"); err != nil {
		state.txnStatus = walletv1.TransferFundsResponse_INTERNAL_ERROR
		state.txnErrMsg = msg
		return err
	}
	return nil
}

func (s *server) executeDebitAndCredit(sessCtx mongo.SessionContext, ctx context.Context, walletsCol *mongo.Collection, req *walletv1.TransferFundsRequest, correlation observability.Correlation, state *transferExecutionState) error {
	destCurrency := state.destCurrency
	if destCurrency == "" {
		destCurrency = req.Amount.Currency
	}
	destUnits := state.destUnits
	if destUnits <= 0 {
		destUnits = req.Amount.Units
	}

	srcCurr := db.NormalizeCurrency(req.Amount.Currency)
	dstCurr := db.NormalizeCurrency(destCurrency)

	filterSource := bson.M{
		"_id": req.SourceWalletId,
		"$or": []bson.M{
			{"balances." + srcCurr: bson.M{"$gte": req.Amount.Units}},
			{"currency": srcCurr, "balance": bson.M{"$gte": req.Amount.Units}},
		},
	}
	updateSource := bson.M{
		"$inc": bson.M{
			"balance":             -req.Amount.Units,
			"balances." + srcCurr: -req.Amount.Units,
		},
		"$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)},
	}
	resSource, err := walletsCol.UpdateOne(sessCtx, filterSource, updateSource)
	if err != nil {
		return err
	}
	if resSource.ModifiedCount == 0 {
		state.txnStatus = walletv1.TransferFundsResponse_FAILED_INSUFFICIENT_FUNDS
		state.txnErrMsg = "Insufficient funds or source wallet not found"
		return fmt.Errorf("insufficient funds")
	}
	state.sourceDebited = true
	log.Printf("[WALLET-TX] trace_id=%s step=source_wallet_debited modified_count=%d", req.IdempotencyKey, resSource.ModifiedCount)

	auditDebitEvt := observability.Event{
		SchemaVersion:  1,
		EventID:        observability.NewAssociationID(),
		OccurredAt:     time.Now().UTC(),
		Service:        walletServiceName,
		Environment:    environmentName(),
		Region:         s.region,
		Level:          observability.LevelAudit,
		EventType:      "wallet.transfer.source_debited",
		Message:        "Source wallet debited",
		AssociationID:  correlation.AssociationID,
		TransactionID:  state.finalTxnID,
		IdempotencyKey: correlation.IdempotencyKey,
		Attributes: observability.RedactAttributes(map[string]any{
			"source_wallet": req.SourceWalletId,
			"debit_amount":  req.Amount.Units,
			"currency":      req.Amount.Currency,
		}),
	}
	s.emit(ctx, auditDebitEvt.EventType, auditDebitEvt.Level, auditDebitEvt.Message, auditDebitEvt.Attributes)
	if s.outbox != nil {
		if err := s.outbox.Append(sessCtx, auditDebitEvt); err != nil {
			log.Printf("[WALLET-TX] outbox append (source_debited) failed: %v", err)
			return fmt.Errorf("outbox append (source_debited) failed: %w", err)
		}
	}

	filterDest := bson.M{
		"_id":      req.DestinationWalletId,
		"currency": destCurrency,
	}
	updateDest := bson.M{
		"$inc": bson.M{
			"balance":             destUnits,
			"balances." + dstCurr: destUnits,
		},
		"$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)},
	}
	resDest, err := walletsCol.UpdateOne(sessCtx, filterDest, updateDest)
	if err != nil {
		return err
	}
	if resDest.ModifiedCount == 0 {
		state.txnStatus = walletv1.TransferFundsResponse_INTERNAL_ERROR
		state.txnErrMsg = "Destination wallet not found or currency mismatch"
		return fmt.Errorf("destination wallet not found")
	}
	log.Printf("[WALLET-TX] trace_id=%s step=destination_wallet_credited modified_count=%d credit_units=%d credit_currency=%s",
		req.IdempotencyKey, resDest.ModifiedCount, destUnits, destCurrency)

	auditCreditEvt := observability.Event{
		SchemaVersion:  1,
		EventID:        observability.NewAssociationID(),
		OccurredAt:     time.Now().UTC(),
		Service:        walletServiceName,
		Environment:    environmentName(),
		Region:         s.region,
		Level:          observability.LevelAudit,
		EventType:      "wallet.transfer.destination_credited",
		Message:        "Destination wallet credited",
		AssociationID:  correlation.AssociationID,
		TransactionID:  state.finalTxnID,
		IdempotencyKey: correlation.IdempotencyKey,
		Attributes: observability.RedactAttributes(map[string]any{
			"dest_wallet":   req.DestinationWalletId,
			"credit_amount": destUnits,
			"currency":      destCurrency,
			"exchange_rate": state.exchangeRate,
			"fx_quote_id":   state.fxQuoteID,
		}),
	}
	s.emit(ctx, auditCreditEvt.EventType, auditCreditEvt.Level, auditCreditEvt.Message, auditCreditEvt.Attributes)
	if s.outbox != nil {
		if err := s.outbox.Append(sessCtx, auditCreditEvt); err != nil {
			log.Printf("[WALLET-TX] outbox append (destination_credited) failed: %v", err)
			return fmt.Errorf("outbox append (destination_credited) failed: %w", err)
		}
	}

	// Record velocity usage for compliance tracking
	if recErr := s.RecordVelocityUsage(sessCtx, req.SourceWalletId, req.Amount.Units, req.Amount.Currency, "TRANSFER"); recErr != nil {
		log.Printf("[COMPLIANCE-TX] Warning: failed to record velocity usage for wallet %s: %v", req.SourceWalletId, recErr)
	}

	return nil
}

func (s *server) dispatchImmediateLedger(ctx context.Context, req *walletv1.TransferFundsRequest, task LedgerTask, finalTxnID string) {
	if s.ledgerRelay == nil {
		return
	}
	s.emit(ctx, "wallet.transfer.ledger_request_sent", observability.LevelDebug, "Ledger transaction record request sent", map[string]any{
		"source_wallet": req.SourceWalletId,
		"dest_wallet":   req.DestinationWalletId,
		"amount":        req.Amount.Units,
		"currency":      req.Amount.Currency,
	})
	if syncErr := s.ledgerRelay.DispatchImmediate(ctx, task); syncErr != nil {
		log.Printf("[WALLET-TX] trace_id=%s immediate ledger sync failed: %v (queued for background relay)", req.IdempotencyKey, syncErr)
	} else {
		log.Printf("[WALLET-TX] trace_id=%s immediate ledger sync succeeded", req.IdempotencyKey)
		s.emit(ctx, "wallet.transfer.ledger_response_received", observability.LevelDebug, "Ledger response received", map[string]any{
			"transaction_id": finalTxnID,
			"ledger_status":  "SUCCESS",
		})
	}
}

func (s *server) executeTransferTransaction(sessCtx mongo.SessionContext, ctx context.Context, req *walletv1.TransferFundsRequest, correlation observability.Correlation, state *transferExecutionState) error {
	traceID := req.IdempotencyKey
	log.Printf("[WALLET-TX] trace_id=%s step=mongo_transaction_started", traceID)
	walletsCol := s.db().Collection("wallets")
	idempCol := s.db().Collection("idempotency_records")
	ledgerTasksCol := s.db().Collection("ledger_tasks")

	if isDup := s.checkIdempotentTransfer(sessCtx, ctx, idempCol, req, state); isDup {
		return nil
	}

	if err := s.validateWalletsOperational(sessCtx, walletsCol, req, state); err != nil {
		return err
	}

	if err := s.executeDebitAndCredit(sessCtx, ctx, walletsCol, req, correlation, state); err != nil {
		return err
	}

	destCurrency := state.destCurrency
	if destCurrency == "" {
		destCurrency = req.Amount.Currency
	}
	destUnits := state.destUnits
	if destUnits <= 0 {
		destUnits = req.Amount.Units
	}

	state.outboxTask = LedgerTask{
		ID:                  state.outboxTaskID,
		TransactionID:       state.finalTxnID,
		IdempotencyKey:      req.IdempotencyKey,
		SourceWalletID:      req.SourceWalletId,
		DestinationWalletID: req.DestinationWalletId,
		Amount:              req.Amount.Units,
		Currency:            req.Amount.Currency,
		DestinationAmount:   destUnits,
		DestinationCurrency: destCurrency,
		ExchangeRate:        state.exchangeRate,
		FXQuoteID:           state.fxQuoteID,
		Region:              s.region,
		Status:              LedgerTaskStatusPending,
		CreatedAt:           time.Now().UTC(),
	}
	if err := AppendLedgerTask(sessCtx, ledgerTasksCol, state.outboxTask); err != nil {
		log.Printf("[WALLET-TX] failed to append ledger outbox task: %v", err)
		return fmt.Errorf("failed to append ledger outbox task: %w", err)
	}
	log.Printf("[WALLET-TX] trace_id=%s step=ledger_outbox_task_appended task_id=%s", traceID, state.outboxTaskID.Hex())

	_, err := idempCol.InsertOne(sessCtx, IdempotencyRecord{
		ID:            req.IdempotencyKey,
		TransactionID: state.finalTxnID,
		CreatedAt:     time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	log.Printf("[WALLET-TX] trace_id=%s step=idempotency_record_stored transaction_id=%s", traceID, state.finalTxnID)

	s.emit(ctx, "wallet.transfer.idempotency_record_stored", observability.LevelDebug, "Idempotency record stored", map[string]any{
		"idempotency_key": req.IdempotencyKey,
		"transaction_id":  state.finalTxnID,
	})
	return nil
}

func (s *server) handleTransferFailure(ctx context.Context, req *walletv1.TransferFundsRequest, state *transferExecutionState, err error, durationMS int64) *walletv1.TransferFundsResponse {
	traceID := req.IdempotencyKey
	log.Printf("[WALLET-TX] trace_id=%s step=mongo_transaction_failed error=%v", traceID, err)
	if state.txnStatus == walletv1.TransferFundsResponse_SUCCESS {
		state.txnStatus = walletv1.TransferFundsResponse_INTERNAL_ERROR
		state.txnErrMsg = err.Error()
	}

	if state.sourceDebited {
		s.emit(ctx, "wallet.transfer.rollback_completed", observability.LevelWarn, "Transaction aborted, balances rolled back", map[string]any{
			"source_wallet": req.SourceWalletId,
			"dest_wallet":   req.DestinationWalletId,
			"amount":        req.Amount.Units,
		})
	}

	s.emitTerminal(ctx, eventWalletTransferFailed, observability.LevelError, "Transfer transaction failed", durationMS, false, map[string]any{
		"error_code":    state.txnStatus.String(),
		"error_message": state.txnErrMsg,
		"step":          "mongo_transaction",
	})

	return &walletv1.TransferFundsResponse{
		TransactionId:   state.finalTxnID,
		Status:          state.txnStatus,
		ErrorMessage:    state.txnErrMsg,
		HandledByRegion: s.region,
	}
}

func (s *server) handleTransferSuccess(ctx context.Context, req *walletv1.TransferFundsRequest, state *transferExecutionState, durationMS int64) {
	traceID := req.IdempotencyKey
	if state.txnStatus == walletv1.TransferFundsResponse_SUCCESS {
		s.dispatchImmediateLedger(ctx, req, state.outboxTask, state.finalTxnID)
		// Evict both source and destination wallets immediately to prevent stale balance reads
		if c := s.cache(); c != nil {
			c.Invalidate(req.SourceWalletId, req.DestinationWalletId)
		}
	}

	log.Printf("[WALLET-TX] trace_id=%s step=wallet_proto_response_created status=%s transaction_id=%s", traceID, state.txnStatus.String(), state.finalTxnID)
	isSuccess := state.txnStatus == walletv1.TransferFundsResponse_SUCCESS
	s.emitTerminal(ctx, "wallet.transfer.completed", observability.LevelAudit, "Transfer completed successfully", durationMS, isSuccess, map[string]any{
		"status":         state.txnStatus.String(),
		"transaction_id": state.finalTxnID,
		"source_wallet":  req.SourceWalletId,
		"dest_wallet":    req.DestinationWalletId,
		"amount":         req.Amount.Units,
		"currency":       req.Amount.Currency,
		"dest_amount":    state.destUnits,
		"dest_currency":  state.destCurrency,
		"exchange_rate":  state.exchangeRate,
		"fx_quote_id":    state.fxQuoteID,
	})
	log.Printf("[WALLET-TX] SUCCESS: TX=%s | %s -> %s (%d %s -> %d %s @ %.6f) [Region: %s]",
		state.finalTxnID, req.SourceWalletId, req.DestinationWalletId, req.Amount.Units, req.Amount.Currency, state.destUnits, state.destCurrency, state.exchangeRate, s.region)
}

// TransferFunds executes an ACID multi-document MongoDB transaction
func (s *server) TransferFunds(ctx context.Context, req *walletv1.TransferFundsRequest) (*walletv1.TransferFundsResponse, error) {
	startTime := time.Now()
	if resp, err := s.validateTransferRequest(ctx, req, startTime); err != nil || resp != nil {
		return resp, err
	}

	correlation := observability.FromIncomingContext(ctx)
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = req.IdempotencyKey
	}
	ctx = observability.WithCorrelation(ctx, correlation)

	s.emit(ctx, "wallet.transfer.request_received", observability.LevelInfo, "Transfer request received", map[string]any{
		"source_wallet": req.SourceWalletId,
		"dest_wallet":   req.DestinationWalletId,
		"amount":        req.Amount.GetUnits(),
		"currency":      req.Amount.GetCurrency(),
	})
	log.Printf("[WALLET-TX] trace_id=%s step=wallet_proto_request_received source=%s destination=%s amount=%d currency=%s region=%s", req.IdempotencyKey, req.SourceWalletId, req.DestinationWalletId, req.Amount.GetUnits(), req.Amount.GetCurrency(), s.region)

	state := transferExecutionState{
		finalTxnID:   primitive.NewObjectID().Hex(),
		outboxTaskID: primitive.NewObjectID(),
		txnStatus:    walletv1.TransferFundsResponse_SUCCESS,
	}

	if fxErr := s.prepareFXConversion(ctx, req, &state); fxErr != nil {
		durationMS := time.Since(startTime).Milliseconds()
		return s.handleTransferFailure(ctx, req, &state, fxErr, durationMS), nil
	}

	session, err := s.mongoClient.StartSession()
	if err != nil {
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, eventWalletTransferFailed, observability.LevelError, "Failed to start mongo session", durationMS, false, map[string]any{
			"error_code":    "mongo_session_error",
			"error_message": err.Error(),
			"step":          "start_session",
		})
		return nil, status.Errorf(codes.Internal, "failed to start mongo session: %v", err)
	}
	defer session.EndSession(ctx)

	txnOpts := options.Transaction().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Snapshot()).
		SetReadPreference(readpref.Primary())

	_, err = session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		return nil, s.executeTransferTransaction(sessCtx, ctx, req, correlation, &state)
	}, txnOpts)

	durationMS := time.Since(startTime).Milliseconds()
	if err != nil {
		return s.handleTransferFailure(ctx, req, &state, err, durationMS), nil
	}

	s.handleTransferSuccess(ctx, req, &state, durationMS)
	resp := &walletv1.TransferFundsResponse{
		TransactionId:   state.finalTxnID,
		Status:          state.txnStatus,
		HandledByRegion: s.region,
		FxQuoteId:       state.fxQuoteID,
		ExchangeRate:    state.exchangeRate,
	}
	if state.destCurrency != "" && state.destUnits > 0 {
		resp.ConvertedAmount = &walletv1.Money{
			Currency: state.destCurrency,
			Units:    state.destUnits,
		}
	}
	return resp, nil
}

// ExchangeCurrency executes an atomic intra-wallet currency exchange between two currency pots.
func (s *server) ExchangeCurrency(ctx context.Context, req *walletv1.ExchangeCurrencyRequest) (*walletv1.ExchangeCurrencyResponse, error) {
	startTime := time.Now()
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "request is required")
	}
	idempKey := strings.TrimSpace(req.IdempotencyKey)
	walletID := strings.TrimSpace(req.WalletId)
	fromCurr := db.NormalizeCurrency(req.FromCurrency)
	toCurr := db.NormalizeCurrency(req.ToCurrency)
	fromAmount := req.FromAmount

	if idempKey == "" {
		return nil, status.Errorf(codes.InvalidArgument, "idempotency_key is required")
	}
	if walletID == "" {
		return nil, status.Errorf(codes.InvalidArgument, errWalletIDRequired)
	}
	if fromCurr == "" || toCurr == "" {
		return nil, status.Errorf(codes.InvalidArgument, "from_currency and to_currency are required")
	}
	if fromCurr == toCurr {
		return nil, status.Errorf(codes.InvalidArgument, "from_currency and to_currency must be different")
	}
	if fromAmount <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "from_amount must be strictly positive")
	}
	if err := db.ValidateAmount(fromAmount, fromCurr); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	correlation := observability.FromIncomingContext(ctx)
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = idempKey
	}
	ctx = observability.WithCorrelation(ctx, correlation)

	s.emit(ctx, "wallet.exchange.request_received", observability.LevelInfo, "Exchange currency request received", map[string]any{
		"wallet_id":     walletID,
		"from_currency": fromCurr,
		"to_currency":   toCurr,
		"from_amount":   fromAmount,
	})
	log.Printf("[WALLET-FX] trace_id=%s step=exchange_request_received wallet_id=%s from=%s to=%s amount=%d",
		idempKey, walletID, fromCurr, toCurr, fromAmount)

	if !s.isWritable(ctx) {
		log.Printf("[WALLET-TX] trace_id=%s step=standby_write_rejected region=%s", idempKey, s.region)
		return &walletv1.ExchangeCurrencyResponse{
			Status:          walletv1.ExchangeCurrencyResponse_INVALID_ARGUMENT,
			ErrorMessage:    fmt.Sprintf(errStandbyReplicaFmt, s.region),
			HandledByRegion: s.region,
		}, nil
	}

	if s.db() == nil {
		return nil, status.Errorf(codes.Unavailable, "database unavailable")
	}

	walletsCol := s.db().Collection("wallets")
	idempCol := s.db().Collection("idempotency_records")
	ledgerTasksCol := s.db().Collection("ledger_tasks")

	// Pre-transaction check for wallet existence & operational status
	var wallet WalletModel
	if err := walletsCol.FindOne(ctx, bson.M{"_id": walletID}).Decode(&wallet); err != nil {
		if err == mongo.ErrNoDocuments {
			return &walletv1.ExchangeCurrencyResponse{
				Status:          walletv1.ExchangeCurrencyResponse_FAILED_INSUFFICIENT_FUNDS,
				ErrorMessage:    fmt.Sprintf("wallet %s not found", walletID),
				HandledByRegion: s.region,
			}, nil
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch wallet: %v", err)
	}

	if msg, err := checkWalletStatus(wallet, "exchange"); err != nil {
		return &walletv1.ExchangeCurrencyResponse{
			Status:          walletv1.ExchangeCurrencyResponse_INTERNAL_ERROR,
			ErrorMessage:    msg,
			HandledByRegion: s.region,
		}, nil
	}

	if !wallet.HasSufficientBalance(fromCurr, fromAmount) {
		return &walletv1.ExchangeCurrencyResponse{
			Status:          walletv1.ExchangeCurrencyResponse_FAILED_INSUFFICIENT_FUNDS,
			ErrorMessage:    fmt.Sprintf("insufficient %s balance in wallet %s (available: %d)", fromCurr, walletID, wallet.GetBalance(fromCurr)),
			HandledByRegion: s.region,
		}, nil
	}

	if err := s.CheckVelocityLimits(ctx, wallet, fromAmount, fromCurr); err != nil {
		log.Printf("[COMPLIANCE-FX] trace_id=%s step=velocity_limit_exceeded wallet_id=%s reason=%v",
			idempKey, walletID, err)
		return &walletv1.ExchangeCurrencyResponse{
			Status:          walletv1.ExchangeCurrencyResponse_FAILED_VELOCITY_LIMIT_EXCEEDED,
			ErrorMessage:    err.Error(),
			HandledByRegion: s.region,
		}, nil
	}

	// Resolve FX conversion via fx-service or fallback
	quoteID := strings.TrimSpace(req.FxQuoteId)
	targetUnits := int64(0)
	effectiveRate := float64(0)
	finalTxnID := primitive.NewObjectID().Hex()

	if s.fxClient != nil {
		callCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		convResp, err := s.fxClient.ConvertCurrency(callCtx, &fxv1.ConvertCurrencyRequest{
			IdempotencyKey: "wallet_exchange_" + idempKey,
			QuoteId:        quoteID,
			BaseCurrency:   fromCurr,
			TargetCurrency: toCurr,
			SourceAmount:   fromAmount,
			ClientId:       walletServiceName,
			ReferenceId:    finalTxnID,
		})
		cancel()
		if err == nil && convResp != nil {
			targetUnits = convResp.TargetAmount
			effectiveRate = convResp.EffectiveRate
			quoteID = convResp.QuoteId
		}
	}
	if targetUnits <= 0 {
		targetUnits, effectiveRate, quoteID = computeFallbackFX(fromCurr, toCurr, fromAmount)
	}
	if targetUnits <= 0 {
		return &walletv1.ExchangeCurrencyResponse{
			Status:          walletv1.ExchangeCurrencyResponse_INVALID_ARGUMENT,
			ErrorMessage:    "converted target amount must be strictly positive",
			HandledByRegion: s.region,
		}, nil
	}

	outboxTaskID := primitive.NewObjectID()
	outboxTask := LedgerTask{
		ID:                  outboxTaskID,
		TransactionID:       finalTxnID,
		IdempotencyKey:      idempKey,
		SourceWalletID:      walletID,
		DestinationWalletID: walletID,
		Amount:              fromAmount,
		Currency:            fromCurr,
		DestinationAmount:   targetUnits,
		DestinationCurrency: toCurr,
		ExchangeRate:        effectiveRate,
		FXQuoteID:           quoteID,
		Region:              s.region,
		Status:              LedgerTaskStatusPending,
		CreatedAt:           time.Now().UTC(),
	}

	session, err := s.mongoClient.StartSession()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to start mongo session: %v", err)
	}
	defer session.EndSession(ctx)

	txnOpts := options.Transaction().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Snapshot()).
		SetReadPreference(readpref.Primary())

	_, err = session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		// 1. Idempotency check
		var existingIdemp IdempotencyRecord
		if findErr := idempCol.FindOne(sessCtx, bson.M{"_id": idempKey}).Decode(&existingIdemp); findErr == nil {
			finalTxnID = existingIdemp.TransactionID
			return nil, nil
		}

		// 2. Atomic debit from_currency pot & credit to_currency pot
		filterExchange := bson.M{
			"_id": walletID,
			"$or": []bson.M{
				{"balances." + fromCurr: bson.M{"$gte": fromAmount}},
				{"currency": fromCurr, "balance": bson.M{"$gte": fromAmount}},
			},
		}
		incMap := bson.M{
			"balances." + fromCurr: -fromAmount,
			"balances." + toCurr:   targetUnits,
		}
		if fromCurr == wallet.Currency {
			incMap["balance"] = -fromAmount
		}
		if toCurr == wallet.Currency {
			if b, ok := incMap["balance"]; ok {
				incMap["balance"] = b.(int64) + targetUnits
			} else {
				incMap["balance"] = targetUnits
			}
		}

		updateExchange := bson.M{
			"$inc": incMap,
			"$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)},
		}

		res, updateErr := walletsCol.UpdateOne(sessCtx, filterExchange, updateExchange)
		if updateErr != nil {
			return nil, updateErr
		}
		if res.ModifiedCount == 0 {
			return nil, fmt.Errorf("insufficient %s balance", fromCurr)
		}

		// 3. Append Ledger Outbox task
		if err := AppendLedgerTask(sessCtx, ledgerTasksCol, outboxTask); err != nil {
			return nil, fmt.Errorf("failed to append ledger task: %w", err)
		}

		// 4. Record idempotency record
		if _, err := idempCol.InsertOne(sessCtx, IdempotencyRecord{
			ID:            idempKey,
			TransactionID: finalTxnID,
			CreatedAt:     time.Now().UTC(),
		}); err != nil {
			return nil, err
		}

		// 5. Record velocity usage for compliance tracking
		if recErr := s.RecordVelocityUsage(sessCtx, walletID, fromAmount, fromCurr, "EXCHANGE"); recErr != nil {
			log.Printf("[COMPLIANCE-FX] Warning: failed to record velocity usage for wallet %s: %v", walletID, recErr)
		}
		return nil, nil
	}, txnOpts)

	durationMS := time.Since(startTime).Milliseconds()
	if err != nil {
		s.emitTerminal(ctx, "wallet.exchange.failed", observability.LevelError, "Intra-wallet exchange failed", durationMS, false, map[string]any{"error": err.Error()})
		return &walletv1.ExchangeCurrencyResponse{
			TransactionId:   finalTxnID,
			Status:          walletv1.ExchangeCurrencyResponse_FAILED_INSUFFICIENT_FUNDS,
			ErrorMessage:    err.Error(),
			HandledByRegion: s.region,
		}, nil
	}

	// Dispatch immediate ledger sync fast-path
	if s.ledgerRelay != nil {
		_ = s.ledgerRelay.DispatchImmediate(ctx, outboxTask)
	}

	// Invalidate local and multi-pod caches
	if c := s.cache(); c != nil {
		c.Invalidate(walletID)
	}

	s.emitTerminal(ctx, "wallet.exchange.completed", observability.LevelAudit, "Intra-wallet currency exchange completed successfully", durationMS, true, map[string]any{
		"transaction_id": finalTxnID,
		"wallet_id":      walletID,
		"from_currency":  fromCurr,
		"from_amount":    fromAmount,
		"to_currency":    toCurr,
		"to_amount":      targetUnits,
		"exchange_rate":  effectiveRate,
		"fx_quote_id":    quoteID,
	})

	log.Printf("[WALLET-FX] SUCCESS: TX=%s | Wallet %s converted %d %s -> %d %s @ %.6f (quote: %s)",
		finalTxnID, walletID, fromAmount, fromCurr, targetUnits, toCurr, effectiveRate, quoteID)

	return &walletv1.ExchangeCurrencyResponse{
		TransactionId:   finalTxnID,
		Status:          walletv1.ExchangeCurrencyResponse_SUCCESS,
		HandledByRegion: s.region,
		FxQuoteId:       quoteID,
		ExchangeRate:    effectiveRate,
		DebitedAmount: &walletv1.Money{
			Currency: fromCurr,
			Units:    fromAmount,
		},
		CreditedAmount: &walletv1.Money{
			Currency: toCurr,
			Units:    targetUnits,
		},
	}, nil
}

func parsePaymentRail(rail walletv1.PaymentRail) (string, error) {
	switch rail {
	case walletv1.PaymentRail_CARD:
		return "CARD", nil
	case walletv1.PaymentRail_ACH:
		return "ACH", nil
	case walletv1.PaymentRail_SEPA:
		return "SEPA", nil
	case walletv1.PaymentRail_WIRE:
		return "WIRE", nil
	default:
		return "", fmt.Errorf("unsupported or missing payment rail (supported: CARD, ACH, SEPA, WIRE)")
	}
}

// DepositFunds handles external inbound payment rail simulations (CARD, ACH, SEPA, WIRE)
// atomically crediting the wallet's multi-currency balance and generating balanced double-entry ledger tasks.
func (s *server) DepositFunds(ctx context.Context, req *walletv1.DepositFundsRequest) (*walletv1.DepositFundsResponse, error) {
	startTime := time.Now()
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "request is required")
	}
	idempKey := strings.TrimSpace(req.IdempotencyKey)
	walletID := strings.TrimSpace(req.WalletId)
	if idempKey == "" {
		return nil, status.Errorf(codes.InvalidArgument, "idempotency_key is required")
	}
	if walletID == "" {
		return nil, status.Errorf(codes.InvalidArgument, errWalletIDRequired)
	}
	if req.Amount == nil || req.Amount.Units <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "amount units must be strictly positive")
	}
	curr := db.NormalizeCurrency(req.Amount.Currency)
	if curr == "" {
		return nil, status.Errorf(codes.InvalidArgument, "currency is required")
	}
	if err := db.ValidateAmount(req.Amount.Units, curr); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	railStr, railErr := parsePaymentRail(req.PaymentRail)
	if railErr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", railErr)
	}

	correlation := observability.FromIncomingContext(ctx)
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = idempKey
	}
	ctx = observability.WithCorrelation(ctx, correlation)

	s.emit(ctx, "wallet.deposit.request_received", observability.LevelInfo, "Deposit funds request received", map[string]any{
		"wallet_id":    walletID,
		"currency":     curr,
		"amount":       req.Amount.Units,
		"payment_rail": railStr,
		"reference":    req.Reference,
	})

	if !s.isWritable(ctx) {
		return &walletv1.DepositFundsResponse{
			Status:          walletv1.DepositFundsResponse_INVALID_ARGUMENT,
			ErrorMessage:    fmt.Sprintf(errStandbyReplicaFmt, s.region),
			HandledByRegion: s.region,
		}, nil
	}

	if s.db() == nil {
		return nil, status.Errorf(codes.Unavailable, "database unavailable")
	}

	walletsCol := s.db().Collection("wallets")
	idempCol := s.db().Collection("idempotency_records")
	ledgerTasksCol := s.db().Collection("ledger_tasks")

	var wallet WalletModel
	if err := walletsCol.FindOne(ctx, bson.M{"_id": walletID}).Decode(&wallet); err != nil {
		if err == mongo.ErrNoDocuments {
			return &walletv1.DepositFundsResponse{
				Status:          walletv1.DepositFundsResponse_INVALID_ARGUMENT,
				ErrorMessage:    fmt.Sprintf("wallet %s not found", walletID),
				HandledByRegion: s.region,
			}, nil
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch wallet: %v", err)
	}

	if msg, err := checkWalletStatus(wallet, "deposit"); err != nil {
		return &walletv1.DepositFundsResponse{
			Status:          walletv1.DepositFundsResponse_INTERNAL_ERROR,
			ErrorMessage:    msg,
			HandledByRegion: s.region,
		}, nil
	}

	finalTxnID := primitive.NewObjectID().Hex()
	sourceClearing := fmt.Sprintf("EXTERNAL_CLEARING_%s_%s", railStr, curr)
	outboxTask := LedgerTask{
		ID:                  primitive.NewObjectID(),
		TransactionID:       finalTxnID,
		IdempotencyKey:      idempKey,
		SourceWalletID:      sourceClearing,
		DestinationWalletID: walletID,
		Amount:              req.Amount.Units,
		Currency:            curr,
		Region:              s.region,
		Status:              LedgerTaskStatusPending,
		CreatedAt:           time.Now().UTC(),
	}

	session, err := s.mongoClient.StartSession()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to start mongo session: %v", err)
	}
	defer session.EndSession(ctx)

	txnOpts := options.Transaction().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Snapshot()).
		SetReadPreference(readpref.Primary())

	_, err = session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		// 1. Idempotency check
		var existingIdemp IdempotencyRecord
		if findErr := idempCol.FindOne(sessCtx, bson.M{"_id": idempKey}).Decode(&existingIdemp); findErr == nil {
			finalTxnID = existingIdemp.TransactionID
			return nil, nil
		}

		// 2. Atomic credit wallet pot & base balance if matching
		incMap := bson.M{
			"balances." + curr: req.Amount.Units,
		}
		if curr == wallet.Currency {
			incMap["balance"] = req.Amount.Units
		}
		update := bson.M{
			"$inc": incMap,
			"$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)},
		}

		res, updateErr := walletsCol.UpdateOne(sessCtx, bson.M{"_id": walletID}, update)
		if updateErr != nil {
			return nil, updateErr
		}
		if res.MatchedCount == 0 {
			return nil, fmt.Errorf("wallet %s not found on deposit update", walletID)
		}

		// 3. Append Ledger Outbox task (credit customer liability, debit external clearing)
		if err := AppendLedgerTask(sessCtx, ledgerTasksCol, outboxTask); err != nil {
			return nil, fmt.Errorf("failed to append ledger task: %w", err)
		}

		// 4. Record idempotency record
		if _, err := idempCol.InsertOne(sessCtx, IdempotencyRecord{
			ID:            idempKey,
			TransactionID: finalTxnID,
			CreatedAt:     time.Now().UTC(),
		}); err != nil {
			return nil, err
		}

		return nil, nil
	}, txnOpts)

	durationMS := time.Since(startTime).Milliseconds()
	if err != nil {
		s.emitTerminal(ctx, "wallet.deposit.failed", observability.LevelError, "External deposit failed", durationMS, false, map[string]any{"error": err.Error()})
		return &walletv1.DepositFundsResponse{
			TransactionId:   finalTxnID,
			Status:          walletv1.DepositFundsResponse_INTERNAL_ERROR,
			ErrorMessage:    err.Error(),
			HandledByRegion: s.region,
		}, nil
	}

	// Dispatch immediate ledger sync fast-path
	if s.ledgerRelay != nil {
		_ = s.ledgerRelay.DispatchImmediate(ctx, outboxTask)
	}

	// Invalidate local and multi-pod caches
	if c := s.cache(); c != nil {
		c.Invalidate(walletID)
	}

	s.emitTerminal(ctx, "wallet.deposit.completed", observability.LevelAudit, "External deposit completed successfully", durationMS, true, map[string]any{
		"transaction_id":     finalTxnID,
		"wallet_id":          walletID,
		"amount":             req.Amount.Units,
		"currency":           curr,
		"payment_rail":       railStr,
		"external_reference": req.Reference,
	})

	return &walletv1.DepositFundsResponse{
		TransactionId:   finalTxnID,
		Status:          walletv1.DepositFundsResponse_SUCCESS,
		HandledByRegion: s.region,
		CreditedAmount: &walletv1.Money{
			Currency: curr,
			Units:    req.Amount.Units,
		},
		PaymentRail:       req.PaymentRail,
		ExternalReference: req.Reference,
	}, nil
}

// WithdrawFunds handles external outbound payment rail simulations (CARD, ACH, SEPA, WIRE)
// atomically debiting the wallet's multi-currency balance, enforcing velocity limits, and generating balanced ledger tasks.
func (s *server) WithdrawFunds(ctx context.Context, req *walletv1.WithdrawFundsRequest) (*walletv1.WithdrawFundsResponse, error) {
	startTime := time.Now()
	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "request is required")
	}
	idempKey := strings.TrimSpace(req.IdempotencyKey)
	walletID := strings.TrimSpace(req.WalletId)
	if idempKey == "" {
		return nil, status.Errorf(codes.InvalidArgument, "idempotency_key is required")
	}
	if walletID == "" {
		return nil, status.Errorf(codes.InvalidArgument, errWalletIDRequired)
	}
	if req.Amount == nil || req.Amount.Units <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "amount units must be strictly positive")
	}
	curr := db.NormalizeCurrency(req.Amount.Currency)
	if curr == "" {
		return nil, status.Errorf(codes.InvalidArgument, "currency is required")
	}
	if err := db.ValidateAmount(req.Amount.Units, curr); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	railStr, railErr := parsePaymentRail(req.PaymentRail)
	if railErr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", railErr)
	}

	correlation := observability.FromIncomingContext(ctx)
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = idempKey
	}
	ctx = observability.WithCorrelation(ctx, correlation)

	s.emit(ctx, "wallet.withdraw.request_received", observability.LevelInfo, "Withdraw funds request received", map[string]any{
		"wallet_id":           walletID,
		"currency":            curr,
		"amount":              req.Amount.Units,
		"payment_rail":        railStr,
		"beneficiary_details": req.BeneficiaryDetails,
	})

	if !s.isWritable(ctx) {
		return &walletv1.WithdrawFundsResponse{
			Status:          walletv1.WithdrawFundsResponse_INVALID_ARGUMENT,
			ErrorMessage:    fmt.Sprintf(errStandbyReplicaFmt, s.region),
			HandledByRegion: s.region,
		}, nil
	}

	if s.db() == nil {
		return nil, status.Errorf(codes.Unavailable, "database unavailable")
	}

	walletsCol := s.db().Collection("wallets")
	idempCol := s.db().Collection("idempotency_records")
	ledgerTasksCol := s.db().Collection("ledger_tasks")

	var wallet WalletModel
	if err := walletsCol.FindOne(ctx, bson.M{"_id": walletID}).Decode(&wallet); err != nil {
		if err == mongo.ErrNoDocuments {
			return &walletv1.WithdrawFundsResponse{
				Status:          walletv1.WithdrawFundsResponse_FAILED_INSUFFICIENT_FUNDS,
				ErrorMessage:    fmt.Sprintf("wallet %s not found", walletID),
				HandledByRegion: s.region,
			}, nil
		}
		return nil, status.Errorf(codes.Internal, "failed to fetch wallet: %v", err)
	}

	if msg, err := checkWalletStatus(wallet, "withdraw"); err != nil {
		return &walletv1.WithdrawFundsResponse{
			Status:          walletv1.WithdrawFundsResponse_INTERNAL_ERROR,
			ErrorMessage:    msg,
			HandledByRegion: s.region,
		}, nil
	}

	if !wallet.HasSufficientBalance(curr, req.Amount.Units) {
		return &walletv1.WithdrawFundsResponse{
			Status:          walletv1.WithdrawFundsResponse_FAILED_INSUFFICIENT_FUNDS,
			ErrorMessage:    fmt.Sprintf("insufficient %s balance in wallet %s (available: %d)", curr, walletID, wallet.GetBalance(curr)),
			HandledByRegion: s.region,
		}, nil
	}

	// Compliance & Velocity Limit Enforcement
	if err := s.CheckVelocityLimits(ctx, wallet, req.Amount.Units, curr); err != nil {
		log.Printf("[COMPLIANCE-WITHDRAW] trace_id=%s step=velocity_limit_exceeded wallet_id=%s reason=%v",
			idempKey, walletID, err)
		return &walletv1.WithdrawFundsResponse{
			Status:          walletv1.WithdrawFundsResponse_FAILED_VELOCITY_LIMIT_EXCEEDED,
			ErrorMessage:    err.Error(),
			HandledByRegion: s.region,
		}, nil
	}

	finalTxnID := primitive.NewObjectID().Hex()
	destClearing := fmt.Sprintf("EXTERNAL_CLEARING_%s_%s", railStr, curr)
	outboxTask := LedgerTask{
		ID:                  primitive.NewObjectID(),
		TransactionID:       finalTxnID,
		IdempotencyKey:      idempKey,
		SourceWalletID:      walletID,
		DestinationWalletID: destClearing,
		Amount:              req.Amount.Units,
		Currency:            curr,
		Region:              s.region,
		Status:              LedgerTaskStatusPending,
		CreatedAt:           time.Now().UTC(),
	}

	session, err := s.mongoClient.StartSession()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to start mongo session: %v", err)
	}
	defer session.EndSession(ctx)

	txnOpts := options.Transaction().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Snapshot()).
		SetReadPreference(readpref.Primary())

	_, err = session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		// 1. Idempotency check
		var existingIdemp IdempotencyRecord
		if findErr := idempCol.FindOne(sessCtx, bson.M{"_id": idempKey}).Decode(&existingIdemp); findErr == nil {
			finalTxnID = existingIdemp.TransactionID
			return nil, nil
		}

		// 2. Atomic debit wallet pot & base balance if matching
		filterWithdraw := bson.M{
			"_id": walletID,
			"$or": []bson.M{
				{"balances." + curr: bson.M{"$gte": req.Amount.Units}},
				{"currency": curr, "balance": bson.M{"$gte": req.Amount.Units}},
			},
		}
		incMap := bson.M{
			"balances." + curr: -req.Amount.Units,
		}
		if curr == wallet.Currency {
			incMap["balance"] = -req.Amount.Units
		}
		updateWithdraw := bson.M{
			"$inc": incMap,
			"$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)},
		}

		res, updateErr := walletsCol.UpdateOne(sessCtx, filterWithdraw, updateWithdraw)
		if updateErr != nil {
			return nil, updateErr
		}
		if res.ModifiedCount == 0 {
			return nil, fmt.Errorf("insufficient %s balance", curr)
		}

		// 3. Append Ledger Outbox task (debit customer liability, credit external clearing)
		if err := AppendLedgerTask(sessCtx, ledgerTasksCol, outboxTask); err != nil {
			return nil, fmt.Errorf("failed to append ledger task: %w", err)
		}

		// 4. Record idempotency record
		if _, err := idempCol.InsertOne(sessCtx, IdempotencyRecord{
			ID:            idempKey,
			TransactionID: finalTxnID,
			CreatedAt:     time.Now().UTC(),
		}); err != nil {
			return nil, err
		}

		// 5. Record velocity usage for compliance tracking
		if recErr := s.RecordVelocityUsage(sessCtx, walletID, req.Amount.Units, curr, "WITHDRAWAL_"+railStr); recErr != nil {
			log.Printf("[COMPLIANCE-WITHDRAW] Warning: failed to record velocity usage for wallet %s: %v", walletID, recErr)
		}
		return nil, nil
	}, txnOpts)

	durationMS := time.Since(startTime).Milliseconds()
	if err != nil {
		s.emitTerminal(ctx, "wallet.withdraw.failed", observability.LevelError, "External withdrawal failed", durationMS, false, map[string]any{"error": err.Error()})
		return &walletv1.WithdrawFundsResponse{
			TransactionId:   finalTxnID,
			Status:          walletv1.WithdrawFundsResponse_FAILED_INSUFFICIENT_FUNDS,
			ErrorMessage:    err.Error(),
			HandledByRegion: s.region,
		}, nil
	}

	// Dispatch immediate ledger sync fast-path
	if s.ledgerRelay != nil {
		_ = s.ledgerRelay.DispatchImmediate(ctx, outboxTask)
	}

	// Invalidate local and multi-pod caches
	if c := s.cache(); c != nil {
		c.Invalidate(walletID)
	}

	s.emitTerminal(ctx, "wallet.withdraw.completed", observability.LevelAudit, "External withdrawal completed successfully", durationMS, true, map[string]any{
		"transaction_id":      finalTxnID,
		"wallet_id":           walletID,
		"amount":              req.Amount.Units,
		"currency":            curr,
		"payment_rail":        railStr,
		"beneficiary_details": req.BeneficiaryDetails,
	})

	return &walletv1.WithdrawFundsResponse{
		TransactionId:   finalTxnID,
		Status:          walletv1.WithdrawFundsResponse_SUCCESS,
		HandledByRegion: s.region,
		DebitedAmount: &walletv1.Money{
			Currency: curr,
			Units:    req.Amount.Units,
		},
		PaymentRail:       req.PaymentRail,
		ExternalReference: req.BeneficiaryDetails,
	}, nil
}

func (s *server) UpdateWalletStatus(ctx context.Context, walletID, newStatus string) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("database unavailable")
	}
	col := db.Collection("wallets")
	filter := bson.M{"_id": walletID}
	update := bson.M{
		"$set": bson.M{
			"status":     newStatus,
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		},
	}
	res, err := col.UpdateOne(ctx, filter, update)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	if c := s.cache(); c != nil {
		c.Invalidate(walletID)
	}
	return nil
}

func (s *server) handleAdminWalletStatusGet(w http.ResponseWriter, r *http.Request) {
	walletID := strings.TrimSpace(r.URL.Query().Get("wallet_id"))
	if walletID == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
			Success: false,
			Message: "wallet_id query parameter is required",
		})
		return
	}
	db := s.db()
	if db == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
			Success: false,
			Message: "database unavailable",
		})
		return
	}
	col := db.Collection("wallets")
	var wallet WalletModel
	if err := col.FindOne(r.Context(), bson.M{"_id": walletID}).Decode(&wallet); err != nil {
		if err == mongo.ErrNoDocuments {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
				Success:  false,
				WalletID: walletID,
				Message:  "wallet not found",
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
			Success:  false,
			WalletID: walletID,
			Message:  "failed to fetch wallet: " + err.Error(),
		})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
		Success:  true,
		WalletID: wallet.ID,
		Status:   wallet.EffectiveStatus(),
		Message:  "wallet status retrieved",
	})
}

func (s *server) handleAdminWalletStatusUpdate(w http.ResponseWriter, r *http.Request) {
	var req AdminWalletStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
			Success: false,
			Message: "invalid JSON body: " + err.Error(),
		})
		return
	}
	req.WalletID = strings.TrimSpace(req.WalletID)
	req.Status = strings.ToUpper(strings.TrimSpace(req.Status))
	if req.WalletID == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
			Success: false,
			Message: errWalletIDRequired,
		})
		return
	}
	if req.Status != WalletStatusActive && req.Status != WalletStatusFrozen && req.Status != WalletStatusClosed {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
			Success: false,
			Message: fmt.Sprintf("invalid status %q: must be ACTIVE, FROZEN, or CLOSED", req.Status),
		})
		return
	}

	err := s.UpdateWalletStatus(r.Context(), req.WalletID, req.Status)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
				Success:  false,
				WalletID: req.WalletID,
				Message:  "wallet not found",
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
			Success:  false,
			WalletID: req.WalletID,
			Message:  "failed to update wallet status: " + err.Error(),
		})
		return
	}

	log.Printf("[WALLET-ADMIN] Wallet status updated: wallet_id=%s status=%s reason=%s region=%s",
		req.WalletID, req.Status, req.Reason, s.region)
	s.emit(r.Context(), "wallet.status.updated", observability.LevelInfo, "Wallet operational status updated", map[string]any{
		"wallet_id": req.WalletID,
		"status":    req.Status,
		"reason":    req.Reason,
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(AdminWalletStatusResponse{
		Success:  true,
		WalletID: req.WalletID,
		Status:   req.Status,
		Message:  fmt.Sprintf("Wallet %s status updated to %s", req.WalletID, req.Status),
	})
}

func (s *server) handleAdminWalletStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		s.handleAdminWalletStatusGet(w, r)
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		s.handleAdminWalletStatusUpdate(w, r)
		return
	}
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func dialLedgerClient(ledgerAddr string) (*grpc.ClientConn, ledgerv1.LedgerServiceClient, error) {
	log.Printf("[WALLET-SERVICE] Connecting to Ledger Service at %s...", ledgerAddr)
	dialOpt, err := getOutboundDialOption()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize outbound mTLS dial credentials: %w", err)
	}
	cb := resilience.NewClientCircuitBreaker(resilience.DefaultCircuitBreakerConfig("ledger-service"))
	ledgerDialOpts := []grpc.DialOption{
		dialOpt,
		grpc.WithChainUnaryInterceptor(
			observability.UnaryClientTraceInterceptor(walletServiceName),
			resilience.UnaryClientCircuitBreakerInterceptor(cb),
		),
	}
	conn, err := grpc.NewClient(ledgerAddr, ledgerDialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to ledger service: %w", err)
	}
	return conn, ledgerv1.NewLedgerServiceClient(conn), nil
}

func initWalletOutbox(ctx context.Context, client *mongo.Client) (*observability.MongoOutbox, *observability.RabbitPublisher) {
	if !observability.OutboxEnabled() {
		return nil, nil
	}
	rabbitURL := os.Getenv("LOGGING_RABBITMQ_URL")
	if rabbitURL == "" {
		log.Println("[WALLET-SERVICE] LOGGING_OUTBOX_ENABLED=true but LOGGING_RABBITMQ_URL is not set — outbox disabled")
		return nil, nil
	}
	log.Println("[WALLET-SERVICE] Transactional outbox ENABLED")
	walletOutbox := observability.NewMongoOutbox(client.Database("banking_db"), "wallet_outbox")
	idxCtx, idxCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := walletOutbox.EnsureIndexes(idxCtx); err != nil {
		log.Printf("[WALLET-SERVICE] outbox index creation warning: %v", err)
	}
	idxCancel()
	tlsCfg, _ := observability.TLSConfigFromEnv()
	outboxPublisher := observability.NewRabbitPublisherWithTLS(rabbitURL, "", tlsCfg)
	relay := observability.NewOutboxRelay(client.Database("banking_db"), "wallet_outbox", outboxPublisher)
	relay.Start(ctx)
	return walletOutbox, outboxPublisher
}

func startWalletMetricsServer(srv *server, region string, isActive bool) *http.Server {
	metricsPort := os.Getenv("METRICS_PORT")
	if metricsPort == "" {
		metricsPort = "9094"
		if !isActive {
			metricsPort = "9093"
		}
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", observability.DefaultMetrics.Handler())
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		active := srv.isWritable(r.Context())
		st := "STANDBY"
		if active {
			st = "ACTIVE"
		}
		fmt.Fprintf(w, `{"status":%q,"region":%q,"is_active":%t}`+"\n", st, region, active)
	})
	metricsMux.HandleFunc("/admin/wallet/status", srv.handleAdminWalletStatus)
	server := &http.Server{
		Addr:              ":" + metricsPort,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("[WALLET-SERVICE] Management metrics server listening on :%s/metrics", metricsPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[WALLET-SERVICE] Metrics server error: %v", err)
		}
	}()
	return server
}

func resolveWalletRole(isActive bool) string {
	targetRole := os.Getenv("COORDINATOR_ROLE")
	if targetRole != "" {
		return targetRole
	}
	if isActive {
		return coordinator.TargetPrimary
	}
	return coordinator.TargetStandby
}

func resolveLedgerAddress() string {
	ledgerAddr := os.Getenv("LEDGER_SERVICE_ADDR")
	if ledgerAddr != "" {
		return ledgerAddr
	}
	if _, err := net.LookupHost("ledger-service"); err == nil {
		return "ledger-service:50052"
	}
	return "127.0.0.1:50052"
}

func resolveFXAddress() string {
	fxAddr := os.Getenv("FX_SERVICE_ADDR")
	if fxAddr != "" {
		return fxAddr
	}
	if _, err := net.LookupHost("fx-service"); err == nil {
		return "fx-service:50055"
	}
	return "127.0.0.1:50055"
}

func dialFXClient(fxAddr string) (*grpc.ClientConn, fxv1.FXServiceClient, error) {
	log.Printf("[WALLET-SERVICE] Connecting to FX Service at %s...", fxAddr)
	dialOpt, err := getOutboundDialOption()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize outbound mTLS dial credentials: %w", err)
	}
	cb := resilience.NewClientCircuitBreaker(resilience.DefaultCircuitBreakerConfig("fx-service"))
	fxDialOpts := []grpc.DialOption{
		dialOpt,
		grpc.WithChainUnaryInterceptor(
			observability.UnaryClientTraceInterceptor(walletServiceName),
			resilience.UnaryClientCircuitBreakerInterceptor(cb),
		),
	}
	conn, err := grpc.NewClient(fxAddr, fxDialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to fx service: %w", err)
	}
	return conn, fxv1.NewFXServiceClient(conn), nil
}

func setupWalletGRPCServer(port string) (*grpc.Server, *health.Server, net.Listener, error) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to listen on port %s: %w", port, err)
	}

	serverOpts, err := getServerOptions()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to configure server mTLS credentials: %w", err)
	}
	serverOpts = append(serverOpts, grpc.ChainUnaryInterceptor(
		observability.UnaryServerTraceInterceptor(walletServiceName),
		observability.DefaultMetrics.UnaryServerMetricsInterceptor(walletServiceName),
	))
	grpcServer := grpc.NewServer(serverOpts...)

	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("wallet.v1.WalletService", grpc_health_v1.HealthCheckResponse_SERVING)
	return grpcServer, healthServer, lis, nil
}

func handleWalletShutdown(grpcServer *grpc.Server, healthServer *health.Server, metricsServer *http.Server, ledgerRelay *LedgerRelay, outboxPublisher *observability.RabbitPublisher, client *mongo.Client) {
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	healthServer.SetServingStatus("wallet.v1.WalletService", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	grpcServer.GracefulStop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[WALLET-SERVICE] Metrics server shutdown error: %v", err)
	}
	if ledgerRelay != nil {
		ledgerRelay.Stop()
	}
	if outboxPublisher != nil {
		outboxPublisher.Close()
	}
	if client != nil {
		_ = client.Disconnect(shutdownCtx)
	}
	log.Println("[WALLET-SERVICE] Graceful shutdown completed cleanly.")
}

func initWalletCoordinator(subCtx context.Context, isActive bool) (*mongo.Client, coordinator.FailoverCoordinator, func(), error) {
	if os.Getenv("TEST_MOCK_DB") == "true" {
		log.Println("[WALLET-SERVICE] TEST_MOCK_DB=true — running in mock DB mode")
		memCoord := coordinator.NewMemoryFailoverCoordinator(resolveWalletRole(isActive))
		cleanup := func() {
			memCoord.Close()
		}
		return nil, memCoord, cleanup, nil
	}

	client, err := db.ConnectWithRetry(subCtx, db.DefaultMongoURI(), 2)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not connect to MongoDB: %w", err)
	}

	walletIdxCtx, walletIdxCancel := context.WithTimeout(subCtx, 10*time.Second)
	if err := db.EnsureWalletIndexes(walletIdxCtx, client.Database("banking_db")); err != nil {
		log.Printf("[WALLET-SERVICE] Wallet database index creation warning: %v", err)
	}
	walletIdxCancel()

	mongoCoord := coordinator.NewMongoFailoverCoordinator(client.Database("banking_db"), 1*time.Second)
	cleanup := func() {
		mongoCoord.Close()
		_ = client.Disconnect(context.Background())
	}
	return client, mongoCoord, cleanup, nil
}

func runWalletServer(ctx context.Context) error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "50051"
	}
	region := os.Getenv("REGION_NAME")
	if region == "" {
		region = "us-east-1"
	}
	isActive, _ := strconv.ParseBool(os.Getenv("IS_ACTIVE"))
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "local"
	}

	log.Printf("[WALLET-SERVICE] Starting in Region: %s (Active: %t) on port %s...", region, isActive, port)

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	client, coord, cleanupCoord, err := initWalletCoordinator(subCtx, isActive)
	if err != nil {
		return err
	}
	if cleanupCoord != nil {
		defer cleanupCoord()
	}

	shutdownTracer, err := observability.InitTracer(walletServiceName)
	if err == nil && shutdownTracer != nil {
		defer func() { _ = shutdownTracer(context.Background()) }()
	}

	conn, ledgerClient, err := dialLedgerClient(resolveLedgerAddress())
	if err != nil {
		return fmt.Errorf("failed to initialize ledger client: %w", err)
	}
	defer conn.Close()

	fxConn, fxClient, fxErr := dialFXClient(resolveFXAddress())
	if fxErr != nil {
		log.Printf("[WALLET-SERVICE] FX client dial warning (%v) — using local FX fallback", fxErr)
	} else {
		defer fxConn.Close()
	}

	grpcServer, healthServer, lis, err := setupWalletGRPCServer(port)
	if err != nil {
		return fmt.Errorf("failed to setup gRPC server: %w", err)
	}

	var ledgerRelay *LedgerRelay
	if client != nil {
		leaseTTL := 10 * time.Second
		if val := os.Getenv("OUTBOX_LEASE_TTL_SECONDS"); val != "" {
			if sec, err := strconv.Atoi(val); err == nil && sec > 0 {
				leaseTTL = time.Duration(sec) * time.Second
			}
		}
		db := client.Database("banking_db")
		instanceID := observability.ResolveInstanceID()
		leaseManager := NewMongoRelayLeaseManager(db, instanceID, leaseTTL)
		ledgerRelay = NewLedgerRelay(db, ledgerClient, 2*time.Second, 50, nil, WithLeaseManager(leaseManager))
		ledgerRelay.Start(subCtx)
	}

	walletOutbox, outboxPublisher := initWalletOutbox(subCtx, client)

	// ── AI Fraud Detection (Phase AI-01) ─────────────────────────────────────
	// NewClient returns nil (not an error) when GEMINI_API_KEY is not set,
	// so all AI features are simply disabled — no crashes, no impact on transfers.
	aiClient, aiErr := ai.NewClient(subCtx)
	if aiErr != nil {
		log.Printf("[WALLET-SERVICE] AI client init warning: %v (AI disabled)", aiErr)
	}
	if aiClient != nil && aiClient.IsEnabled() {
		log.Printf("[WALLET-SERVICE] AI fraud detection ENABLED (model=%s enforcement=%s)",
			os.Getenv("GEMINI_MODEL"), os.Getenv("AI_FRAUD_ENFORCEMENT"))
		defer aiClient.Close()
	} else {
		log.Printf("[WALLET-SERVICE] AI fraud detection DISABLED (set GEMINI_API_KEY to enable)")
	}
	// ── End AI Init ───────────────────────────────────────────────────────────

	balanceCache := NewBalanceCacheFromEnv(walletServiceName)
	if balanceCache != nil {
		defer balanceCache.Close()
		log.Printf("[WALLET-SERVICE] Wallet balance in-memory cache ENABLED (ttl=%v maxEntries=%d)", balanceCache.ttl, balanceCache.maxEntries)
	}

	srv := &server{
		mongoClient:   client,
		ledgerClient:  ledgerClient,
		fxClient:      fxClient,
		region:        region,
		isActive:      isActive,
		targetRole:    resolveWalletRole(isActive),
		coordinator:   coord,
		logger:        observability.LoggerFromEnvironment(walletServiceName, environment, region, os.Stdout),
		outbox:        walletOutbox,
		ledgerRelay:   ledgerRelay,
		aiClient:      aiClient,
		fraudDetector: ai.NewFraudDetector(aiClient),
		balanceCache:  balanceCache,
	}
	walletv1.RegisterWalletServiceServer(grpcServer, srv)

	metricsServer := startWalletMetricsServer(srv, region, isActive)

	go func() {
		log.Printf("[WALLET-SERVICE] Listening for gRPC requests on :%s", port)
		if err := grpcServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			log.Printf("gRPC server stopped: %v", err)
		}
	}()

	<-subCtx.Done()
	log.Printf("[WALLET-SERVICE] Context cancelled, initiating graceful shutdown...")

	handleWalletShutdown(grpcServer, healthServer, metricsServer, ledgerRelay, outboxPublisher, client)
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWalletServer(ctx); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
}

func environmentName() string {
	if environment := os.Getenv("ENVIRONMENT"); environment != "" {
		return environment
	}
	return "development"
}

func getOutboundDialOption() (grpc.DialOption, error) {
	if os.Getenv("GRPC_TLS_ENABLED") == "true" {
		certFile := os.Getenv("GRPC_CLIENT_CERT")
		keyFile := os.Getenv("GRPC_CLIENT_KEY")
		caFile := os.Getenv("GRPC_CA_CERT")
		serverName := os.Getenv("GRPC_SERVER_NAME")
		creds, err := tlsutil.NewClientTransportCredentials(certFile, keyFile, caFile, serverName)
		if err != nil {
			return nil, fmt.Errorf("failed to load client mTLS credentials: %w", err)
		}
		log.Println("[SECURITY] Outbound ledger gRPC dialed with mTLS transport credentials")
		return grpc.WithTransportCredentials(creds), nil
	}
	return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
}

func getServerOptions() ([]grpc.ServerOption, error) {
	if os.Getenv("GRPC_TLS_ENABLED") == "true" {
		certFile := os.Getenv("GRPC_SERVER_CERT")
		keyFile := os.Getenv("GRPC_SERVER_KEY")
		caFile := os.Getenv("GRPC_CA_CERT")
		creds, err := tlsutil.NewServerTransportCredentials(certFile, keyFile, caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load server mTLS credentials: %w", err)
		}
		log.Println("[SECURITY] Wallet gRPC server configured with mTLS transport credentials")
		return []grpc.ServerOption{grpc.Creds(creds)}, nil
	}
	return nil, nil
}
