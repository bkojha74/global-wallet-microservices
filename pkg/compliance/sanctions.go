package compliance

import (
	"encoding/json"
	"encoding/xml"
	"math"
	"strings"
	"sync"
	"time"
)

// SanctionsEntry defines an entity listed on OFAC SDN, UN, or EU consolidated sanctions lists.
type SanctionsEntry struct {
	EntityID        string   `json:"entity_id"`
	FullName        string   `json:"full_name"`
	Aliases         []string `json:"aliases,omitempty"`
	Country         string   `json:"country,omitempty"` // ISO 3166-1 alpha-2 or high-risk jurisdiction
	Category        string   `json:"category"`          // "SDN", "TERRORISM", "PROLIFERATION", "PEP"
	SanctionProgram string   `json:"sanction_program"`  // e.g. "UKRAINE-EO14024", "IRAN", "DPRK"
}

// SanctionsMatchResult details the outcome of an AML sanctions screen.
type SanctionsMatchResult struct {
	IsBlocked       bool    `json:"is_blocked"`
	RequiresReview  bool    `json:"requires_review"`
	MatchScore      float64 `json:"match_score"` // 0.0 - 1.0
	MatchedName     string  `json:"matched_name,omitempty"`
	EntityID        string  `json:"entity_id,omitempty"`
	SanctionProgram string  `json:"sanction_program,omitempty"`
	Action          string  `json:"action"` // "BLOCK", "REVIEW", "CLEAR"
}

// SanctionsEngine provides real-time in-memory sanctions screening with fuzzy matching.
type SanctionsEngine struct {
	mu      sync.RWMutex
	entries []SanctionsEntry
}

// NewSanctionsEngine creates a screening engine populated with international sanctions designations.
func NewSanctionsEngine() *SanctionsEngine {
	engine := &SanctionsEngine{
		entries: make([]SanctionsEntry, 0),
	}

	// Pre-populate core baseline OFAC SDN, UN and high-risk sanctions profiles
	defaultSanctions := []SanctionsEntry{
		{
			EntityID:        "OFAC-SDN-1001",
			FullName:        "EVIL CORP CYBER SYNDICATE",
			Aliases:         []string{"EVILCORP", "DRIDEX GANG"},
			Country:         "RU",
			Category:        "CYBER-CRIME",
			SanctionProgram: "CYBER2",
		},
		{
			EntityID:        "OFAC-SDN-1002",
			FullName:        "AL-QALAM WEAPONS LOGISTICS",
			Aliases:         []string{"QALAM LOGISTICS", "AL QALAM TRADING"},
			Country:         "SY",
			Category:        "TERRORISM",
			SanctionProgram: "SDGT",
		},
		{
			EntityID:        "OFAC-SDN-1003",
			FullName:        "KORYO ILLICIT COMMERCE BANK",
			Aliases:         []string{"KORYO CREDIT", "KORYO COMMERCIAL"},
			Country:         "KP",
			Category:        "PROLIFERATION",
			SanctionProgram: "DPRK3",
		},
		{
			EntityID:        "OFAC-SDN-1004",
			FullName:        "CARTEL DEL GOLFO PACIFICO",
			Aliases:         []string{"GOLFO PACIFICO TRADING"},
			Country:         "MX",
			Category:        "NARCOTICS",
			SanctionProgram: "SDNTK",
		},
	}

	engine.entries = append(engine.entries, defaultSanctions...)
	return engine
}

// AddEntry dynamically appends a sanctioned entity to the active screening set.
func (s *SanctionsEngine) AddEntry(entry SanctionsEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
}

// CleanName normalizes entity names by stripping punctuation and corporate suffixes.
func CleanName(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, p := range []string{",", ".", "-", "_", "/", "\\", "(", ")", "'", "\""} {
		lower = strings.ReplaceAll(lower, p, "")
	}

	suffixes := []string{" llc", " ltd", " inc", " corp", " gmbh", " plc", " sa", " ag", " sarl"}
	for _, suffix := range suffixes {
		lower = strings.TrimSuffix(lower, suffix)
	}
	return strings.TrimSpace(lower)
}

