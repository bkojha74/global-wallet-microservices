package openbanking

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrConsentNotFound      = errors.New("open banking consent not found")
	ErrConsentExpired       = errors.New("open banking consent has expired (90-day PSD2 re-authentication required)")
	ErrConsentNotAuthorised = errors.New("open banking consent is not authorised")
	ErrConsentRevoked       = errors.New("open banking consent has been revoked")
	ErrScopeNotGranted      = errors.New("requested action not permitted by consent permissions")
	ErrWalletNotAuthorised  = errors.New("target wallet not authorized under this consent")
)

const (
	StatusAwaitingAuthorisation = "AWAITING_AUTHORISATION"
	StatusAuthorised            = "AUTHORISED"
	StatusRejected              = "REJECTED"
	StatusRevoked               = "REVOKED"
	StatusExpired               = "EXPIRED"

	PermReadAccountsDetail      = "ReadAccountsDetail"
	PermReadBalances            = "ReadBalances"
	PermReadTransactionsDetail  = "ReadTransactionsDetail"
	PermCreatePaymentInitiation = "CreatePaymentInitiation"
)

// Consent models a PSD2/UK Open Banking customer authorization grant.
type Consent struct {
	ConsentID    string     `json:"consent_id" bson:"consent_id"`
	ClientTPPId  string     `json:"client_tpp_id" bson:"client_tpp_id"` // Third Party Provider / Fintech
	UserID       string     `json:"user_id" bson:"user_id"`             // Consenting Customer Subject
	WalletIDs    []string   `json:"wallet_ids" bson:"wallet_ids"`
	Permissions  []string   `json:"permissions" bson:"permissions"`
	Status       string     `json:"status" bson:"status"`
	CreatedAt    time.Time  `json:"created_at" bson:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at" bson:"expires_at"`
	AuthorisedAt *time.Time `json:"authorised_at,omitempty" bson:"authorised_at,omitempty"`
}

// DomesticPaymentInitiation models a PISP account-to-account payment request.
type DomesticPaymentInitiation struct {
	InstructionID    string  `json:"instruction_id"`
	EndToEndID       string  `json:"end_to_end_id"`
	ConsentID        string  `json:"consent_id"`
	DebtorWalletID   string  `json:"debtor_wallet_id"`
	CreditorWalletID string  `json:"creditor_wallet_id"`
	CreditorIBAN     string  `json:"creditor_iban,omitempty"`
	Amount           float64 `json:"amount"`
	Currency         string  `json:"currency"`
	PaymentStatus    string  `json:"payment_status"` // "Pending", "AcceptedSettlementInProcess", "Rejected"
}

// ConsentStore provides persistence operations for Open Banking consent grants.
type ConsentStore interface {
	SaveConsent(ctx context.Context, c *Consent) error
	GetConsent(ctx context.Context, consentID string) (*Consent, error)
	RevokeConsent(ctx context.Context, consentID, userID string) error
	AuthoriseConsent(ctx context.Context, consentID, userID string) error
	ListConsents(ctx context.Context, userID string) ([]*Consent, error)
}

// MemoryConsentStore provides an in-memory implementation for unit testing and local gateway routing.
type MemoryConsentStore struct {
	mu       sync.RWMutex
	consents map[string]*Consent
}

// NewMemoryConsentStore creates an empty consent memory registry.
func NewMemoryConsentStore() *MemoryConsentStore {
	return &MemoryConsentStore{
		consents: make(map[string]*Consent),
	}
}

// SaveConsent stores or updates a consent.
func (m *MemoryConsentStore) SaveConsent(_ context.Context, c *Consent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consents[c.ConsentID] = c
	return nil
}

// GetConsent retrieves a consent and auto-evaluates expiration.
func (m *MemoryConsentStore) GetConsent(_ context.Context, consentID string) (*Consent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	c, exists := m.consents[consentID]
	if !exists {
		return nil, ErrConsentNotFound
	}

	if time.Now().UTC().After(c.ExpiresAt) && c.Status != StatusRevoked {
		c.Status = StatusExpired
	}

	return c, nil
}

// AuthoriseConsent records Strong Customer Authentication (SCA) approval.
func (m *MemoryConsentStore) AuthoriseConsent(_ context.Context, consentID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, exists := m.consents[consentID]
	if !exists {
		return ErrConsentNotFound
	}
	if c.UserID != "" && c.UserID != userID {
		return errors.New("unauthorized user attempting to approve consent")
	}

	now := time.Now().UTC()
	c.UserID = userID
	c.Status = StatusAuthorised
	c.AuthorisedAt = &now
	return nil
}

// RevokeConsent cancels an active consent.
func (m *MemoryConsentStore) RevokeConsent(_ context.Context, consentID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, exists := m.consents[consentID]
	if !exists {
		return ErrConsentNotFound
	}
	if userID != "" && c.UserID != "" && c.UserID != userID {
		return errors.New("unauthorized user attempting to revoke consent")
	}

	c.Status = StatusRevoked
	return nil
}

// ListConsents lists all active or past consents for a user.
func (m *MemoryConsentStore) ListConsents(_ context.Context, userID string) ([]*Consent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*Consent
	for _, c := range m.consents {
		if c.UserID == userID {
			result = append(result, c)
		}
	}
	return result, nil
}

// ValidateConsentAccess checks that a consent is AUTHORISED, not expired, covers the wallet and permission.
func ValidateConsentAccess(c *Consent, requiredPermission, targetWalletID string) error {
	if c == nil {
		return ErrConsentNotFound
	}
	if time.Now().UTC().After(c.ExpiresAt) || c.Status == StatusExpired {
		return ErrConsentExpired
	}
	if c.Status == StatusRevoked {
		return ErrConsentRevoked
	}
	if c.Status != StatusAuthorised {
		return ErrConsentNotAuthorised
	}

	// Check permission
	hasPerm := false
	for _, p := range c.Permissions {
		if p == requiredPermission {
			hasPerm = true
			break
		}
	}
	if !hasPerm {
		return fmt.Errorf("%w: missing required scope %s", ErrScopeNotGranted, requiredPermission)
	}

	// Check wallet if target specified
	if targetWalletID != "" && len(c.WalletIDs) > 0 {
		hasWallet := false
		for _, w := range c.WalletIDs {
			if w == targetWalletID {
				hasWallet = true
				break
			}
		}
		if !hasWallet {
			return fmt.Errorf("%w: wallet %s not included in consent", ErrWalletNotAuthorised, targetWalletID)
		}
	}

	return nil
}
