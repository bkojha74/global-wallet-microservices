package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
)

func TestValidatePostings(t *testing.T) {
	t.Run("ValidBalancedPostings", func(t *testing.T) {
		postings := []JournalPosting{
			{
				AccountID:   "alice",
				AccountType: AccountTypeCustomerLiability,
				Direction:   PostingDebit,
				Amount:      500,
				Currency:    "USD",
			},
			{
				AccountID:   "bob",
				AccountType: AccountTypeCustomerLiability,
				Direction:   PostingCredit,
				Amount:      500,
				Currency:    "USD",
			},
		}

		if err := ValidatePostings(postings); err != nil {
			t.Fatalf("expected valid postings to pass, got error: %v", err)
		}
	})

	t.Run("MultiLegBalancedWithFee", func(t *testing.T) {
		postings := []JournalPosting{
			{
				AccountID:   "alice",
				AccountType: AccountTypeCustomerLiability,
				Direction:   PostingDebit,
				Amount:      505,
				Currency:    "USD",
			},
			{
				AccountID:   "bob",
				AccountType: AccountTypeCustomerLiability,
				Direction:   PostingCredit,
				Amount:      500,
				Currency:    "USD",
			},
			{
				AccountID:   "system_fee_account",
				AccountType: AccountTypeFeeRevenue,
				Direction:   PostingCredit,
				Amount:      5,
				Currency:    "USD",
			},
		}

		if err := ValidatePostings(postings); err != nil {
			t.Fatalf("expected 3-leg balanced entry to pass, got: %v", err)
		}
	})

	t.Run("RejectUnbalancedDebitsAndCredits", func(t *testing.T) {
		postings := []JournalPosting{
			{
				AccountID:   "alice",
				AccountType: AccountTypeCustomerLiability,
				Direction:   PostingDebit,
				Amount:      500,
				Currency:    "USD",
			},
			{
				AccountID:   "bob",
				AccountType: AccountTypeCustomerLiability,
				Direction:   PostingCredit,
				Amount:      450, // 50 missing!
				Currency:    "USD",
			},
		}

		err := ValidatePostings(postings)
		if err == nil {
			t.Fatal("expected unbalanced postings to fail validation, but it passed")
		}
	})

	t.Run("RejectNonPositiveAmount", func(t *testing.T) {
		postings := []JournalPosting{
			{AccountID: "alice", Direction: PostingDebit, Amount: 0, Currency: "USD"},
			{AccountID: "bob", Direction: PostingCredit, Amount: 0, Currency: "USD"},
		}
		if err := ValidatePostings(postings); err == nil {
			t.Fatal("expected zero amount to fail validation")
		}
	})

	t.Run("RejectLessThanTwoPostings", func(t *testing.T) {
		postings := []JournalPosting{
			{AccountID: "alice", Direction: PostingDebit, Amount: 100, Currency: "USD"},
		}
		if err := ValidatePostings(postings); err == nil {
			t.Fatal("expected 1-leg posting to fail validation")
		}
	})
}

func TestCryptographicHashChaining(t *testing.T) {
	now := time.Now().UTC()
	postings, _ := CreateTransferPostings("alice", "bob", 1000, "USD")

	hash1 := ComputeEntryHash(GenesisHash, 1, "tx-1", "k1", now, postings)
	if hash1 == "" {
		t.Fatal("expected non-empty hash1")
	}

	// Deterministic
	hash1Dup := ComputeEntryHash(GenesisHash, 1, "tx-1", "k1", now, postings)
	if hash1 != hash1Dup {
		t.Fatalf("hash computation is non-deterministic: %s != %s", hash1, hash1Dup)
	}

	// Tamper in previous hash produces different hash
	hashTamperedPrev := ComputeEntryHash("fake_prev_hash", 1, "tx-1", "k1", now, postings)
	if hash1 == hashTamperedPrev {
		t.Fatal("expected different hash when previousHash is changed")
	}

	// Tamper in amount produces different hash
	tamperedPostings, _ := CreateTransferPostings("alice", "bob", 999, "USD")
	hashTamperedAmount := ComputeEntryHash(GenesisHash, 1, "tx-1", "k1", now, tamperedPostings)
	if hash1 == hashTamperedAmount {
		t.Fatal("expected different hash when postings are tampered")
	}

	// Sequential link to entry #2
	now2 := now.Add(time.Second)
	postings2, _ := CreateTransferPostings("bob", "charlie", 400, "USD")
	hash2 := ComputeEntryHash(hash1, 2, "tx-2", "k2", now2, postings2)
	if hash2 == "" || hash2 == hash1 {
		t.Fatalf("expected valid distinct hash2, got: %s", hash2)
	}
}

