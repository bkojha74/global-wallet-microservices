package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"wallet-system/pkg/db"
	fxv1 "wallet-system/proto/fx"
)

const (
	QuoteStatusActive   = "ACTIVE"
	QuoteStatusExecuted = "EXECUTED"
	QuoteStatusExpired  = "EXPIRED"

	ConversionStatusCompleted = "COMPLETED"
	ConversionStatusReplay    = "DUPLICATE_REPLAY"

	defaultQuoteTTLSeconds = 60
	defaultSpreadBps       = 25 // 0.25% standard institutional FX spread
)

// ExchangeRateDoc represents a persisted currency pair rate in fx_db.exchange_rates.
type ExchangeRateDoc struct {
	BaseCurrency   string    `bson:"base_currency" json:"base_currency"`
	TargetCurrency string    `bson:"target_currency" json:"target_currency"`
	MidRate        float64   `bson:"mid_rate" json:"mid_rate"`
	BidRate        float64   `bson:"bid_rate" json:"bid_rate"`
	AskRate        float64   `bson:"ask_rate" json:"ask_rate"`
	EffectiveRate  float64   `bson:"effective_rate" json:"effective_rate"`
	InverseRate    float64   `bson:"inverse_rate" json:"inverse_rate"`
	SpreadBps      int32     `bson:"spread_bps" json:"spread_bps"`
	SourceProvider string    `bson:"source_provider" json:"source_provider"`
	UpdatedAt      time.Time `bson:"updated_at" json:"updated_at"`
}

// QuoteDoc represents a fixed-rate FX quote stored in fx_db.fx_quotes.
type QuoteDoc struct {
	QuoteID        string    `bson:"_id" json:"quote_id"`
	BaseCurrency   string    `bson:"base_currency" json:"base_currency"`
	TargetCurrency string    `bson:"target_currency" json:"target_currency"`
	SourceAmount   int64     `bson:"source_amount" json:"source_amount"`
	TargetAmount   int64     `bson:"target_amount" json:"target_amount"`
	FeeAmount      int64     `bson:"fee_amount" json:"fee_amount"`
	MidRate        float64   `bson:"mid_rate" json:"mid_rate"`
	EffectiveRate  float64   `bson:"effective_rate" json:"effective_rate"`
	SpreadBps      int32     `bson:"spread_bps" json:"spread_bps"`
	Status         string    `bson:"status" json:"status"`
	ClientID       string    `bson:"client_id" json:"client_id"`
	SourceProvider string    `bson:"source_provider" json:"source_provider"`
	CreatedAt      time.Time `bson:"created_at" json:"created_at"`
	ExpiresAt      time.Time `bson:"expires_at" json:"expires_at"`
}

// ConversionDoc represents an executed FX conversion in fx_db.fx_conversions.
type ConversionDoc struct {
	ConversionID   string    `bson:"_id" json:"conversion_id"`
	IdempotencyKey string    `bson:"idempotency_key" json:"idempotency_key"`
	QuoteID        string    `bson:"quote_id" json:"quote_id"`
	BaseCurrency   string    `bson:"base_currency" json:"base_currency"`
	TargetCurrency string    `bson:"target_currency" json:"target_currency"`
	SourceAmount   int64     `bson:"source_amount" json:"source_amount"`
	TargetAmount   int64     `bson:"target_amount" json:"target_amount"`
	FeeAmount      int64     `bson:"fee_amount" json:"fee_amount"`
	MidRate        float64   `bson:"mid_rate" json:"mid_rate"`
	EffectiveRate  float64   `bson:"effective_rate" json:"effective_rate"`
	SpreadBps      int32     `bson:"spread_bps" json:"spread_bps"`
	ClientID       string    `bson:"client_id" json:"client_id"`
	ReferenceID    string    `bson:"reference_id,omitempty" json:"reference_id,omitempty"`
	SourceProvider string    `bson:"source_provider" json:"source_provider"`
	ExecutedAt     time.Time `bson:"executed_at" json:"executed_at"`
}

