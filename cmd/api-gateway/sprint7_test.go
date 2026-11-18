package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wallet-system/pkg/ai"
	"wallet-system/pkg/eventsourcing"
	"wallet-system/pkg/multientity"
	"wallet-system/pkg/treasury"
)

func TestGateway_MultiEntityEndpoints(t *testing.T) {
	gw := &Gateway{
		entityRegistry: multientity.NewEntityRegistry(),
	}
	gw.entitySettler = multientity.NewInterEntitySettler(gw.entityRegistry)

	// 1. List Entities
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/entities", nil)
	w1 := httptest.NewRecorder()
	gw.handleListEntities(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("handleListEntities returned %d: %s", w1.Code, w1.Body.String())
	}

	var entResp struct {
		Entities []*multientity.LegalEntity `json:"entities"`
		Count    int                        `json:"count"`
	}
	if err := json.Unmarshal(w1.Body.Bytes(), &entResp); err != nil {
		t.Fatalf("failed to decode entities: %v", err)
	}
	if entResp.Count != 4 {
		t.Errorf("expected 4 entities, got %d", entResp.Count)
	}

	// 2. Cross-Entity Settlement
	settleBody := []byte(`{
		"transaction_id": "tx-sprint7-01",
		"source_wallet": "wallet_alice_us",
		"target_wallet": "wallet_bob_eu",
		"amount": 2500.00,
		"currency": "EUR"
	}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/entities/settlement", bytes.NewReader(settleBody))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	gw.handleCrossEntitySettlement(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("handleCrossEntitySettlement returned %d: %s", w2.Code, w2.Body.String())
	}

	var settleResp multientity.CrossEntityTransferResult
	if err := json.Unmarshal(w2.Body.Bytes(), &settleResp); err != nil {
		t.Fatalf("failed to decode settlement result: %v", err)
	}
	if !settleResp.IsCrossEntity || len(settleResp.OmnibusPostings) != 4 {
		t.Errorf("expected 4 omnibus legs, got %d", len(settleResp.OmnibusPostings))
	}
}

func TestGateway_EventSourcingAuditEndpoints(t *testing.T) {
	store := eventsourcing.NewEventStore()
	gw := &Gateway{eventStore: store}
	acct := "acct-test-audit-001"

	// Append test events
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	_ = store.AppendEvent(eventsourcing.Event{
		AccountID: acct,
		Sequence:  1,
		Type:      eventsourcing.EventAccountOpened,
		Amount:    1000.00,
		Currency:  "USD",
		Timestamp: t0,
	})
	_ = store.AppendEvent(eventsourcing.Event{
		AccountID: acct,
		Sequence:  2,
		Type:      eventsourcing.EventFundsCredited,
		Amount:    500.00,
		Currency:  "USD",
		Timestamp: t0.Add(30 * time.Minute),
	})

	// Audit request as-of T0 + 10 mins (should only reflect opening of $1000)
	asOf := t0.Add(10 * time.Minute).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ledger/events?account_id="+acct+"&as_of="+asOf, nil)
	w := httptest.NewRecorder()
	gw.handleAuditEvents(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleAuditEvents returned %d: %s", w.Code, w.Body.String())
	}

	var snap eventsourcing.BalanceSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("failed to decode snapshot: %v", err)
	}
	if snap.PostedBalance != 1000.00 || snap.EventCount != 1 {
		t.Errorf("expected posted balance $1000 and 1 event at cutoff, got: %+v", snap)
	}
}

func TestGateway_TreasuryAndSafeguardingEndpoints(t *testing.T) {
	treasuryEng := treasury.NewTreasuryEngine()
	gw := &Gateway{treasuryEngine: treasuryEng}

	// 1. Create Rule
	ruleBody := []byte(`{
		"id": "rule-ops-zba",
		"source_account": "ops-card-collection",
		"master_account": "treasury-master-safe",
		"type": "ZBA",
		"currency": "USD"
	}`)
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/treasury/rules", bytes.NewReader(ruleBody))
	w1 := httptest.NewRecorder()
	gw.handleTreasuryRules(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("handleTreasuryRules POST returned %d: %s", w1.Code, w1.Body.String())
	}

	// 2. Execute Sweeps
	execBody := []byte(`{
		"balances": {
			"ops-card-collection": 75000.00
		}
	}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/treasury/sweeps/execute", bytes.NewReader(execBody))
	w2 := httptest.NewRecorder()
	gw.handleExecuteSweeps(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("handleExecuteSweeps returned %d: %s", w2.Code, w2.Body.String())
	}

	var execResp struct {
		Executions []treasury.SweepExecution `json:"executions"`
		Count      int                       `json:"count"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &execResp); err != nil {
		t.Fatalf("failed to decode sweep executions: %v", err)
	}
	if execResp.Count != 1 || execResp.Executions[0].TransferredAmount != 75000.00 {
		t.Errorf("unexpected sweep executions: %+v", execResp)
	}

	// 3. Safeguarding Report
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/treasury/safeguarding?customer_deposits=50000000&trustee_reserves=51500000", nil)
	w3 := httptest.NewRecorder()
	gw.handleSafeguarding(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("handleSafeguarding returned %d: %s", w3.Code, w3.Body.String())
	}

	var safeResp treasury.SafeguardingReport
	if err := json.Unmarshal(w3.Body.Bytes(), &safeResp); err != nil {
		t.Fatalf("failed to decode safeguarding report: %v", err)
	}
	if safeResp.Status != "COMPLIANT" || safeResp.SurplusDeficit != 1500000.00 {
		t.Errorf("unexpected safeguarding report: %+v", safeResp)
	}
}

func TestGateway_SARNarrativeGeneratorEndpoint(t *testing.T) {
	gw := &Gateway{
		sarGenerator: ai.NewSARNarrativeGenerator("Apex Digital Bank"),
	}

	sarBody := []byte(`{
		"incident_id": "sar-test-44",
		"subject_wallet_id": "wallet-hacker-99",
		"subject_legal_name": "Ghost Holdings SA",
		"subject_country": "PA",
		"alert_types": ["CIRCULAR_LOOP", "RAPID_PASSTHROUGH"],
		"total_suspicious_volume": 850000.00,
		"currency": "USD",
		"risk_score": 0.98
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/compliance/sar/generate", bytes.NewReader(sarBody))
	w := httptest.NewRecorder()
	gw.handleGenerateSAR(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleGenerateSAR returned %d: %s", w.Code, w.Body.String())
	}

	var dossier ai.SARNarrativeDossier
	if err := json.Unmarshal(w.Body.Bytes(), &dossier); err != nil {
		t.Fatalf("failed to decode SAR dossier: %v", err)
	}
	if dossier.IncidentID != "sar-test-44" || dossier.FullNarrativeText == "" || dossier.RegulatoryXML == "" {
		t.Errorf("incomplete dossier response: %+v", dossier)
	}
}
