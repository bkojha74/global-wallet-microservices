package treasury

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Supported sweep strategy types
const (
	SweepTypeZBA           = "ZBA"            // Zero-Balance Account: Sweeps 100% of available funds to master
	SweepTypeTargetBalance = "TARGET_BALANCE" // Retains specified target balance; sweeps excess or replenishes deficit
)

var (
	ErrRuleNotFound   = errors.New("sweep rule not found")
	ErrInvalidAccount = errors.New("source and master accounts must be specified and non-identical")
	ErrInvalidTarget  = errors.New("target balance cannot be negative")
)

// SweepRule defines automated liquidity concentration parameters.
type SweepRule struct {
	ID                string    `json:"id"`
	SourceAccount     string    `json:"source_account"`
	MasterAccount     string    `json:"master_account"`
	Type              string    `json:"type"` // ZBA or TARGET_BALANCE
	TargetBalance     float64   `json:"target_balance"`
	MinTransferAmount float64   `json:"min_transfer_amount"`
	Currency          string    `json:"currency"`
	IsActive          bool      `json:"is_active"`
	CreatedAt         time.Time `json:"created_at"`
}

// SweepExecution records an executed automated liquidity sweep.
type SweepExecution struct {
	ExecutionID       string    `json:"execution_id"`
	RuleID            string    `json:"rule_id"`
	SourceAccount     string    `json:"source_account"`
	MasterAccount     string    `json:"master_account"`
	InitialBalance    float64   `json:"initial_balance"`
	TransferredAmount float64   `json:"transferred_amount"`
	ResultingBalance  float64   `json:"resulting_balance"`
	Direction         string    `json:"direction"` // SWEEP_TO_MASTER, REPLENISH_FROM_MASTER, NO_OP
	Currency          string    `json:"currency"`
	ExecutedAt        time.Time `json:"executed_at"`
}

// SafeguardingReport describes regulatory deposit trust compliance (FCA / BaFin / MAS).
type SafeguardingReport struct {
	TotalCustomerDeposits float64   `json:"total_customer_deposits"`
	SafeguardedReserves   float64   `json:"safeguarded_reserves"`
	CoverageRatio         float64   `json:"coverage_ratio"` // e.g. 1.02 = 102%
	SurplusDeficit        float64   `json:"surplus_deficit"`
	Status                string    `json:"status"` // COMPLIANT, DEFICIT_ACTION_REQUIRED
	GeneratedAt           time.Time `json:"generated_at"`
}

// TreasuryEngine manages liquidity concentration rules and regulatory safeguarding reserves.
type TreasuryEngine struct {
	mu    sync.RWMutex
	rules map[string]SweepRule
}

// NewTreasuryEngine initializes a thread-safe TreasuryEngine.
func NewTreasuryEngine() *TreasuryEngine {
	return &TreasuryEngine{
		rules: make(map[string]SweepRule),
	}
}

// RegisterRule configures a new automated liquidity sweeping rule.
func (e *TreasuryEngine) RegisterRule(rule SweepRule) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	rule.ID = strings.TrimSpace(rule.ID)
	rule.SourceAccount = strings.TrimSpace(rule.SourceAccount)
	rule.MasterAccount = strings.TrimSpace(rule.MasterAccount)

	if rule.ID == "" {
		rule.ID = fmt.Sprintf("sweep-rule-%s-to-%s", rule.SourceAccount, rule.MasterAccount)
	}
	if rule.SourceAccount == "" || rule.MasterAccount == "" || rule.SourceAccount == rule.MasterAccount {
		return ErrInvalidAccount
	}
	if rule.TargetBalance < 0 {
		return ErrInvalidTarget
	}
	if rule.Type == "" {
		rule.Type = SweepTypeZBA
	}
	if rule.MinTransferAmount <= 0 {
		rule.MinTransferAmount = 1.00 // minimum $1.00 unit
	}
	if rule.Currency == "" {
		rule.Currency = "USD"
	}

	rule.IsActive = true
	rule.CreatedAt = time.Now().UTC()
	e.rules[rule.ID] = rule
	return nil
}

// ListRules returns all configured liquidity sweeping rules.
func (e *TreasuryEngine) ListRules() []SweepRule {
	e.mu.RLock()
	defer e.mu.RUnlock()

	res := make([]SweepRule, 0, len(e.rules))
	for _, r := range e.rules {
		res = append(res, r)
	}
	return res
}

// ExecuteSweeps evaluates active sweeping rules against current operational account balances.
func (e *TreasuryEngine) ExecuteSweeps(balances map[string]float64) ([]SweepExecution, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var executions []SweepExecution
	now := time.Now().UTC()

	for _, rule := range e.rules {
		if !rule.IsActive {
			continue
		}

		currBal, exists := balances[rule.SourceAccount]
		if !exists {
			continue
		}

		var transferAmt float64
		var direction string
		var resultingBal float64

		if rule.Type == SweepTypeZBA {
			// Zero Balance: Sweep entire positive balance to master
			if currBal > 0 && currBal >= rule.MinTransferAmount {
				transferAmt = currBal
				direction = "SWEEP_TO_MASTER"
				resultingBal = 0
			} else {
				direction = "NO_OP"
				resultingBal = currBal
			}
		} else if rule.Type == SweepTypeTargetBalance {
			diff := currBal - rule.TargetBalance
			if diff > 0 && diff >= rule.MinTransferAmount {
				// Excess cash over target balance -> sweep to master
				transferAmt = diff
				direction = "SWEEP_TO_MASTER"
				resultingBal = rule.TargetBalance
			} else if diff < 0 && (-diff) >= rule.MinTransferAmount {
				// Deficit below target balance -> replenish from master
				transferAmt = -diff
				direction = "REPLENISH_FROM_MASTER"
				resultingBal = rule.TargetBalance
			} else {
				direction = "NO_OP"
				resultingBal = currBal
			}
		}

		exec := SweepExecution{
			ExecutionID:       fmt.Sprintf("exec-%s-%d", rule.ID, now.UnixNano()),
			RuleID:            rule.ID,
			SourceAccount:     rule.SourceAccount,
			MasterAccount:     rule.MasterAccount,
			InitialBalance:    currBal,
			TransferredAmount: transferAmt,
			ResultingBalance:  resultingBal,
			Direction:         direction,
			Currency:          rule.Currency,
			ExecutedAt:        now,
		}
		executions = append(executions, exec)
	}

	return executions, nil
}

// CalculateSafeguarding validates that 100% of customer deposits are safeguarded in qualified trustee accounts.
func (e *TreasuryEngine) CalculateSafeguarding(customerDeposits, trusteeReserves float64) *SafeguardingReport {
	now := time.Now().UTC()
	var ratio float64
	if customerDeposits > 0 {
		ratio = trusteeReserves / customerDeposits
	} else {
		ratio = 1.0
	}

	surplus := trusteeReserves - customerDeposits
	status := "COMPLIANT"
	if surplus < 0 {
		status = "DEFICIT_ACTION_REQUIRED"
	}

	return &SafeguardingReport{
		TotalCustomerDeposits: customerDeposits,
		SafeguardedReserves:   trusteeReserves,
		CoverageRatio:         ratio,
		SurplusDeficit:        surplus,
		Status:                status,
		GeneratedAt:           now,
	}
}
