package banking

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrVirtualIBANNotFound = errors.New("virtual IBAN not found")
	ErrVirtualIBANExists   = errors.New("virtual IBAN already allocated")
	ErrUnsupportedCountry  = errors.New("unsupported country code for virtual IBAN issuance (supported: GB, DE, FR)")
	ErrVirtualIBANRevoked  = errors.New("virtual IBAN has been revoked")
)

// VirtualIBAN represents a BaaS issued international bank account number tied to a wallet.
type VirtualIBAN struct {
	IBAN      string    `json:"iban" bson:"iban"`
	WalletID  string    `json:"wallet_id" bson:"wallet_id"`
	Currency  string    `json:"currency" bson:"currency"`
	Country   string    `json:"country" bson:"country"`
	Status    string    `json:"status" bson:"status"` // "ACTIVE", "SUSPENDED", "REVOKED"
	Label     string    `json:"label,omitempty" bson:"label,omitempty"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
}

// GenerateVirtualIBAN constructs a valid ISO 13616 vIBAN for the specified jurisdiction.
func GenerateVirtualIBAN(countryCode, currency, walletID, label string, seed int64) (*VirtualIBAN, error) {
	country := strings.ToUpper(strings.TrimSpace(countryCode))
	ccy := strings.ToUpper(strings.TrimSpace(currency))

	var bban string
	switch country {
	case "GB":
		// UK format: 4 alpha bank code + 6 digit sort code + 8 digit account number = 18 char BBAN
		accountSeq := seed % 100000000
		bban = fmt.Sprintf("BARK200000%08d", accountSeq)
	case "DE":
		// German format: 8 digit BLZ + 10 digit account number = 18 char BBAN
		accountSeq := seed % 10000000000
		bban = fmt.Sprintf("37040044%010d", accountSeq)
	case "FR":
		// French format: 5 digit bank + 5 digit branch + 11 digit account + 2 digit national check = 23 char BBAN
		accountSeq := seed % 100000000000
		bban = fmt.Sprintf("3000200550%011d01", accountSeq)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCountry, country)
	}

	fullIBAN, err := GenerateIBAN(country, bban)
	if err != nil {
		return nil, fmt.Errorf("failed to generate virtual IBAN: %w", err)
	}

	return &VirtualIBAN{
		IBAN:      fullIBAN,
		WalletID:  walletID,
		Currency:  ccy,
		Country:   country,
		Status:    "ACTIVE",
		Label:     label,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// VirtualIBANStore defines persistence operations for BaaS virtual account routing.
type VirtualIBANStore interface {
	IssueVirtualIBAN(ctx context.Context, v *VirtualIBAN) error
	GetVirtualIBAN(ctx context.Context, iban string) (*VirtualIBAN, error)
	ListVirtualIBANs(ctx context.Context, walletID string) ([]*VirtualIBAN, error)
	RevokeVirtualIBAN(ctx context.Context, iban string) error
}

// MemoryVirtualIBANStore provides a thread-safe in-memory registry for vIBANs.
type MemoryVirtualIBANStore struct {
	mu     sync.RWMutex
	byIBAN map[string]*VirtualIBAN
}

// NewMemoryVirtualIBANStore creates an empty memory store for vIBANs.
func NewMemoryVirtualIBANStore() *MemoryVirtualIBANStore {
	return &MemoryVirtualIBANStore{
		byIBAN: make(map[string]*VirtualIBAN),
	}
}

// IssueVirtualIBAN stores a new virtual IBAN in memory.
func (m *MemoryVirtualIBANStore) IssueVirtualIBAN(_ context.Context, v *VirtualIBAN) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cleaned := CleanIBAN(v.IBAN)
	if _, exists := m.byIBAN[cleaned]; exists {
		return ErrVirtualIBANExists
	}
	m.byIBAN[cleaned] = v
	return nil
}

// GetVirtualIBAN retrieves an active virtual IBAN.
func (m *MemoryVirtualIBANStore) GetVirtualIBAN(_ context.Context, iban string) (*VirtualIBAN, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cleaned := CleanIBAN(iban)
	v, exists := m.byIBAN[cleaned]
	if !exists {
		return nil, ErrVirtualIBANNotFound
	}
	if v.Status == "REVOKED" {
		return nil, ErrVirtualIBANRevoked
	}
	return v, nil
}

// ListVirtualIBANs lists all virtual IBANs for a given wallet ID.
func (m *MemoryVirtualIBANStore) ListVirtualIBANs(_ context.Context, walletID string) ([]*VirtualIBAN, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var results []*VirtualIBAN
	for _, v := range m.byIBAN {
		if v.WalletID == walletID && v.Status != "REVOKED" {
			results = append(results, v)
		}
	}
	return results, nil
}

// RevokeVirtualIBAN marks a virtual IBAN as revoked.
func (m *MemoryVirtualIBANStore) RevokeVirtualIBAN(_ context.Context, iban string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cleaned := CleanIBAN(iban)
	v, exists := m.byIBAN[cleaned]
	if !exists {
		return ErrVirtualIBANNotFound
	}
	v.Status = "REVOKED"
	return nil
}