// FXEngine manages live exchange rate books, triangulation, bid/ask spreads,
// fixed-rate RFQ quotes, and idempotent conversions.
type FXEngine struct {
	mu             sync.RWMutex
	usdRates       map[string]float64         // Currency -> MidRate relative to 1 USD
	pairOverrides  map[string]ExchangeRateDoc // "BASE:TARGET" -> manual/direct override
	quotesMem      map[string]*QuoteDoc       // In-memory quote store & cache
	conversionsMem map[string]*ConversionDoc  // In-memory idempotency store & cache
	spreadBps      int32                      // Default spread in basis points
	feeBps         int32                      // Default fee in basis points
	sourceProvider string
	lastSyncedAt   time.Time
	provider       RateProvider
	fxDB           *mongo.Database
	cancelSync     context.CancelFunc
}

func NewFXEngine(fxDB *mongo.Database, provider RateProvider, spreadBps, feeBps int32) *FXEngine {
	if provider == nil {
		provider = &BaselineProvider{}
	}
	if spreadBps < 0 {
		spreadBps = defaultSpreadBps
	}
	if feeBps < 0 {
		feeBps = 0
	}
	usdRates := make(map[string]float64, len(InstitutionalBaselineRates))
	for k, v := range InstitutionalBaselineRates {
		usdRates[k] = v
	}
	return &FXEngine{
		usdRates:       usdRates,
		pairOverrides:  make(map[string]ExchangeRateDoc),
		quotesMem:      make(map[string]*QuoteDoc),
		conversionsMem: make(map[string]*ConversionDoc),
		spreadBps:      spreadBps,
		feeBps:         feeBps,
		sourceProvider: "institutional-baseline",
		lastSyncedAt:   time.Now().UTC(),
		provider:       provider,
		fxDB:           fxDB,
	}
}

func roundPips(val float64) float64 {
	return math.Round(val*1e6) / 1e6
}

// SyncUpstreamRates queries the configured 3rd-party RateProvider and updates the rate book.
func (e *FXEngine) SyncUpstreamRates(ctx context.Context) (int, string, error) {
	snap, err := e.provider.FetchRates(ctx, "USD")
	if err != nil {
		return 0, "", err
	}

	e.mu.Lock()
	for curr, rate := range snap.Rates {
		code := db.NormalizeCurrency(curr)
		if db.IsValidCurrency(code) && rate > 0 {
			e.usdRates[code] = rate
		}
	}
	e.usdRates["USD"] = 1.0
	e.sourceProvider = snap.ProviderName
	e.lastSyncedAt = snap.FetchedAt
	e.mu.Unlock()

	// Persist all supported cross-pairs to MongoDB if connected
	pairsUpdated := e.persistAllPairs(ctx)
	return pairsUpdated, snap.ProviderName, nil
}

func (e *FXEngine) persistAllPairs(ctx context.Context) int {
	currencies := db.ListSupportedCurrencies()
	count := 0
	for _, base := range currencies {
		for _, target := range currencies {
			if base.Code == target.Code {
				continue
			}
			count++
			if e.fxDB != nil {
				rateProto, err := e.GetRate(ctx, base.Code, target.Code)
				if err == nil && rateProto != nil {
					col := e.fxDB.Collection("exchange_rates")
					filter := bson.M{
						"base_currency":   base.Code,
						"target_currency": target.Code,
					}
					update := bson.M{
						"$set": ExchangeRateDoc{
							BaseCurrency:   base.Code,
							TargetCurrency: target.Code,
							MidRate:        rateProto.MidRate,
							BidRate:        rateProto.BidRate,
							AskRate:        rateProto.AskRate,
							EffectiveRate:  rateProto.EffectiveRate,
							InverseRate:    rateProto.InverseRate,
							SpreadBps:      rateProto.SpreadBps,
							SourceProvider: rateProto.SourceProvider,
							UpdatedAt:      time.Now().UTC(),
						},
					}
					_, _ = col.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
				}
			}
		}
	}
	return count
}

