package ai

import (
	"strings"
	"testing"
	"time"
)

func TestSARNarrativeGenerator_FullDossier(t *testing.T) {
	generator := NewSARNarrativeGenerator("First International Digital Reserve Bank")

	incident := SARIncident{
		IncidentID:            "sar-inc-2026-9901",
		SubjectWalletID:       "wallet-suspect-mule-1",
		SubjectLegalName:      "Acme Logistics Shell LLC",
		SubjectCountry:        "CY",
		AlertTypes:            []string{"CIRCULAR_LOOP", "RAPID_PASSTHROUGH", "SMURFING_NETWORK"},
		TotalSuspiciousVolume: 475000.00,
		Currency:              "USD",
		TransactionIDs:        []string{"tx-cy-01", "tx-cy-02", "tx-cy-03"},
		Counterparties:        []string{"wallet-layer-A", "wallet-layer-B"},
		RiskScore:             0.96,
		FilingStatus:          "READY_FOR_FILING",
		CreatedAt:             time.Now().UTC(),
	}

	dossier := generator.GenerateDossier(incident)
	if dossier == nil {
		t.Fatalf("expected non-nil dossier")
	}

	// 1. Validate Executive Summary
	if !strings.Contains(dossier.ExecutiveSummary, "CIRCULAR_LOOP") || !strings.Contains(dossier.ExecutiveSummary, "475000.00 USD") {
		t.Errorf("executive summary missing key fields: %s", dossier.ExecutiveSummary)
	}

	// 2. Validate Part V Narrative content
	narrative := dossier.FullNarrativeText
	if !strings.Contains(narrative, "CIRCULAR LAYERING DETECTED") {
		t.Errorf("narrative missing circular loop explanation")
	}
	if !strings.Contains(narrative, "RAPID PASSTHROUGH VELOCITY") {
		t.Errorf("narrative missing rapid passthrough explanation")
	}
	if !strings.Contains(narrative, "SMURFING DISPERSAL/CONVERGENCE") {
		t.Errorf("narrative missing smurfing explanation")
	}
	if !strings.Contains(narrative, "31 U.S.C. 5318(g)") {
		t.Errorf("narrative missing statutory BSA citation")
	}

	// 3. Validate XML payload
	xmlStr := dossier.RegulatoryXML
	if !strings.Contains(xmlStr, "<FinCEN_Suspicious_Activity_Report>") {
		t.Errorf("xml payload missing root element: %s", xmlStr)
	}
	if !strings.Contains(xmlStr, "<FilingIdentifier>sar-inc-2026-9901</FilingIdentifier>") {
		t.Errorf("xml payload missing filing ID: %s", xmlStr)
	}
	if !strings.Contains(xmlStr, "<ReportingInstitution>First International Digital Reserve Bank</ReportingInstitution>") {
		t.Errorf("xml payload missing reporting institution: %s", xmlStr)
	}
}
