package ai

import (
	"sync"
	"time"
)

// MuleAlertType represents the type of money laundering / mule network topology detected.
type MuleAlertType string

const (
	AlertTypeCircularLoop     MuleAlertType = "CIRCULAR_LOOP"
	AlertTypeRapidPassthrough MuleAlertType = "RAPID_PASSTHROUGH" // #nosec G101 -- false positive on transaction fund pass-through
	AlertTypeSmurfingNetwork  MuleAlertType = "SMURFING_NETWORK"
)

// GraphEdge represents a directed transfer in the transaction graph.
type GraphEdge struct {
	TxID      string    `json:"tx_id"`
	Source    string    `json:"source"`
	Target    string    `json:"target"`
	Amount    float64   `json:"amount"`
	Currency  string    `json:"currency"`
	Timestamp time.Time `json:"timestamp"`
}

// MuleAlert describes a detected suspicious money mule or smurfing topology.
type MuleAlert struct {
	Type          MuleAlertType `json:"type"`
	RiskScore     float64       `json:"risk_score"` // 0.0 - 1.0
	Description   string        `json:"description"`
	InvolvedNodes []string      `json:"involved_nodes"`
	DetectedAt    time.Time     `json:"detected_at"`
}

// MuleGraphEngine tracks directed transaction graphs over rolling windows to detect laundering topologies.
type MuleGraphEngine struct {
	mu           sync.RWMutex
	window       time.Duration
	edges        []GraphEdge
	adjOut       map[string][]GraphEdge
	adjIn        map[string][]GraphEdge
	recentAlerts []MuleAlert
}

// NewMuleGraphEngine initializes a MuleGraphEngine with a 72-hour sliding window.
func NewMuleGraphEngine(window time.Duration) *MuleGraphEngine {
	if window <= 0 {
		window = 72 * time.Hour
	}
	return &MuleGraphEngine{
		window: window,
		adjOut: make(map[string][]GraphEdge),
		adjIn:  make(map[string][]GraphEdge),
	}
}

// RecordTransaction adds a transfer to the transaction graph and evaluates money mule topologies.
func (g *MuleGraphEngine) RecordTransaction(txID, src, dst string, amount float64, currency string, ts time.Time) []MuleAlert {
	g.mu.Lock()
	defer g.mu.Unlock()

	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	edge := GraphEdge{
		TxID:      txID,
		Source:    src,
		Target:    dst,
		Amount:    amount,
		Currency:  currency,
		Timestamp: ts,
	}

	// 1. Evict edges outside the rolling window
	g.evictExpired(ts)

	// 2. Insert new edge
	g.edges = append(g.edges, edge)
	g.adjOut[src] = append(g.adjOut[src], edge)
	g.adjIn[dst] = append(g.adjIn[dst], edge)

	// 3. Detect Topologies
	var alerts []MuleAlert

	// (A) Circular Layering Loop (e.g. A -> B -> C -> A)
	if loopNodes := g.detectCircularLoop(src, dst); len(loopNodes) > 0 {
		alert := MuleAlert{
			Type:          AlertTypeCircularLoop,
			RiskScore:     0.95,
			Description:   "Circular layering cycle detected across accounts",
			InvolvedNodes: loopNodes,
			DetectedAt:    ts,
		}
		alerts = append(alerts, alert)
	}

	// (B) Rapid Passthrough Velocity: Target account receives funds and forwards >= 95% within < 180s
	if passthroughAlert := g.detectRapidPassthrough(src, dst, amount, ts); passthroughAlert != nil {
		alerts = append(alerts, *passthroughAlert)
	}

	// (C) Fan-Out / Fan-In Smurfing: Dispersal from single origin to 5+ intermediaries converging to exit
	if smurfingAlert := g.detectSmurfing(dst, ts); smurfingAlert != nil {
		alerts = append(alerts, *smurfingAlert)
	}

	g.recentAlerts = append(g.recentAlerts, alerts...)
	return alerts
}

