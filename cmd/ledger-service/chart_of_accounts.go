package main

import (
	"strings"
)

// Standard GAAP / IFRS Account Categories
const (
	GLCategoryAsset     = "ASSET"     // Debit normal balance
	GLCategoryLiability = "LIABILITY" // Credit normal balance
	GLCategoryEquity    = "EQUITY"    // Credit normal balance
	GLCategoryRevenue   = "REVENUE"   // Credit normal balance
	GLCategoryExpense   = "EXPENSE"   // Debit normal balance
)

// GLAccountDefinition represents a certified General Ledger Account per GAAP/IFRS.
type GLAccountDefinition struct {
	Code     string
	Name     string
	Category string // ASSET, LIABILITY, EQUITY, REVENUE, EXPENSE
	Normal   PostingDirection
}

var ChartOfAccounts = map[string]GLAccountDefinition{
	// 1000s: ASSETS
	"1100": {Code: "1100", Name: "Central Bank Settlement Reserves", Category: GLCategoryAsset, Normal: PostingDebit},
	"1210": {Code: "1210", Name: "Commercial Bank Nostro Operating USD", Category: GLCategoryAsset, Normal: PostingDebit},
	"1220": {Code: "1220", Name: "Commercial Bank Nostro Operating EUR", Category: GLCategoryAsset, Normal: PostingDebit},
	"1230": {Code: "1230", Name: "Commercial Bank Nostro Operating GBP", Category: GLCategoryAsset, Normal: PostingDebit},
	"1310": {Code: "1310", Name: "Clearing Scheme Receivables - ACH", Category: GLCategoryAsset, Normal: PostingDebit},
	"1320": {Code: "1320", Name: "Clearing Scheme Receivables - SEPA", Category: GLCategoryAsset, Normal: PostingDebit},
	"1330": {Code: "1330", Name: "Clearing Scheme Receivables - Card Networks", Category: GLCategoryAsset, Normal: PostingDebit},
	"1340": {Code: "1340", Name: "Clearing Scheme Receivables - FedNow / Instant", Category: GLCategoryAsset, Normal: PostingDebit},
	"1400": {Code: "1400", Name: "FX Liquidity Pool Buffer", Category: GLCategoryAsset, Normal: PostingDebit},

	// 2000s: LIABILITIES
	"2110": {Code: "2110", Name: "Customer Demand Deposits - Retail Wallets", Category: GLCategoryLiability, Normal: PostingCredit},
	"2120": {Code: "2120", Name: "Customer Demand Deposits - Corporate Wallets", Category: GLCategoryLiability, Normal: PostingCredit},
	"2200": {Code: "2200", Name: "Safeguarded Client Funds Statutory Trust", Category: GLCategoryLiability, Normal: PostingCredit},
	"2300": {Code: "2300", Name: "Inbound Clearing Suspense & Unallocated Funds", Category: GLCategoryLiability, Normal: PostingCredit},

	// 3000s: EQUITY
	"3100": {Code: "3100", Name: "Paid-In Tier-1 Capital", Category: GLCategoryEquity, Normal: PostingCredit},
	"3200": {Code: "3200", Name: "Retained Earnings / Capital Buffer", Category: GLCategoryEquity, Normal: PostingCredit},

	// 4000s: REVENUE
	"4100": {Code: "4100", Name: "Transaction Processing Fee Income", Category: GLCategoryRevenue, Normal: PostingCredit},
	"4200": {Code: "4200", Name: "FX Conversion Spread Margin", Category: GLCategoryRevenue, Normal: PostingCredit},

	// 5000s: EXPENSES
	"5100": {Code: "5100", Name: "Interchange & Network Assessment Expense", Category: GLCategoryExpense, Normal: PostingDebit},
	"5200": {Code: "5200", Name: "Core Clearing & Settlement Charges", Category: GLCategoryExpense, Normal: PostingDebit},
}

// MapAccountToGL maps any internal account ID or external rail identifier to its canonical GL account.
func MapAccountToGL(accountID, currency string) (code, name, category string) {
	upper := strings.ToUpper(strings.TrimSpace(accountID))

	switch {
	case strings.Contains(upper, "FEDNOW") || strings.Contains(upper, "INSTANT"):
		def := ChartOfAccounts["1340"]
		return def.Code, def.Name, def.Category
	case strings.Contains(upper, "ACH"):
		def := ChartOfAccounts["1310"]
		return def.Code, def.Name, def.Category
	case strings.Contains(upper, "SEPA"):
		def := ChartOfAccounts["1320"]
		return def.Code, def.Name, def.Category
	case strings.Contains(upper, "CARD"):
		def := ChartOfAccounts["1330"]
		return def.Code, def.Name, def.Category
	case strings.HasPrefix(upper, "FX_LIQUIDITY_POOL"):
		def := ChartOfAccounts["1400"]
		return def.Code, def.Name, def.Category
	case strings.HasPrefix(upper, "NOSTRO_") || strings.HasPrefix(upper, "BANK_"):
		switch strings.ToUpper(currency) {
		case "EUR":
			def := ChartOfAccounts["1220"]
			return def.Code, def.Name, def.Category
		case "GBP":
			def := ChartOfAccounts["1230"]
			return def.Code, def.Name, def.Category
		default:
			def := ChartOfAccounts["1210"]
			return def.Code, def.Name, def.Category
		}
	case strings.Contains(upper, "CORP") || strings.Contains(upper, "MERCHANT"):
		def := ChartOfAccounts["2120"]
		return def.Code, def.Name, def.Category
	default:
		// Default to standard customer retail demand deposit liability (2110)
		def := ChartOfAccounts["2110"]
		return def.Code, def.Name, def.Category
	}
}
