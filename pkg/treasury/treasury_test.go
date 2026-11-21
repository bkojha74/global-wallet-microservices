package treasury

import (
	"testing"
)

func TestTreasuryEngine_ZBASweep(t *testing.T) {
	engine := NewTreasuryEngine()

	rule := SweepRule{
		ID:            "rule-zba-sub-01",
		SourceAccount: "ops-merchant-collect",
		MasterAccount: "treasury-master-usd",
		Type:          SweepTypeZBA,
		Currency:      "USD",
	}
	if err := engine.RegisterRule(rule); err != nil {
		t.Fatalf("RegisterRule failed: %v", err)
	}

	balances := map[string]float64{
		"ops-merchant-collect": 25000.00,
	}

	execs, err := engine.ExecuteSweeps(balances)
	if err != nil {
		t.Fatalf("ExecuteSweeps failed: %v", err)
	}
	if len(execs) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(execs))
	}

	e := execs[0]
	if e.Direction != "SWEEP_TO_MASTER" || e.TransferredAmount != 25000.00 || e.ResultingBalance != 0 {
		t.Errorf("unexpected ZBA execution result: %+v", e)
	}
}

func TestTreasuryEngine_TargetBalanceSweepingAndReplenishment(t *testing.T) {
	engine := NewTreasuryEngine()

	// Rule 1: Excess sweep (target $10,000)
	rule1 := SweepRule{
		ID:            "rule-target-excess",
		SourceAccount: "operational-payroll",
		MasterAccount: "treasury-master-usd",
		Type:          SweepTypeTargetBalance,
		TargetBalance: 10000.00,
		Currency:      "USD",
	}
	// Rule 2: Deficit replenishment (target $5,000)
	rule2 := SweepRule{
		ID:            "rule-target-deficit",
		SourceAccount: "atm-vault-cash",
		MasterAccount: "treasury-master-usd",
		Type:          SweepTypeTargetBalance,
		TargetBalance: 5000.00,
		Currency:      "USD",
	}

	_ = engine.RegisterRule(rule1)
	_ = engine.RegisterRule(rule2)

	balances := map[string]float64{
		"operational-payroll": 17500.00, // $7,500 excess
		"atm-vault-cash":      2200.00,  // $2,800 deficit
	}

	execs, err := engine.ExecuteSweeps(balances)
	if err != nil {
		t.Fatalf("ExecuteSweeps failed: %v", err)
	}
	if len(execs) != 2 {
		t.Fatalf("expected 2 executions, got %d", len(execs))
	}

	for _, ex := range execs {
		if ex.RuleID == "rule-target-excess" {
			if ex.Direction != "SWEEP_TO_MASTER" || ex.TransferredAmount != 7500.00 || ex.ResultingBalance != 10000.00 {
				t.Errorf("unexpected excess execution: %+v", ex)
			}
		} else if ex.RuleID == "rule-target-deficit" {
			if ex.Direction != "REPLENISH_FROM_MASTER" || ex.TransferredAmount != 2800.00 || ex.ResultingBalance != 5000.00 {
				t.Errorf("unexpected deficit execution: %+v", ex)
			}
		}
	}
}

func TestTreasuryEngine_SafeguardingReserveCalculation(t *testing.T) {
	engine := NewTreasuryEngine()

	// 1. Compliant scenario: $100M deposits backed by $102M trust reserves (102% coverage)
	rep1 := engine.CalculateSafeguarding(100000000.00, 102000000.00)
	if rep1.Status != "COMPLIANT" || rep1.SurplusDeficit != 2000000.00 || rep1.CoverageRatio != 1.02 {
		t.Errorf("unexpected compliant report: %+v", rep1)
	}

	// 2. Deficit scenario: $100M deposits backed by only $98M reserves ($2M deficit)
	rep2 := engine.CalculateSafeguarding(100000000.00, 98000000.00)
	if rep2.Status != "DEFICIT_ACTION_REQUIRED" || rep2.SurplusDeficit != -2000000.00 || rep2.CoverageRatio != 0.98 {
		t.Errorf("unexpected deficit report: %+v", rep2)
	}
}

func TestTreasuryEngine_Validation(t *testing.T) {
	engine := NewTreasuryEngine()

	// Identical source and master account
	err := engine.RegisterRule(SweepRule{
		SourceAccount: "acct-1",
		MasterAccount: "acct-1",
	})
	if err != ErrInvalidAccount {
		t.Errorf("expected ErrInvalidAccount, got %v", err)
	}

	// Negative target balance
	err = engine.RegisterRule(SweepRule{
		SourceAccount: "acct-1",
		MasterAccount: "acct-2",
		TargetBalance: -50,
	})
	if err != ErrInvalidTarget {
		t.Errorf("expected ErrInvalidTarget, got %v", err)
	}
}