func (g *MuleGraphEngine) evictExpired(now time.Time) {
	cutoff := now.Add(-g.window)
	newEdges := make([]GraphEdge, 0, len(g.edges))
	g.adjOut = make(map[string][]GraphEdge)
	g.adjIn = make(map[string][]GraphEdge)

	for _, e := range g.edges {
		if e.Timestamp.After(cutoff) {
			newEdges = append(newEdges, e)
			g.adjOut[e.Source] = append(g.adjOut[e.Source], e)
			g.adjIn[e.Target] = append(g.adjIn[e.Target], e)
		}
	}
	g.edges = newEdges
}

// detectCircularLoop checks if adding src -> dst creates a directed cycle back to src.
// We search if there is a path from dst to src in adjOut.
func (g *MuleGraphEngine) detectCircularLoop(src, dst string) []string {
	if src == dst {
		return []string{src}
	}

	visited := make(map[string]bool)
	parent := make(map[string]string)
	queue := []string{dst}
	visited[dst] = true

	found := false
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		if curr == src {
			found = true
			break
		}

		for _, edge := range g.adjOut[curr] {
			next := edge.Target
			if !visited[next] {
				visited[next] = true
				parent[next] = curr
				queue = append(queue, next)
			}
		}
	}

	if !found {
		return nil
	}

	// Reconstruct cycle path
	var path []string
	curr := src
	for curr != "" {
		path = append([]string{curr}, path...)
		if curr == dst {
			break
		}
		curr = parent[curr]
	}
	// Append the closing hop
	path = append(path, dst)
	return path
}

// detectRapidPassthrough checks if src was a recipient of inbound funds within 180s
// and is now sending out >= 95% of those funds (< 5% retention).
func (g *MuleGraphEngine) detectRapidPassthrough(src, dst string, outboundAmount float64, now time.Time) *MuleAlert {
	inbounds := g.adjIn[src]
	if len(inbounds) == 0 {
		return nil
	}

	for _, in := range inbounds {
		timeDiff := now.Sub(in.Timestamp)
		if timeDiff >= 0 && timeDiff <= 180*time.Second {
			// Retention check: forwarding >= 95% of inbound
			if in.Amount > 0 && (outboundAmount/in.Amount) >= 0.95 {
				return &MuleAlert{
					Type:          AlertTypeRapidPassthrough,
					RiskScore:     0.90,
					Description:   "Rapid passthrough detected: funds forwarded within 180s with <5% retention",
					InvolvedNodes: []string{in.Source, src, dst},
					DetectedAt:    now,
				}
			}
		}
	}
	return nil
}

// detectSmurfing checks if target dst is receiving funds from 5+ intermediaries that originated from a common source within 4 hours.
func (g *MuleGraphEngine) detectSmurfing(exitNode string, now time.Time) *MuleAlert {
	inbounds := g.adjIn[exitNode]
	if len(inbounds) < 5 {
		return nil
	}

	// Track distinct intermediary sources to exitNode
	intermediaries := make(map[string]bool)
	for _, in := range inbounds {
		if now.Sub(in.Timestamp) <= 4*time.Hour {
			intermediaries[in.Source] = true
		}
	}

	if len(intermediaries) < 5 {
		return nil
	}

	// Check if these intermediaries share a common origin source wallet
	originCounts := make(map[string]int)
	for inter := range intermediaries {
		for _, inEdge := range g.adjIn[inter] {
			if now.Sub(inEdge.Timestamp) <= 4*time.Hour {
				originCounts[inEdge.Source]++
			}
		}
	}

	for origin, count := range originCounts {
		if count >= 5 {
			var nodes []string
			nodes = append(nodes, origin)
			for inter := range intermediaries {
				nodes = append(nodes, inter)
			}
			nodes = append(nodes, exitNode)

			return &MuleAlert{
				Type:          AlertTypeSmurfingNetwork,
				RiskScore:     0.98,
				Description:   "Smurfing network detected: fan-out from origin converging into common exit wallet",
				InvolvedNodes: nodes,
				DetectedAt:    now,
			}
		}
	}

	return nil
}

// GetRecentAlerts returns all security alerts generated in the active window.
func (g *MuleGraphEngine) GetRecentAlerts() []MuleAlert {
	g.mu.RLock()
	defer g.mu.RUnlock()

	res := make([]MuleAlert, len(g.recentAlerts))
	copy(res, g.recentAlerts)
	return res
}
