package ai

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// SARIncident aggregates all evidentiary findings from AI fraud detection,
// graph money mule topologies, and OFAC sanctions screening for regulatory reporting.
type SARIncident struct {
	IncidentID            string    `json:"incident_id"`
	SubjectWalletID       string    `json:"subject_wallet_id"`
	SubjectLegalName      string    `json:"subject_legal_name"`
	SubjectCountry        string    `json:"subject_country"`
	AlertTypes            []string  `json:"alert_types"`
	TotalSuspiciousVolume float64   `json:"total_suspicious_volume"`
	Currency              string    `json:"currency"`
	TransactionIDs        []string  `json:"transaction_ids"`
	Counterparties        []string  `json:"counterparties"`
	RiskScore             float64   `json:"risk_score"`
	FilingStatus          string    `json:"filing_status"` // DRAFT, READY_FOR_FILING
	CreatedAt             time.Time `json:"created_at"`
}

// SARNarrativeDossier represents a finalized FinCEN Form 111 package.
type SARNarrativeDossier struct {
	IncidentID        string    `json:"incident_id"`
	SubjectID         string    `json:"subject_id"`
	ExecutiveSummary  string    `json:"executive_summary"`
	FullNarrativeText string    `json:"full_narrative_text"`
	RegulatoryXML     string    `json:"regulatory_xml"`
	GeneratedAt       time.Time `json:"generated_at"`
}

// FinCENBSASAR is the standardized XML structure for FinCEN electronic filing.
type FinCENBSASAR struct {
	XMLName              xml.Name `xml:"FinCEN_Suspicious_Activity_Report"`
	FilingID             string   `xml:"FilingIdentifier"`
	FilingInstitution    string   `xml:"ReportingInstitution"`
	SubjectName          string   `xml:"Subject>LegalName"`
	SubjectWallet        string   `xml:"Subject>AccountIdentifier"`
	SubjectCountry       string   `xml:"Subject>Country"`
	SuspiciousAmount     float64  `xml:"Activity>Amount"`
	Currency             string   `xml:"Activity>Currency"`
	SuspiciousActivity   string   `xml:"Activity>Characterization"`
	NarrativeDescription string   `xml:"PartV_Narrative"`
	SubmissionDate       string   `xml:"SubmissionDate"`
}

// SARNarrativeGenerator produces regulator-ready dossiers for AML compliance officers.
type SARNarrativeGenerator struct {
	reportingInstitution string
}

// NewSARNarrativeGenerator initializes an automated SAR narrative generator.
func NewSARNarrativeGenerator(institution string) *SARNarrativeGenerator {
	if institution == "" {
		institution = "Global Digital Wallet Core Banking Institution"
	}
	return &SARNarrativeGenerator{reportingInstitution: institution}
}

// GenerateDossier builds both the natural language FinCEN Part V narrative and electronic XML payload.
func (g *SARNarrativeGenerator) GenerateDossier(inc SARIncident) *SARNarrativeDossier {
	now := time.Now().UTC()
	if inc.CreatedAt.IsZero() {
		inc.CreatedAt = now
	}
	if inc.Currency == "" {
		inc.Currency = "USD"
	}
	if inc.SubjectLegalName == "" {
		inc.SubjectLegalName = "Unknown Natural Person / Entity"
	}

	narrative := g.buildPartVNarrative(inc)
	xmlPayload := g.buildFinCENXML(inc, narrative, now)

	summary := fmt.Sprintf("Suspicious activity alert (%s) involving account %s with cumulative volume of %.2f %s.",
		strings.Join(inc.AlertTypes, ", "), inc.SubjectWalletID, inc.TotalSuspiciousVolume, inc.Currency)

	return &SARNarrativeDossier{
		IncidentID:        inc.IncidentID,
		SubjectID:         inc.SubjectWalletID,
		ExecutiveSummary:  summary,
		FullNarrativeText: narrative,
		RegulatoryXML:     xmlPayload,
		GeneratedAt:       now,
	}
}

