package multientity

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Supported banking legal entities
const (
	EntityUSCorp = "US_CORP"
	EntityEUGmbh = "EU_GMBH"
	EntityGBLtd  = "GB_LTD"
	EntitySGPte  = "SG_PTE"
)

var (
	ErrEntityNotFound     = errors.New("legal entity not found")
	ErrWalletAlreadyBound = errors.New("wallet is already registered to another legal entity")
	ErrInvalidWalletID    = errors.New("wallet ID cannot be empty")
	ErrInvalidAmount      = errors.New("settlement amount must be positive")
	ErrSameEntityTransfer = errors.New("transfer is intra-entity; cross-entity omnibus not required")
)

// LegalEntity represents a regulated financial banking corporate subsidiary.
type LegalEntity struct {
	ID           string    `json:"id"`
	LegalName    string    `json:"legal_name"`
	Jurisdiction string    `json:"jurisdiction"`  // US, DE, GB, SG
	BaseCurrency string    `json:"base_currency"` // USD, EUR, GBP, SGD
	Regulator    string    `json:"regulator"`     // FinCEN, BaFin, FCA, MAS
	Status       string    `json:"status"`        // ACTIVE, SUSPENDED
	CreatedAt    time.Time `json:"created_at"`
}

// OmnibusPosting represents an atomic double-entry leg in an inter-entity clearing journal.
type OmnibusPosting struct {
	EntityID      string    `json:"entity_id"`
	AccountNumber string    `json:"account_number"`
	AccountName   string    `json:"account_name"`
	Direction     string    `json:"direction"` // DEBIT or CREDIT
	Amount        float64   `json:"amount"`
	Currency      string    `json:"currency"`
	Description   string    `json:"description"`
	Timestamp     time.Time `json:"timestamp"`
}

// CrossEntityTransferResult contains the coordinated 4-legged inter-entity clearing record.
type CrossEntityTransferResult struct {
	TransactionID   string           `json:"transaction_id"`
	SourceEntityID  string           `json:"source_entity_id"`
	TargetEntityID  string           `json:"target_entity_id"`
	IsCrossEntity   bool             `json:"is_cross_entity"`
	SourceWalletID  string           `json:"source_wallet_id"`
	TargetWalletID  string           `json:"target_wallet_id"`
	Amount          float64          `json:"amount"`
	Currency        string           `json:"currency"`
	OmnibusPostings []OmnibusPosting `json:"omnibus_postings"`
	Status          string           `json:"status"` // SETTLED, PENDING
	SettledAt       time.Time        `json:"settled_at"`
}

// EntityRegistry manages corporate entity registrations and customer wallet multi-entity partitioning.
type EntityRegistry struct {
	mu          sync.RWMutex
	entities    map[string]*LegalEntity
	walletBinds map[string]string // walletID -> entityID
}

// NewEntityRegistry creates an EntityRegistry pre-loaded with global banking subsidiaries.
func NewEntityRegistry() *EntityRegistry {
	now := time.Now().UTC()
	r := &EntityRegistry{
		entities: map[string]*LegalEntity{
			EntityUSCorp: {
				ID:           EntityUSCorp,
				LegalName:    "Global Digital Wallet Inc.",
				Jurisdiction: "US",
				BaseCurrency: "USD",
				Regulator:    "FinCEN / OCC",
				Status:       "ACTIVE",
				CreatedAt:    now,
			},
			EntityEUGmbh: {
				ID:           EntityEUGmbh,
				LegalName:    "Global Digital Wallet Europe GmbH",
				Jurisdiction: "DE",
				BaseCurrency: "EUR",
				Regulator:    "BaFin / Deutsche Bundesbank",
				Status:       "ACTIVE",
				CreatedAt:    now,
			},
			EntityGBLtd: {
				ID:           EntityGBLtd,
				LegalName:    "Global Digital Wallet UK Ltd",
				Jurisdiction: "GB",
				BaseCurrency: "GBP",
				Regulator:    "Financial Conduct Authority (FCA)",
				Status:       "ACTIVE",
				CreatedAt:    now,
			},
			EntitySGPte: {
				ID:           EntitySGPte,
				LegalName:    "Global Digital Wallet (Asia-Pacific) Pte. Ltd.",
				Jurisdiction: "SG",
				BaseCurrency: "SGD",
				Regulator:    "Monetary Authority of Singapore (MAS)",
				Status:       "ACTIVE",
				CreatedAt:    now,
			},
		},
		walletBinds: make(map[string]string),
	}
	return r
}

// RegisterWalletEntity maps a wallet ID to a legal entity.
func (r *EntityRegistry) RegisterWalletEntity(walletID, entityID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	walletID = strings.TrimSpace(walletID)
	if walletID == "" {
		return ErrInvalidWalletID
	}
	if _, ok := r.entities[entityID]; !ok {
		return ErrEntityNotFound
	}
	if existing, ok := r.walletBinds[walletID]; ok && existing != entityID {
		return ErrWalletAlreadyBound
	}

	r.walletBinds[walletID] = entityID
	return nil
}

