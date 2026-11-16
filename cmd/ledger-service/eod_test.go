package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"

	ledgerv1 "wallet-system/proto/ledger"
)

func TestEODAndTrialBalance(t *testing.T) {
	opts := mtest.NewOptions().ClientType(mtest.Mock)
	mt := mtest.New(t, opts)

	mt.Run("GetTrialBalanceBalanced", func(mt *mtest.T) {
		ts := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		id1 := primitive.NewObjectID()
		postings1, _ := CreateDepositPostings("alice", "ACH", 10000, "USD")

		doc1 := bson.D{
			bson.E{Key: "_id", Value: id1},
			bson.E{Key: "sequence_number", Value: int64(1)},
			bson.E{Key: "idempotency_key", Value: "dep-1"},
			bson.E{Key: "source_wallet_id", Value: "EXTERNAL_CLEARING_ACH_USD"},
			bson.E{Key: "destination_wallet_id", Value: "alice"},
			bson.E{Key: "amount", Value: int64(10000)},
			bson.E{Key: "currency", Value: "USD"},
			bson.E{Key: "timestamp", Value: ts},
			bson.E{Key: "postings", Value: bson.A{
				bson.D{
					bson.E{Key: "account_id", Value: "EXTERNAL_CLEARING_ACH_USD"},
					bson.E{Key: "account_type", Value: "SETTLEMENT_ASSET"},
					bson.E{Key: "direction", Value: "DEBIT"},
					bson.E{Key: "amount", Value: int64(10000)},
					bson.E{Key: "currency", Value: "USD"},
					bson.E{Key: "gl_account_code", Value: "1310"},
					bson.E{Key: "gl_account_category", Value: "ASSET"},
				},
				bson.D{
					bson.E{Key: "account_id", Value: "alice"},
					bson.E{Key: "account_type", Value: "CUSTOMER_LIABILITY"},
					bson.E{Key: "direction", Value: "CREDIT"},
					bson.E{Key: "amount", Value: int64(10000)},
					bson.E{Key: "currency", Value: "USD"},
					bson.E{Key: "gl_account_code", Value: "2110"},
					bson.E{Key: "gl_account_category", Value: "LIABILITY"},
				},
			}},
		}

		mt.AddMockResponses(mtest.CreateCursorResponse(1, "banking_db.ledger_entries", mtest.FirstBatch, doc1))

		s := &server{
			mongoClient: mt.Client,
			dbName:      "banking_db",
			region:      "us-test",
		}

		resp, err := s.GetTrialBalance(context.Background(), &ledgerv1.GetTrialBalanceRequest{
			Currency: "USD",
			AsOfDate: "2026-11-13",
		})
		if err != nil {
			mt.Fatalf("unexpected error: %v", err)
		}

		if !resp.IsBalanced {
			mt.Fatalf("expected trial balance to be balanced, got unbalanced")
		}
		if resp.TotalAssets != 10000 {
			mt.Errorf("expected total assets 10000, got %d", resp.TotalAssets)
		}
		if resp.TotalLiabilities != 10000 {
			mt.Errorf("expected total liabilities 10000, got %d", resp.TotalLiabilities)
		}
		_ = postings1
	})

	mt.Run("GenerateEODReport", func(mt *mtest.T) {
		ts := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		id1 := primitive.NewObjectID()

		doc1 := bson.D{
			bson.E{Key: "_id", Value: id1},
			bson.E{Key: "sequence_number", Value: int64(1)},
			bson.E{Key: "amount", Value: int64(5000)},
			bson.E{Key: "currency", Value: "USD"},
			bson.E{Key: "timestamp", Value: ts},
			bson.E{Key: "postings", Value: bson.A{
				bson.D{
					bson.E{Key: "account_id", Value: "EXTERNAL_CLEARING_ACH_USD"},
					bson.E{Key: "direction", Value: "DEBIT"},
					bson.E{Key: "amount", Value: int64(5000)},
					bson.E{Key: "currency", Value: "USD"},
					bson.E{Key: "gl_account_code", Value: "1310"},
					bson.E{Key: "gl_account_category", Value: "ASSET"},
				},
				bson.D{
					bson.E{Key: "account_id", Value: "alice"},
					bson.E{Key: "direction", Value: "CREDIT"},
					bson.E{Key: "amount", Value: int64(5000)},
					bson.E{Key: "currency", Value: "USD"},
					bson.E{Key: "gl_account_code", Value: "2110"},
					bson.E{Key: "gl_account_category", Value: "LIABILITY"},
				},
			}},
		}

		// Mock cursor for GetTrialBalance, then mock cursor for statement entries, then insert for eod_reports
		mt.AddMockResponses(
			mtest.CreateCursorResponse(1, "banking_db.ledger_entries", mtest.FirstBatch, doc1),
			mtest.CreateCursorResponse(1, "banking_db.ledger_entries", mtest.FirstBatch, doc1),
			mtest.CreateSuccessResponse(),
		)

		s := &server{
			mongoClient: mt.Client,
			dbName:      "banking_db",
			region:      "us-test",
		}

		resp, err := s.GenerateEODReport(context.Background(), &ledgerv1.GenerateEODReportRequest{
			Date:        "2026-11-13",
			Currency:    "USD",
			GeneratedBy: "chief_accountant",
		})
		if err != nil {
			mt.Fatalf("unexpected error: %v", err)
		}

		if resp.ReportId == "" {
			mt.Errorf("expected non-empty report ID")
		}
		if !resp.Verified {
			mt.Errorf("expected verified EOD report")
		}
		if !strings.Contains(resp.Camt053Xml, "BkToCstmrStmt") {
			mt.Errorf("expected Camt053Xml in response, got %s", resp.Camt053Xml)
		}
	})
}