func (g *SARNarrativeGenerator) buildPartVNarrative(inc SARIncident) string {
	var sb strings.Builder

	sb.WriteString("FINCEN SUSPICIOUS ACTIVITY REPORT (SAR) - PART V NARRATIVE\n")
	sb.WriteString("==========================================================\n\n")

	sb.WriteString("SECTION 1: EXECUTIVE SUMMARY\n")
	fmt.Fprintf(&sb, "This Suspicious Activity Report is submitted by %s regarding unusual transactions detected on account %s held by '%s' (Jurisdiction: %s).\n",
		g.reportingInstitution, inc.SubjectWalletID, inc.SubjectLegalName, inc.SubjectCountry)
	fmt.Fprintf(&sb, "Between automated surveillance intervals, the subject account engaged in activity matching risk patterns: %s, with an aggregate transaction value of %.2f %s (Evaluated Risk Score: %.2f).\n\n",
		strings.Join(inc.AlertTypes, "; "), inc.TotalSuspiciousVolume, inc.Currency, inc.RiskScore)

	sb.WriteString("SECTION 2: TOPOLOGICAL EVIDENCE & TRANSACTION BREAKDOWN\n")
	fmt.Fprintf(&sb, "Automated transaction graph analysis and AML behavioral engines flagged %d specific transactions involving %d known counterparties:\n",
		len(inc.TransactionIDs), len(inc.Counterparties))
	for _, txID := range inc.TransactionIDs {
		fmt.Fprintf(&sb, " - Transaction Reference ID: %s\n", txID)
	}
	for _, cp := range inc.Counterparties {
		fmt.Fprintf(&sb, " - Transacting Counterparty: %s\n", cp)
	}
	sb.WriteString("\n")

	sb.WriteString("SECTION 3: BEHAVIORAL INDICATORS & SUSPICIOUS TYPOLOGY\n")
	for _, alert := range inc.AlertTypes {
		switch alert {
		case "CIRCULAR_LOOP":
			sb.WriteString("- CIRCULAR LAYERING DETECTED: The subject account participated in closed-loop fund circulation with no evident economic substance or commercial rationale, consistent with money laundering wash cycles.\n")
		case "RAPID_PASSTHROUGH":
			sb.WriteString("- RAPID PASSTHROUGH VELOCITY: Inbound wire/deposit funds were forwarded to external endpoints within < 180 seconds with negligible retention (< 5%), indicative of an active money mule intermediary account.\n")
		case "SMURFING_NETWORK":
			sb.WriteString("- SMURFING DISPERSAL/CONVERGENCE: Multiple structured transfers originated from dispersed accounts and rapidly converged into this single destination, indicating structured smurfing to bypass statutory CTR reporting limits.\n")
		case "OFAC_SANCTION":
			sb.WriteString("- SANCTIONS LIST PROXIMITY: Direct or indirect transaction links observed with parties matching international sanctions screening profiles.\n")
		default:
			fmt.Fprintf(&sb, "- UNUSUAL ACTIVITY PATTERN: %s\n", alert)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("SECTION 4: STATUTORY & REGULATORY CITATIONS\n")
	sb.WriteString("This report is filed pursuant to the Bank Secrecy Act (BSA), 31 U.S.C. 5318(g), 31 CFR 1020.320, and FATF Recommendation 16 guidelines governing mandatory suspicious transaction reporting.\n\n")

	sb.WriteString("SECTION 5: INSTITUTIONAL ACTION TAKEN\n")
	sb.WriteString("The institution has placed the subject account on enhanced monitoring, restricted outgoing payment limits, and frozen transacting funds pending law enforcement inquiry.")

	return sb.String()
}

func (g *SARNarrativeGenerator) buildFinCENXML(inc SARIncident, narrative string, ts time.Time) string {
	doc := FinCENBSASAR{
		FilingID:             inc.IncidentID,
		FilingInstitution:    g.reportingInstitution,
		SubjectName:          inc.SubjectLegalName,
		SubjectWallet:        inc.SubjectWalletID,
		SubjectCountry:       inc.SubjectCountry,
		SuspiciousAmount:     inc.TotalSuspiciousVolume,
		Currency:             inc.Currency,
		SuspiciousActivity:   strings.Join(inc.AlertTypes, "; "),
		NarrativeDescription: narrative,
		SubmissionDate:       ts.Format("2006-01-02"),
	}

	data, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Sprintf("<error>%v</error>", err)
	}
	return xml.Header + string(data)
}