// GetWalletEntity resolves the legal entity owning the given wallet ID.
// If not explicitly bound, defaults by wallet prefix or currency hints.
func (r *EntityRegistry) GetWalletEntity(walletID string) (*LegalEntity, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	walletID = strings.TrimSpace(walletID)
	if walletID == "" {
		return nil, ErrInvalidWalletID
	}

	if entID, ok := r.walletBinds[walletID]; ok {
		if ent, exists := r.entities[entID]; exists {
			return ent, nil
		}
	}

	// Heuristic default mapping based on wallet naming conventions
	lower := strings.ToLower(walletID)
	switch {
	case strings.Contains(lower, "eu") || strings.Contains(lower, "de") || strings.Contains(lower, "eur"):
		return r.entities[EntityEUGmbh], nil
	case strings.Contains(lower, "uk") || strings.Contains(lower, "gb") || strings.Contains(lower, "gbp"):
		return r.entities[EntityGBLtd], nil
	case strings.Contains(lower, "sg") || strings.Contains(lower, "asia") || strings.Contains(lower, "sgd"):
		return r.entities[EntitySGPte], nil
	default:
		return r.entities[EntityUSCorp], nil
	}
}

// ListEntities returns all registered legal banking subsidiaries.
func (r *EntityRegistry) ListEntities() []*LegalEntity {
	r.mu.RLock()
	defer r.mu.RUnlock()

	list := make([]*LegalEntity, 0, len(r.entities))
	for _, ent := range r.entities {
		list = append(list, ent)
	}
	return list
}

// InterEntitySettler handles cross-entity transaction clearing with 4-legged omnibus accounts.
type InterEntitySettler struct {
	registry *EntityRegistry
}

// NewInterEntitySettler creates an inter-entity settlement coordinator.
func NewInterEntitySettler(reg *EntityRegistry) *InterEntitySettler {
	if reg == nil {
		reg = NewEntityRegistry()
	}
	return &InterEntitySettler{registry: reg}
}

// CoordinateCrossEntitySettlement generates the 4 balanced accounting legs for cross-border/cross-entity settlement.
func (s *InterEntitySettler) CoordinateCrossEntitySettlement(txID, srcWallet, dstWallet string, amount float64, currency string) (*CrossEntityTransferResult, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}

	srcEntity, err := s.registry.GetWalletEntity(srcWallet)
	if err != nil {
		return nil, fmt.Errorf("source entity resolution failed: %w", err)
	}
	dstEntity, err := s.registry.GetWalletEntity(dstWallet)
	if err != nil {
		return nil, fmt.Errorf("destination entity resolution failed: %w", err)
	}

	now := time.Now().UTC()

	// Case 1: Intra-entity transfer (both wallets belong to same legal subsidiary)
	if srcEntity.ID == dstEntity.ID {
		return &CrossEntityTransferResult{
			TransactionID:  txID,
			SourceEntityID: srcEntity.ID,
			TargetEntityID: dstEntity.ID,
			IsCrossEntity:  false,
			SourceWalletID: srcWallet,
			TargetWalletID: dstWallet,
			Amount:         amount,
			Currency:       currency,
			Status:         "SETTLED",
			SettledAt:      now,
		}, nil
	}

	// Case 2: Cross-Entity Omnibus Settlement (4-legged balancing entries)
	// Leg 1 (Source Entity): Debit Customer Account
	// Leg 2 (Source Entity): Credit Due-To Target Entity (Liability: 2100)
	// Leg 3 (Target Entity): Debit Due-From Source Entity (Asset: 1300)
	// Leg 4 (Target Entity): Credit Target Customer Account
	dueToAcct := fmt.Sprintf("2100-DUE-TO-%s", dstEntity.ID)
	dueFromAcct := fmt.Sprintf("1300-DUE-FROM-%s", srcEntity.ID)

	postings := []OmnibusPosting{
		{
			EntityID:      srcEntity.ID,
			AccountNumber: srcWallet,
			AccountName:   "Customer Depository Liability",
			Direction:     "DEBIT",
			Amount:        amount,
			Currency:      currency,
			Description:   fmt.Sprintf("Cross-entity transfer to %s/%s", dstEntity.ID, dstWallet),
			Timestamp:     now,
		},
		{
			EntityID:      srcEntity.ID,
			AccountNumber: dueToAcct,
			AccountName:   fmt.Sprintf("Inter-Entity Clearing Due To %s", dstEntity.ID),
			Direction:     "CREDIT",
			Amount:        amount,
			Currency:      currency,
			Description:   fmt.Sprintf("Omnibus payable to %s for tx %s", dstEntity.ID, txID),
			Timestamp:     now,
		},
		{
			EntityID:      dstEntity.ID,
			AccountNumber: dueFromAcct,
			AccountName:   fmt.Sprintf("Inter-Entity Clearing Due From %s", srcEntity.ID),
			Direction:     "DEBIT",
			Amount:        amount,
			Currency:      currency,
			Description:   fmt.Sprintf("Omnibus receivable from %s for tx %s", srcEntity.ID, txID),
			Timestamp:     now,
		},
		{
			EntityID:      dstEntity.ID,
			AccountNumber: dstWallet,
			AccountName:   "Customer Depository Liability",
			Direction:     "CREDIT",
			Amount:        amount,
			Currency:      currency,
			Description:   fmt.Sprintf("Cross-entity credit received from %s/%s", srcEntity.ID, srcWallet),
			Timestamp:     now,
		},
	}

	return &CrossEntityTransferResult{
		TransactionID:   txID,
		SourceEntityID:  srcEntity.ID,
		TargetEntityID:  dstEntity.ID,
		IsCrossEntity:   true,
		SourceWalletID:  srcWallet,
		TargetWalletID:  dstWallet,
		Amount:          amount,
		Currency:        currency,
		OmnibusPostings: postings,
		Status:          "SETTLED",
		SettledAt:       now,
	}, nil
}