// JaroSimilarity calculates the Jaro distance between two normalized character sequences.
func JaroSimilarity(s1, s2 string) float64 {
	if s1 == s2 {
		return 1.0
	}
	l1, l2 := len(s1), len(s2)
	if l1 == 0 || l2 == 0 {
		return 0.0
	}

	matchDistance := int(math.Floor(math.Max(float64(l1), float64(l2))/2)) - 1
	if matchDistance < 0 {
		matchDistance = 0
	}

	s1Matches := make([]bool, l1)
	s2Matches := make([]bool, l2)
	matches := 0

	for i := 0; i < l1; i++ {
		start := int(math.Max(0, float64(i-matchDistance)))
		end := int(math.Min(float64(i+matchDistance+1), float64(l2)))
		for j := start; j < end; j++ {
			if s2Matches[j] || s1[i] != s2[j] {
				continue
			}
			s1Matches[i] = true
			s2Matches[j] = true
			matches++
			break
		}
	}

	if matches == 0 {
		return 0.0
	}

	transpositions := 0
	k := 0
	for i := 0; i < l1; i++ {
		if !s1Matches[i] {
			continue
		}
		for !s2Matches[k] {
			k++
		}
		if s1[i] != s2[k] {
			transpositions++
		}
		k++
	}

	m := float64(matches)
	return (m/float64(l1) + m/float64(l2) + (m-float64(transpositions/2))/m) / 3.0
}

// ScreenParty evaluates a party name against the active sanctions database.
func (s *SanctionsEngine) ScreenParty(name, country string) *SanctionsMatchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cleanedTarget := CleanName(name)
	if cleanedTarget == "" {
		return &SanctionsMatchResult{Action: "CLEAR"}
	}

	var bestScore float64
	var bestMatch SanctionsEntry
	var matchedAlias string

	for _, entry := range s.entries {
		namesToTest := append([]string{entry.FullName}, entry.Aliases...)
		for _, candidate := range namesToTest {
			cleanedCandidate := CleanName(candidate)
			var score float64
			if cleanedTarget == cleanedCandidate {
				score = 1.0
			} else {
				score = JaroSimilarity(cleanedTarget, cleanedCandidate)
			}

			// Country match bonus
			if country != "" && entry.Country != "" && strings.EqualFold(country, entry.Country) {
				score = math.Min(1.0, score+0.05)
			}

			if score > bestScore {
				bestScore = score
				bestMatch = entry
				matchedAlias = candidate
			}
		}
	}

	res := &SanctionsMatchResult{
		MatchScore: bestScore,
	}

	if bestScore >= 0.85 {
		res.IsBlocked = true
		res.Action = "BLOCK"
		res.EntityID = bestMatch.EntityID
		res.MatchedName = matchedAlias
		res.SanctionProgram = bestMatch.SanctionProgram
	} else if bestScore >= 0.70 {
		res.RequiresReview = true
		res.Action = "REVIEW"
		res.EntityID = bestMatch.EntityID
		res.MatchedName = matchedAlias
		res.SanctionProgram = bestMatch.SanctionProgram
	} else {
		res.Action = "CLEAR"
	}

	return res
}

// SARReport documents an automated FinCEN Suspicious Activity Report.
type SARReport struct {
	XMLName         xml.Name          `xml:"SuspiciousActivityReport" json:"-"`
	ReportID        string            `xml:"ReportID" json:"report_id"`
	Timestamp       string            `xml:"Timestamp" json:"timestamp"`
	NarrativeReason string            `xml:"NarrativeReason" json:"narrative_reason"`
	Amount          float64           `xml:"Amount" json:"amount"`
	Currency        string            `xml:"Currency" json:"currency"`
	Parties         []TravelRuleParty `xml:"SubjectParties>Party" json:"parties"`
}

// GenerateSAR compiles a regulatory Suspicious Activity Report payload.
func GenerateSAR(reportID, reason string, parties []TravelRuleParty, amount float64, currency string) ([]byte, error) {
	report := SARReport{
		ReportID:        reportID,
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		NarrativeReason: reason,
		Amount:          amount,
		Currency:        currency,
		Parties:         parties,
	}
	return json.MarshalIndent(report, "", "  ")
}