// StartBackgroundSync starts a background goroutine that periodically syncs rates from 3rd-party APIs.
func (e *FXEngine) StartBackgroundSync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	syncCtx, cancel := context.WithCancel(ctx)
	e.cancelSync = cancel

	// Initial sync at startup
	if pairs, prov, err := e.SyncUpstreamRates(syncCtx); err != nil {
		log.Printf("[FX-ENGINE] Initial upstream sync warning: %v", err)
	} else {
		log.Printf("[FX-ENGINE] Initial rate book synced (%d pairs via %s)", pairs, prov)
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if pairs, prov, err := e.SyncUpstreamRates(syncCtx); err != nil {
					log.Printf("[FX-ENGINE] Periodic upstream sync warning: %v", err)
				} else {
					log.Printf("[FX-ENGINE] Periodic rate book refreshed (%d pairs via %s)", pairs, prov)
				}
			case <-syncCtx.Done():
				log.Println("[FX-ENGINE] Background rate sync worker stopped.")
				return
			}
		}
	}()
}

func (e *FXEngine) Stop() {
	if e.cancelSync != nil {
		e.cancelSync()
	}
}

// SetPairRate manually sets or overrides a specific currency pair's mid-market rate and spread.
func (e *FXEngine) SetPairRate(ctx context.Context, base, target string, midRate float64, spreadBps int32, provider string) (*fxv1.ExchangeRate, error) {
	base = db.NormalizeCurrency(base)
	target = db.NormalizeCurrency(target)
	if !db.IsValidCurrency(base) {
		return nil, fmt.Errorf("invalid or unsupported base currency: %s", base)
	}
	if !db.IsValidCurrency(target) {
		return nil, fmt.Errorf("invalid or unsupported target currency: %s", target)
	}
	if midRate <= 0 {
		return nil, fmt.Errorf("mid_rate must be strictly positive, got %f", midRate)
	}
	if spreadBps < 0 || spreadBps >= 5000 {
		return nil, fmt.Errorf("spread_bps must be between 0 and 4999, got %d", spreadBps)
	}
	if provider == "" {
		provider = "manual-override"
	}

	doc := computeRateDoc(base, target, midRate, spreadBps, provider, time.Now().UTC())

	e.mu.Lock()
	e.pairOverrides[base+":"+target] = doc
	if base == "USD" {
		e.usdRates[target] = midRate
	} else if target == "USD" && midRate > 0 {
		e.usdRates[base] = 1.0 / midRate
	}
	e.lastSyncedAt = doc.UpdatedAt
	e.mu.Unlock()

	if e.fxDB != nil {
		col := e.fxDB.Collection("exchange_rates")
		filter := bson.M{"base_currency": base, "target_currency": target}
		_, _ = col.UpdateOne(ctx, filter, bson.M{"$set": doc}, options.Update().SetUpsert(true))
	}

	return rateDocToProto(doc), nil
}

func computeRateDoc(base, target string, midRate float64, spreadBps int32, provider string, ts time.Time) ExchangeRateDoc {
	if base == target {
		return ExchangeRateDoc{
			BaseCurrency:   base,
			TargetCurrency: target,
			MidRate:        1.0,
			BidRate:        1.0,
			AskRate:        1.0,
			EffectiveRate:  1.0,
			InverseRate:    1.0,
			SpreadBps:      0,
			SourceProvider: provider,
			UpdatedAt:      ts,
		}
	}

	spreadFraction := float64(spreadBps) / 10000.0
	mid := roundPips(midRate)
	bid := roundPips(midRate * (1.0 - spreadFraction/2.0))
	ask := roundPips(midRate * (1.0 + spreadFraction/2.0))
	effective := roundPips(midRate * (1.0 - spreadFraction))
	if effective <= 0 {
		effective = mid
	}
	inverse := roundPips(1.0 / effective)

	return ExchangeRateDoc{
		BaseCurrency:   base,
		TargetCurrency: target,
		MidRate:        mid,
		BidRate:        bid,
		AskRate:        ask,
		EffectiveRate:  effective,
		InverseRate:    inverse,
		SpreadBps:      spreadBps,
		SourceProvider: provider,
		UpdatedAt:      ts,
	}
}

