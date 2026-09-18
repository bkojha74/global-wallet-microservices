package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// GenesisHash is the foundational cryptographic anchor for the audit chain.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// PostingDirection represents standard accounting entry polarity (DEBIT or CREDIT).
type PostingDirection string

const (
	PostingDebit  PostingDirection = "DEBIT"
	PostingCredit PostingDirection = "CREDIT"
)

// AccountType categorizes double-entry balance sheet accounts per GAAP/IFRS.
type AccountType string

const (
	// AccountTypeCustomerLiability represents customer wallet deposits (bank liability).
	AccountTypeCustomerLiability AccountType = "CUSTOMER_LIABILITY"
	// AccountTypeSettlementAsset represents interbank/vault settlement balances (bank asset).
	AccountTypeSettlementAsset AccountType = "SETTLEMENT_ASSET"
	// AccountTypeFeeExpense represents transaction fee expenses.
	AccountTypeFeeExpense AccountType = "FEE_EXPENSE"
	// AccountTypeFeeRevenue represents transaction fee revenues.
	AccountTypeFeeRevenue AccountType = "FEE_REVENUE"
)

// JournalPosting represents a single atomic leg of a double-entry financial entry.
type JournalPosting struct {
	AccountID   string           `bson:"account_id" json:"account_id"`
	AccountType AccountType      `bson:"account_type" json:"account_type"`
	Direction   PostingDirection `bson:"direction" json:"direction"`
	Amount      int64            `bson:"amount" json:"amount"`
	Currency    string           `bson:"currency" json:"currency"`
}

// ValidatePostings enforces GAAP/IFRS financial equilibrium:
// 1. Must contain at least two postings.
// 2. Each posting amount must be strictly positive.
// 3. For each currency, sum(DEBIT) must exactly equal sum(CREDIT).
func ValidatePostings(postings []JournalPosting) error {
	if len(postings) < 2 {
		return fmt.Errorf("double-entry journal entry must have at least 2 postings (got %d)", len(postings))
	}

	debits := make(map[string]int64)
	credits := make(map[string]int64)

	for i, p := range postings {
		if p.AccountID == "" {
			return fmt.Errorf("posting [%d]: account_id is required", i)
		}
		if p.Amount <= 0 {
			return fmt.Errorf("posting [%d]: amount must be positive, got %d", i, p.Amount)
		}
		if p.Currency == "" {
			return fmt.Errorf("posting [%d]: currency is required", i)
		}

		switch p.Direction {
		case PostingDebit:
			debits[p.Currency] += p.Amount
		case PostingCredit:
			credits[p.Currency] += p.Amount
		default:
			return fmt.Errorf("posting [%d]: invalid direction %q (must be DEBIT or CREDIT)", i, p.Direction)
		}
	}

	for curr, debitSum := range debits {
		creditSum := credits[curr]
		if debitSum != creditSum {
			return fmt.Errorf("unbalanced double-entry entry for currency %s: debits=%d credits=%d (diff=%d)",
				curr, debitSum, creditSum, debitSum-creditSum)
		}
	}

	for curr, creditSum := range credits {
		if _, exists := debits[curr]; !exists {
			return fmt.Errorf("unbalanced double-entry entry for currency %s: debits=0 credits=%d", curr, creditSum)
		}
	}

	return nil
}

// CreateTransferPostings creates balanced GAAP double-entry postings for a transfer:
// - Debit Source Customer Liability (reduces liability to sender)
// - Credit Destination Customer Liability (increases liability to receiver)
func CreateTransferPostings(sourceWalletID, destWalletID string, amount int64, currency string) ([]JournalPosting, error) {
	postings := []JournalPosting{
		{
			AccountID:   sourceWalletID,
			AccountType: AccountTypeCustomerLiability,
			Direction:   PostingDebit,
			Amount:      amount,
			Currency:    currency,
		},
		{
			AccountID:   destWalletID,
			AccountType: AccountTypeCustomerLiability,
			Direction:   PostingCredit,
			Amount:      amount,
			Currency:    currency,
		},
	}

	if err := ValidatePostings(postings); err != nil {
		return nil, err
	}
	return postings, nil
}

// ComputeEntryHash produces an immutable SHA-256 cryptographic hash of the journal entry,
// mathematically sealing the sequence number, transaction IDs, timestamp, double-entry postings,
// and the previous entry hash.
func ComputeEntryHash(previousHash string, sequenceNumber int64, txID, idempKey string, ts time.Time, postings []JournalPosting) string {
	postingsBytes, _ := json.Marshal(postings)
	canonical := fmt.Sprintf("%s|%d|%s|%s|%d|%s",
		previousHash,
		sequenceNumber,
		txID,
		idempKey,
		ts.UTC().UnixNano(),
		string(postingsBytes),
	)

	hash := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(hash[:])
}