func TestAuditVerificationChainIntegrity(t *testing.T) {
	opts := mtest.NewOptions().ClientType(mtest.Mock)
	mt := mtest.New(t, opts)

	mt.Run("VerifyValidAuditChain", func(mt *mtest.T) {
		ts1 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
		ts2 := time.Date(2026, 9, 18, 10, 1, 0, 0, time.UTC)

		postings1, _ := CreateTransferPostings("alice", "bob", 100, "USD")
		id1 := primitive.NewObjectID()
		hash1 := ComputeEntryHash(GenesisHash, 1, id1.Hex(), "k-1", ts1, postings1)

		postings2, _ := CreateTransferPostings("bob", "charlie", 50, "USD")
		id2 := primitive.NewObjectID()
		hash2 := ComputeEntryHash(hash1, 2, id2.Hex(), "k-2", ts2, postings2)

		doc1 := bson.D{
			bson.E{Key: "_id", Value: id1},
			bson.E{Key: "sequence_number", Value: int64(1)},
			bson.E{Key: "idempotency_key", Value: "k-1"},
			bson.E{Key: "source_wallet_id", Value: "alice"},
			bson.E{Key: "destination_wallet_id", Value: "bob"},
			bson.E{Key: "amount", Value: int64(100)},
			bson.E{Key: "currency", Value: "USD"},
			bson.E{Key: "timestamp", Value: ts1},
			bson.E{Key: "postings", Value: bson.A{
				bson.D{
					bson.E{Key: "account_id", Value: "alice"},
					bson.E{Key: "account_type", Value: "CUSTOMER_LIABILITY"},
					bson.E{Key: "direction", Value: "DEBIT"},
					bson.E{Key: "amount", Value: int64(100)},
					bson.E{Key: "currency", Value: "USD"},
				},
				bson.D{
					bson.E{Key: "account_id", Value: "bob"},
					bson.E{Key: "account_type", Value: "CUSTOMER_LIABILITY"},
					bson.E{Key: "direction", Value: "CREDIT"},
					bson.E{Key: "amount", Value: int64(100)},
					bson.E{Key: "currency", Value: "USD"},
				},
			}},
			bson.E{Key: "previous_hash", Value: GenesisHash},
			bson.E{Key: "entry_hash", Value: hash1},
		}

		doc2 := bson.D{
			bson.E{Key: "_id", Value: id2},
			bson.E{Key: "sequence_number", Value: int64(2)},
			bson.E{Key: "idempotency_key", Value: "k-2"},
			bson.E{Key: "source_wallet_id", Value: "bob"},
			bson.E{Key: "destination_wallet_id", Value: "charlie"},
			bson.E{Key: "amount", Value: int64(50)},
			bson.E{Key: "currency", Value: "USD"},
			bson.E{Key: "timestamp", Value: ts2},
			bson.E{Key: "postings", Value: bson.A{
				bson.D{
					bson.E{Key: "account_id", Value: "bob"},
					bson.E{Key: "account_type", Value: "CUSTOMER_LIABILITY"},
					bson.E{Key: "direction", Value: "DEBIT"},
					bson.E{Key: "amount", Value: int64(50)},
					bson.E{Key: "currency", Value: "USD"},
				},
				bson.D{
					bson.E{Key: "account_id", Value: "charlie"},
					bson.E{Key: "account_type", Value: "CUSTOMER_LIABILITY"},
					bson.E{Key: "direction", Value: "CREDIT"},
					bson.E{Key: "amount", Value: int64(50)},
					bson.E{Key: "currency", Value: "USD"},
				},
			}},
			bson.E{Key: "previous_hash", Value: hash1},
			bson.E{Key: "entry_hash", Value: hash2},
		}

		mt.AddMockResponses(mtest.CreateCursorResponse(1, "banking_db.ledger_entries", mtest.FirstBatch, doc1, doc2))

		result, err := VerifyAuditChain(context.Background(), mt.Coll)
		if err != nil {
			mt.Fatalf("expected VerifyAuditChain to succeed, got: %v", err)
		}
		if result.Status != "VERIFIED" {
			mt.Fatalf("expected status VERIFIED, got: %s (error: %s)", result.Status, result.ErrorMessage)
		}
		if result.TotalEntries != 2 {
			mt.Fatalf("expected 2 total entries, got: %d", result.TotalEntries)
		}
		if !result.IsBalanced {
			mt.Fatalf("expected balanced trial balance, got unbalanced")
		}
		if result.TotalDebits["USD"] != 150 || result.TotalCredits["USD"] != 150 {
			mt.Fatalf("expected debits=150 and credits=150, got debits=%d, credits=%d",
				result.TotalDebits["USD"], result.TotalCredits["USD"])
		}
	})

	mt.Run("DetectTamperedEntryInChain", func(mt *mtest.T) {
		ts1 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
		postings1, _ := CreateTransferPostings("alice", "bob", 100, "USD")
		id1 := primitive.NewObjectID()
		_ = ComputeEntryHash(GenesisHash, 1, id1.Hex(), "k-1", ts1, postings1)

		// Tampered doc where entry_hash doesn't match content
		docTampered := bson.D{
			bson.E{Key: "_id", Value: id1},
			bson.E{Key: "sequence_number", Value: int64(1)},
			bson.E{Key: "idempotency_key", Value: "k-1"},
			bson.E{Key: "source_wallet_id", Value: "alice"},
			bson.E{Key: "destination_wallet_id", Value: "bob"},
			bson.E{Key: "amount", Value: int64(100)},
			bson.E{Key: "currency", Value: "USD"},
			bson.E{Key: "timestamp", Value: ts1},
			bson.E{Key: "postings", Value: bson.A{
				bson.D{
					bson.E{Key: "account_id", Value: "alice"},
					bson.E{Key: "account_type", Value: "CUSTOMER_LIABILITY"},
					bson.E{Key: "direction", Value: "DEBIT"},
					bson.E{Key: "amount", Value: int64(100)},
					bson.E{Key: "currency", Value: "USD"},
				},
				bson.D{
					bson.E{Key: "account_id", Value: "bob"},
					bson.E{Key: "account_type", Value: "CUSTOMER_LIABILITY"},
					bson.E{Key: "direction", Value: "CREDIT"},
					bson.E{Key: "amount", Value: int64(100)},
					bson.E{Key: "currency", Value: "USD"},
				},
			}},
			bson.E{Key: "previous_hash", Value: GenesisHash},
			bson.E{Key: "entry_hash", Value: "corrupted_hash_value_12345"},
		}

		mt.AddMockResponses(mtest.CreateCursorResponse(1, "banking_db.ledger_entries", mtest.FirstBatch, docTampered))

		result, err := VerifyAuditChain(context.Background(), mt.Coll)
		if err != nil {
			mt.Fatalf("unexpected error: %v", err)
		}
		if result.Status != "CORRUPTED" {
			mt.Fatalf("expected status CORRUPTED when hash tampered, got: %s", result.Status)
		}
		if result.TamperedEntrySeq != 1 {
			mt.Fatalf("expected tampered entry sequence 1, got: %d", result.TamperedEntrySeq)
		}
		fmt.Printf("[TEST] Correctly caught tamper: %s\n", result.ErrorMessage)
	})
}