func rateDocToProto(doc ExchangeRateDoc) *fxv1.ExchangeRate {
	baseScale, _ := db.CurrencyScale(doc.BaseCurrency)
	targetScale, _ := db.CurrencyScale(doc.TargetCurrency)
	return &fxv1.ExchangeRate{
		BaseCurrency:   doc.BaseCurrency,
		TargetCurrency: doc.TargetCurrency,
		MidRate:        doc.MidRate,
		BidRate:        doc.BidRate,
		AskRate:        doc.AskRate,
		EffectiveRate:  doc.EffectiveRate,
		InverseRate:    doc.InverseRate,
		SpreadBps:      doc.SpreadBps,
		SourceProvider: doc.SourceProvider,
		UpdatedAt:      doc.UpdatedAt.UTC().Format(time.RFC3339),
		// #nosec G115 -- ISO-4217 minor unit scale exponent is between 0 and 4
		BaseScale: int32(baseScale),
		// #nosec G115 -- ISO-4217 minor unit scale exponent is between 0 and 4
		TargetScale: int32(targetScale),
	}
}

// GetRate returns the current exchange rate for (base -> target) using direct overrides or USD triangulation.
func (e *FXEngine) GetRate(_ context.Context, base, target string) (*fxv1.ExchangeRate, error) {
	base = db.NormalizeCurrency(base)
	target = db.NormalizeCurrency(target)
	if !db.IsValidCurrency(base) {
		return nil, fmt.Errorf("invalid or unsupported base currency: %s", base)
	}
	if !db.IsValidCurrency(target) {
		return nil, fmt.Errorf("invalid or unsupported target currency: %s", target)
	}

	e.mu.RLock()
	if override, ok := e.pairOverrides[base+":"+target]; ok {
		e.mu.RUnlock()
		return rateDocToProto(override), nil
	}

	baseToUSD := e.usdRates[base]
	targetToUSD := e.usdRates[target]
	spread := e.spreadBps
	provider := e.sourceProvider
	ts := e.lastSyncedAt
	e.mu.RUnlock()

	if baseToUSD <= 0 || targetToUSD <= 0 {
		return nil, fmt.Errorf("exchange rate unavailable for pair %s/%s", base, target)
	}

	midRate := targetToUSD / baseToUSD
	doc := computeRateDoc(base, target, midRate, spread, provider, ts)
	return rateDocToProto(doc), nil
}

// ListRates returns all supported currency pair rates for the requested base currency.
func (e *FXEngine) ListRates(ctx context.Context, base string) ([]*fxv1.ExchangeRate, string, string, error) {
	base = db.NormalizeCurrency(base)
	if base == "" {
		base = "USD"
	}
	if !db.IsValidCurrency(base) {
		return nil, "", "", fmt.Errorf("invalid or unsupported base currency: %s", base)
	}

	currencies := db.ListSupportedCurrencies()
	rates := make([]*fxv1.ExchangeRate, 0, len(currencies))
	for _, info := range currencies {
		r, err := e.GetRate(ctx, base, info.Code)
		if err != nil {
			continue
		}
		rates = append(rates, r)
	}
	sort.Slice(rates, func(i, j int) bool {
		return rates[i].TargetCurrency < rates[j].TargetCurrency
	})

	e.mu.RLock()
	prov := e.sourceProvider
	ts := e.lastSyncedAt.UTC().Format(time.RFC3339)
	e.mu.RUnlock()

	return rates, prov, ts, nil
}