// AuditVerificationResult summarizes the outcome of an audit chain validation.
type AuditVerificationResult struct {
	Status           string           `json:"status"`
	TotalEntries     int64            `json:"total_entries"`
	TotalDebits      map[string]int64 `json:"total_debits"`
	TotalCredits     map[string]int64 `json:"total_credits"`
	IsBalanced       bool             `json:"is_balanced"`
	GenesisHash      string           `json:"genesis_hash"`
	LatestHash       string           `json:"latest_hash"`
	LastSequence     int64            `json:"last_sequence"`
	TamperedEntrySeq int64            `json:"tampered_entry_seq,omitempty"`
	ErrorMessage     string           `json:"error_message,omitempty"`
}

// VerifyAuditChain traverses the entire immutable ledger in ascending sequence order,
// recomputing cryptographic SHA-256 hashes and verifying trial balance equilibrium.
func VerifyAuditChain(ctx context.Context, col *mongo.Collection) (*AuditVerificationResult, error) {
	findOpts := options.Find().SetSort(bson.D{{Key: "sequence_number", Value: 1}})
	cursor, err := col.Find(ctx, bson.M{"sequence_number": bson.M{"$gt": 0}}, findOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to query ledger entries for verification: %w", err)
	}
	defer cursor.Close(ctx)

	result := &AuditVerificationResult{
		Status:       "VERIFIED",
		GenesisHash:  GenesisHash,
		TotalDebits:  make(map[string]int64),
		TotalCredits: make(map[string]int64),
		IsBalanced:   true,
	}

	expectedPrevHash := GenesisHash
	var expectedSeq int64 = 1

	for cursor.Next(ctx) {
		var doc LedgerDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("failed to decode ledger document: %w", err)
		}

		// Verify sequence continuity
		if doc.SequenceNumber != expectedSeq {
			result.Status = "CORRUPTED"
			result.TamperedEntrySeq = doc.SequenceNumber
			result.ErrorMessage = fmt.Sprintf("sequence gap detected: expected %d, got %d", expectedSeq, doc.SequenceNumber)
			return result, nil
		}

		// Verify previous hash chain linkage
		if doc.PreviousHash != expectedPrevHash {
			result.Status = "CORRUPTED"
			result.TamperedEntrySeq = doc.SequenceNumber
			result.ErrorMessage = fmt.Sprintf("hash chain broken at sequence %d: expected previous_hash %q, got %q",
				doc.SequenceNumber, expectedPrevHash, doc.PreviousHash)
			return result, nil
		}

		// Recompute hash and verify integrity
		computed := ComputeEntryHash(doc.PreviousHash, doc.SequenceNumber, doc.ID.Hex(), doc.IdempotencyKey, doc.Timestamp, doc.Postings)
		if doc.EntryHash != computed {
			result.Status = "CORRUPTED"
			result.TamperedEntrySeq = doc.SequenceNumber
			result.ErrorMessage = fmt.Sprintf("tamper detected at sequence %d: recorded hash %q does not match computed hash %q",
				doc.SequenceNumber, doc.EntryHash, computed)
			return result, nil
		}

		// Validate double-entry postings equilibrium on the record
		if err := ValidatePostings(doc.Postings); err != nil {
			result.Status = "CORRUPTED"
			result.TamperedEntrySeq = doc.SequenceNumber
			result.ErrorMessage = fmt.Sprintf("unbalanced posting at sequence %d: %v", doc.SequenceNumber, err)
			return result, nil
		}

		// Accumulate trial balance
		for _, p := range doc.Postings {
			if p.Direction == PostingDebit {
				result.TotalDebits[p.Currency] += p.Amount
			} else if p.Direction == PostingCredit {
				result.TotalCredits[p.Currency] += p.Amount
			}
		}

		expectedPrevHash = doc.EntryHash
		expectedSeq++
		result.TotalEntries++
		result.LatestHash = doc.EntryHash
		result.LastSequence = doc.SequenceNumber
	}

	// Verify ledger-wide trial balance equilibrium
	for curr, debitTotal := range result.TotalDebits {
		creditTotal := result.TotalCredits[curr]
		if debitTotal != creditTotal {
			result.IsBalanced = false
			result.Status = "TRIAL_BALANCE_UNBALANCED"
			result.ErrorMessage = fmt.Sprintf("ledger-wide trial balance discrepancy in %s: debits=%d, credits=%d",
				curr, debitTotal, creditTotal)
			return result, nil
		}
	}

	return result, nil
}
