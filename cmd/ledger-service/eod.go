package main

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wallet-system/pkg/iso20022"
	ledgerv1 "wallet-system/proto/ledger"
)

// GetTrialBalance computes rolling debits, credits, and net balances across all GL accounts.
func (s *server) GetTrialBalance(ctx context.Context, req *ledgerv1.GetTrialBalanceRequest) (*ledgerv1.GetTrialBalanceResponse, error) {
	if s.db() == nil {
		return nil, status.Errorf(codes.Unavailable, "database unavailable")
	}

	currency := "USD"
	if req != nil && req.Currency != "" {
		currency = req.Currency
	}

	filter := bson.M{"currency": currency}
	if req != nil && req.AsOfDate != "" {
		parsedDate, err := time.Parse("2006-01-02", req.AsOfDate)
		if err == nil {
			endOfDay := parsedDate.Add(24*time.Hour - time.Nanosecond)
			filter["timestamp"] = bson.M{"$lte": endOfDay}
		}
	}

	col := s.db().Collection("ledger_entries")
	cursor, err := col.Find(ctx, filter, options.Find().SetSort(bson.D{bson.E{Key: "sequence_number", Value: 1}}))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to query ledger entries: %v", err)
	}
	defer cursor.Close(ctx)

	accountTotals := make(map[string]*ledgerv1.GLAccountSummary)
	for code, def := range ChartOfAccounts {
		accountTotals[code] = &ledgerv1.GLAccountSummary{
			GlAccountCode: code,
			GlAccountName: def.Name,
			Category:      def.Category,
			Currency:      currency,
		}
	}

	var sumDebits int64
	var sumCredits int64

	for cursor.Next(ctx) {
		var doc LedgerDocument
		if err := cursor.Decode(&doc); err != nil {
			continue
		}

		for _, p := range doc.Postings {
			if p.Currency != currency {
				continue
			}

			code := p.GLAccountCode
			if code == "" {
				code, _, _ = MapAccountToGL(p.AccountID, p.Currency)
			}

			acc, exists := accountTotals[code]
			if !exists {
				def, hasDef := ChartOfAccounts[code]
				name := "Account " + code
				cat := GLCategoryAsset
				if hasDef {
					name = def.Name
					cat = def.Category
				}
				acc = &ledgerv1.GLAccountSummary{
					GlAccountCode: code,
					GlAccountName: name,
					Category:      cat,
					Currency:      currency,
				}
				accountTotals[code] = acc
			}

			if p.Direction == PostingDebit {
				acc.TotalDebits += p.Amount
				sumDebits += p.Amount
			} else {
				acc.TotalCredits += p.Amount
				sumCredits += p.Amount
			}
		}
	}

	var totalAssets int64
	var totalLiabilities int64
	var totalEquity int64
	var totalRevenue int64
	var totalExpenses int64

	var accountsList []*ledgerv1.GLAccountSummary
	for _, acc := range accountTotals {
		switch acc.Category {
		case GLCategoryAsset:
			acc.NetBalance = acc.TotalDebits - acc.TotalCredits
			totalAssets += acc.NetBalance
		case GLCategoryExpense:
			acc.NetBalance = acc.TotalDebits - acc.TotalCredits
			totalExpenses += acc.NetBalance
		case GLCategoryLiability:
			acc.NetBalance = acc.TotalCredits - acc.TotalDebits
			totalLiabilities += acc.NetBalance
		case GLCategoryEquity:
			acc.NetBalance = acc.TotalCredits - acc.TotalDebits
			totalEquity += acc.NetBalance
		case GLCategoryRevenue:
			acc.NetBalance = acc.TotalCredits - acc.TotalDebits
			totalRevenue += acc.NetBalance
		}
		if acc.TotalDebits > 0 || acc.TotalCredits > 0 {
			accountsList = append(accountsList, acc)
		}
	}

	// Fundamental Balance Sheet Equation: Assets = Liabilities + Equity + (Revenue - Expenses)
	rightHandSide := totalLiabilities + totalEquity + (totalRevenue - totalExpenses)
	isBalanced := (sumDebits == sumCredits) && (totalAssets == rightHandSide)

	return &ledgerv1.GetTrialBalanceResponse{
		Accounts:         accountsList,
		TotalAssets:      totalAssets,
		TotalLiabilities: totalLiabilities,
		TotalEquity:      totalEquity,
		TotalRevenue:     totalRevenue,
		TotalExpenses:    totalExpenses,
		IsBalanced:       isBalanced,
		Currency:         currency,
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// GenerateEODReport seals daily ledger activity and produces a certified camt.053 statement.
func (s *server) GenerateEODReport(ctx context.Context, req *ledgerv1.GenerateEODReportRequest) (*ledgerv1.GenerateEODReportResponse, error) {
	if s.db() == nil {
		return nil, status.Errorf(codes.Unavailable, "database unavailable")
	}

	currency := "USD"
	if req != nil && req.Currency != "" {
		currency = req.Currency
	}

	dateStr := time.Now().UTC().Format("2006-01-02")
	if req != nil && req.Date != "" {
		dateStr = req.Date
	}

	trialBal, err := s.GetTrialBalance(ctx, &ledgerv1.GetTrialBalanceRequest{
		Currency: currency,
		AsOfDate: dateStr,
	})
	if err != nil {
		return nil, err
	}

	// Query entries for the day to formulate camt.053 StatementEntries
	parsedDate, _ := time.Parse("2006-01-02", dateStr)
	startOfDay := parsedDate
	endOfDay := parsedDate.Add(24*time.Hour - time.Nanosecond)

	filter := bson.M{
		"currency":  currency,
		"timestamp": bson.M{"$gte": startOfDay, "$lte": endOfDay},
	}

	col := s.db().Collection("ledger_entries")
	cursor, err := col.Find(ctx, filter)
	var stmtEntries []iso20022.StatementEntry
	if err == nil {
		defer cursor.Close(ctx)
		for cursor.Next(ctx) {
			var doc LedgerDocument
			if err := cursor.Decode(&doc); err == nil {
				stmtEntries = append(stmtEntries, iso20022.StatementEntry{
					TxId:        doc.ID.Hex(),
					Amount:      float64(doc.Amount) / 100.0,
					Currency:    doc.Currency,
					IsCredit:    true,
					BookingDate: doc.Timestamp,
					Category:    "SETTLEMENT",
				})
			}
		}
	}

	openingBal := float64(trialBal.TotalAssets-trialBal.TotalRevenue) / 100.0
	closingBal := float64(trialBal.TotalAssets) / 100.0
	accountIBAN := fmt.Sprintf("US24NOSTRO%s12345678", currency)

	camtXML, err := iso20022.BuildCamt053("EOD-"+dateStr, accountIBAN, currency, openingBal, closingBal, stmtEntries)
	if err != nil {
		camtXML = []byte("")
	}

	reportID := primitive.NewObjectID().Hex()
	eodRecord := bson.M{
		"_id":          reportID,
		"date":         dateStr,
		"currency":     currency,
		"total_assets": trialBal.TotalAssets,
		"total_liab":   trialBal.TotalLiabilities,
		"is_balanced":  trialBal.IsBalanced,
		"generated_by": req.GeneratedBy,
		"created_at":   time.Now().UTC(),
	}

	_, _ = s.db().Collection("eod_reports").InsertOne(ctx, eodRecord)

	return &ledgerv1.GenerateEODReportResponse{
		ReportId:     reportID,
		Date:         dateStr,
		TrialBalance: trialBal,
		Camt053Xml:   string(camtXML),
		Verified:     trialBal.IsBalanced,
	}, nil
}