// CreateQuote generates a locked fixed-rate FX quote with TTL expiration.
func (e *FXEngine) CreateQuote(ctx context.Context, base, target string, sourceAmount int64, clientID string, ttlSeconds int32) (*fxv1.Quote, error) {
	base = db.NormalizeCurrency(base)
	target = db.NormalizeCurrency(target)
	if err := db.ValidateAmount(sourceAmount, base); err != nil {
		return nil, err
	}
	if !db.IsValidCurrency(target) {
		return nil, fmt.Errorf("invalid or unsupported target currency: %s", target)
	}

	rate, err := e.GetRate(ctx, base, target)
	if err != nil {
		return nil, err
	}

	if ttlSeconds <= 0 {
		ttlSeconds = defaultQuoteTTLSeconds
	}
	if ttlSeconds > 3600 {
		ttlSeconds = 3600
	}
	if strings.TrimSpace(clientID) == "" {
		clientID = "anonymous-client"
	}

	e.mu.RLock()
	feeBps := e.feeBps
	e.mu.RUnlock()

	var feeAmount int64
	if feeBps > 0 && base != target {
		feeAmount = int64(math.RoundToEven(float64(sourceAmount) * float64(feeBps) / 10000.0))
	}
	netSource := sourceAmount - feeAmount
	if netSource <= 0 {
		netSource = sourceAmount
		feeAmount = 0
	}

	targetAmount := db.ConvertUnitsScaleBankers(netSource, base, target, rate.EffectiveRate)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(ttlSeconds) * time.Second)
	quoteID := "fxq_" + primitive.NewObjectID().Hex()

	doc := &QuoteDoc{
		QuoteID:        quoteID,
		BaseCurrency:   base,
		TargetCurrency: target,
		SourceAmount:   sourceAmount,
		TargetAmount:   targetAmount,
		FeeAmount:      feeAmount,
		MidRate:        rate.MidRate,
		EffectiveRate:  rate.EffectiveRate,
		SpreadBps:      rate.SpreadBps,
		Status:         QuoteStatusActive,
		ClientID:       clientID,
		SourceProvider: rate.SourceProvider,
		CreatedAt:      now,
		ExpiresAt:      expiresAt,
	}

	e.mu.Lock()
	e.quotesMem[quoteID] = doc
	e.mu.Unlock()

	if e.fxDB != nil {
		_, _ = e.fxDB.Collection("fx_quotes").InsertOne(ctx, doc)
	}

	return quoteDocToProto(doc), nil
}

// GetQuote retrieves a quote by ID and evaluates whether its TTL window has expired.
func (e *FXEngine) GetQuote(ctx context.Context, quoteID string) (*fxv1.Quote, error) {
	quoteID = strings.TrimSpace(quoteID)
	if quoteID == "" {
		return nil, fmt.Errorf("quote_id is required")
	}

	doc, err := e.loadQuoteDoc(ctx, quoteID)
	if err != nil {
		return nil, err
	}

	if doc.Status == QuoteStatusActive && time.Now().UTC().After(doc.ExpiresAt) {
		doc.Status = QuoteStatusExpired
		e.mu.Lock()
		e.quotesMem[quoteID] = doc
		e.mu.Unlock()
		if e.fxDB != nil {
			_, _ = e.fxDB.Collection("fx_quotes").UpdateOne(ctx, bson.M{"_id": quoteID}, bson.M{"$set": bson.M{"status": QuoteStatusExpired}})
		}
	}

	return quoteDocToProto(doc), nil
}

func (e *FXEngine) loadQuoteDoc(ctx context.Context, quoteID string) (*QuoteDoc, error) {
	e.mu.RLock()
	if cached, ok := e.quotesMem[quoteID]; ok {
		copyDoc := *cached
		e.mu.RUnlock()
		return &copyDoc, nil
	}
	e.mu.RUnlock()

	if e.fxDB != nil {
		var doc QuoteDoc
		err := e.fxDB.Collection("fx_quotes").FindOne(ctx, bson.M{"_id": quoteID}).Decode(&doc)
		if err == nil {
			e.mu.Lock()
			e.quotesMem[quoteID] = &doc
			e.mu.Unlock()
			return &doc, nil
		}
	}

	return nil, fmt.Errorf("quote %s not found", quoteID)
}

func quoteDocToProto(doc *QuoteDoc) *fxv1.Quote {
	return &fxv1.Quote{
		QuoteId:        doc.QuoteID,
		BaseCurrency:   doc.BaseCurrency,
		TargetCurrency: doc.TargetCurrency,
		SourceAmount:   doc.SourceAmount,
		TargetAmount:   doc.TargetAmount,
		FeeAmount:      doc.FeeAmount,
		MidRate:        doc.MidRate,
		EffectiveRate:  doc.EffectiveRate,
		SpreadBps:      doc.SpreadBps,
		Status:         doc.Status,
		ClientId:       doc.ClientID,
		SourceProvider: doc.SourceProvider,
		CreatedAt:      doc.CreatedAt.UTC().Format(time.RFC3339),
		ExpiresAt:      doc.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// ConvertCurrency executes an idempotent FX conversion either by consuming a locked quote
// or by pricing an immediate spot conversion.
func (e *FXEngine) ConvertCurrency(ctx context.Context, req *fxv1.ConvertCurrencyRequest) (*fxv1.ConvertCurrencyResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("conversion request is required")
	}
	idempKey := strings.TrimSpace(req.IdempotencyKey)
	if idempKey == "" {
		return nil, fmt.Errorf("idempotency_key is required")
	}

	// 1. Check idempotency store (memory + MongoDB)
	if existing, found := e.findExistingConversion(ctx, idempKey); found {
		return conversionDocToProto(existing, ConversionStatusReplay), nil
	}

	quoteID := strings.TrimSpace(req.QuoteId)
	clientID := strings.TrimSpace(req.ClientId)
	if clientID == "" {
		clientID = "global-wallet-service"
	}

	var base, target, provider string
	var srcAmt, dstAmt, feeAmt int64
	var midRate, effectiveRate float64
	var spreadBps int32

	if quoteID != "" {
		// Locked Quote Path
		qDoc, err := e.loadQuoteDoc(ctx, quoteID)
		if err != nil {
			return nil, err
		}
		if qDoc.Status == QuoteStatusExecuted {
			return nil, fmt.Errorf("quote %s has already been executed", quoteID)
		}
		if qDoc.Status == QuoteStatusExpired || time.Now().UTC().After(qDoc.ExpiresAt) {
			return nil, fmt.Errorf("quote %s has expired", quoteID)
		}
		if req.BaseCurrency != "" && db.NormalizeCurrency(req.BaseCurrency) != qDoc.BaseCurrency {
			return nil, fmt.Errorf("quote base_currency mismatch: quote=%s request=%s", qDoc.BaseCurrency, req.BaseCurrency)
		}
		if req.TargetCurrency != "" && db.NormalizeCurrency(req.TargetCurrency) != qDoc.TargetCurrency {
			return nil, fmt.Errorf("quote target_currency mismatch: quote=%s request=%s", qDoc.TargetCurrency, req.TargetCurrency)
		}
		if req.SourceAmount > 0 && req.SourceAmount != qDoc.SourceAmount {
			return nil, fmt.Errorf("quote source_amount mismatch: quote=%d request=%d", qDoc.SourceAmount, req.SourceAmount)
		}

		// Mark quote as EXECUTED
		qDoc.Status = QuoteStatusExecuted
		e.mu.Lock()
		e.quotesMem[quoteID] = qDoc
		e.mu.Unlock()
		if e.fxDB != nil {
			_, _ = e.fxDB.Collection("fx_quotes").UpdateOne(ctx, bson.M{"_id": quoteID}, bson.M{"$set": bson.M{"status": QuoteStatusExecuted}})
		}

		base = qDoc.BaseCurrency
		target = qDoc.TargetCurrency
		srcAmt = qDoc.SourceAmount
		dstAmt = qDoc.TargetAmount
		feeAmt = qDoc.FeeAmount
		midRate = qDoc.MidRate
		effectiveRate = qDoc.EffectiveRate
		spreadBps = qDoc.SpreadBps
		provider = qDoc.SourceProvider
	} else {
		// Spot Conversion Path
		base = db.NormalizeCurrency(req.BaseCurrency)
		target = db.NormalizeCurrency(req.TargetCurrency)
		if err := db.ValidateAmount(req.SourceAmount, base); err != nil {
			return nil, err
		}
		if !db.IsValidCurrency(target) {
			return nil, fmt.Errorf("invalid or unsupported target currency: %s", target)
		}

		quote, err := e.CreateQuote(ctx, base, target, req.SourceAmount, clientID, defaultQuoteTTLSeconds)
		if err != nil {
			return nil, err
		}
		quoteID = quote.QuoteId
		base = quote.BaseCurrency
		target = quote.TargetCurrency
		srcAmt = quote.SourceAmount
		dstAmt = quote.TargetAmount
		feeAmt = quote.FeeAmount
		midRate = quote.MidRate
		effectiveRate = quote.EffectiveRate
		spreadBps = quote.SpreadBps
		provider = quote.SourceProvider

		// Mark spot quote as EXECUTED immediately
		e.mu.Lock()
		if qDoc, ok := e.quotesMem[quoteID]; ok {
			qDoc.Status = QuoteStatusExecuted
		}
		e.mu.Unlock()
		if e.fxDB != nil {
			_, _ = e.fxDB.Collection("fx_quotes").UpdateOne(ctx, bson.M{"_id": quoteID}, bson.M{"$set": bson.M{"status": QuoteStatusExecuted}})
		}
	}

	now := time.Now().UTC()
	convDoc := &ConversionDoc{
		ConversionID:   "fxc_" + primitive.NewObjectID().Hex(),
		IdempotencyKey: idempKey,
		QuoteID:        quoteID,
		BaseCurrency:   base,
		TargetCurrency: target,
		SourceAmount:   srcAmt,
		TargetAmount:   dstAmt,
		FeeAmount:      feeAmt,
		MidRate:        midRate,
		EffectiveRate:  effectiveRate,
		SpreadBps:      spreadBps,
		ClientID:       clientID,
		ReferenceID:    req.ReferenceId,
		SourceProvider: provider,
		ExecutedAt:     now,
	}

	e.mu.Lock()
	e.conversionsMem[idempKey] = convDoc
	e.mu.Unlock()

	if e.fxDB != nil {
		_, err := e.fxDB.Collection("fx_conversions").InsertOne(ctx, convDoc)
		if err != nil && mongo.IsDuplicateKeyError(err) {
			if existing, found := e.findExistingConversion(ctx, idempKey); found {
				return conversionDocToProto(existing, ConversionStatusReplay), nil
			}
		}
	}

	return conversionDocToProto(convDoc, ConversionStatusCompleted), nil
}

func (e *FXEngine) findExistingConversion(ctx context.Context, idempKey string) (*ConversionDoc, bool) {
	e.mu.RLock()
	if cached, ok := e.conversionsMem[idempKey]; ok {
		copyDoc := *cached
		e.mu.RUnlock()
		return &copyDoc, true
	}
	e.mu.RUnlock()

	if e.fxDB != nil {
		var doc ConversionDoc
		if err := e.fxDB.Collection("fx_conversions").FindOne(ctx, bson.M{"idempotency_key": idempKey}).Decode(&doc); err == nil {
			e.mu.Lock()
			e.conversionsMem[idempKey] = &doc
			e.mu.Unlock()
			return &doc, true
		}
	}
	return nil, false
}

func conversionDocToProto(doc *ConversionDoc, status string) *fxv1.ConvertCurrencyResponse {
	return &fxv1.ConvertCurrencyResponse{
		ConversionId:   doc.ConversionID,
		QuoteId:        doc.QuoteID,
		BaseCurrency:   doc.BaseCurrency,
		TargetCurrency: doc.TargetCurrency,
		SourceAmount:   doc.SourceAmount,
		TargetAmount:   doc.TargetAmount,
		FeeAmount:      doc.FeeAmount,
		MidRate:        doc.MidRate,
		EffectiveRate:  doc.EffectiveRate,
		SpreadBps:      doc.SpreadBps,
		Status:         status,
		SourceProvider: doc.SourceProvider,
		ExecutedAt:     doc.ExecutedAt.UTC().Format(time.RFC3339),
	}
}

func (e *FXEngine) HealthSummary() (string, int32, int32, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	// #nosec G115 -- currency count is bounded by ISO-4217 catalog size
	numCurrencies := int32(len(db.SupportedCurrencies))
	activePairs := numCurrencies * (numCurrencies - 1)
	return e.sourceProvider, numCurrencies, activePairs, e.lastSyncedAt.UTC().Format(time.RFC3339)
}
