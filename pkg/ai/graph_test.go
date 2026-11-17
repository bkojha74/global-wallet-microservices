package ai

import (
	"testing"
	"time"
)

func TestMuleGraphEngine_CircularLoop(t *testing.T) {
	engine := NewMuleGraphEngine(72 * time.Hour)
	now := time.Now().UTC()

	// A -> B
	alerts := engine.RecordTransaction("tx-1", "wallet-A", "wallet-B", 5000, "USD", now)
	if len(alerts) != 0 {
		t.Fatalf("expected 0 alerts on initial edge, got %d", len(alerts))
	}

	// B -> C
	alerts = engine.RecordTransaction("tx-2", "wallet-B", "wallet-C", 5000, "USD", now.Add(10*time.Minute))
	if len(alerts) != 0 {
		t.Fatalf("expected 0 alerts on second edge, got %d", len(alerts))
	}

	// C -> A (Completes cycle A -> B -> C -> A)
	alerts = engine.RecordTransaction("tx-3", "wallet-C", "wallet-A", 4900, "USD", now.Add(20*time.Minute))
	if len(alerts) == 0 {
		t.Fatalf("expected circular loop alert, got none")
	}

	foundLoop := false
	for _, a := range alerts {
		if a.Type == AlertTypeCircularLoop {
			foundLoop = true
			if a.RiskScore < 0.9 {
				t.Errorf("expected high risk score >= 0.9, got %f", a.RiskScore)
			}
		}
	}
	if !foundLoop {
		t.Errorf("expected AlertTypeCircularLoop in alerts")
	}
}

func TestMuleGraphEngine_RapidPassthrough(t *testing.T) {
	engine := NewMuleGraphEngine(72 * time.Hour)
	now := time.Now().UTC()

	// Step 1: External -> Mule ($10,000)
	alerts := engine.RecordTransaction("tx-in", "wallet-origin", "wallet-mule", 10000, "USD", now)
	if len(alerts) != 0 {
		t.Fatalf("expected 0 alerts on initial inbound, got %d", len(alerts))
	}

	// Step 2: 45 seconds later, Mule -> Offramp ($9,700, 97% forwarded, < 5% retained)
	alerts = engine.RecordTransaction("tx-out", "wallet-mule", "wallet-offramp", 9700, "USD", now.Add(45*time.Second))
	if len(alerts) == 0 {
		t.Fatalf("expected rapid passthrough alert, got none")
	}

	foundPassthrough := false
	for _, a := range alerts {
		if a.Type == AlertTypeRapidPassthrough {
			foundPassthrough = true
			if len(a.InvolvedNodes) != 3 {
				t.Errorf("expected 3 involved nodes (origin, mule, offramp), got %d", len(a.InvolvedNodes))
			}
		}
	}
	if !foundPassthrough {
		t.Errorf("expected AlertTypeRapidPassthrough in alerts")
	}
}

func TestMuleGraphEngine_SmurfingNetwork(t *testing.T) {
	engine := NewMuleGraphEngine(72 * time.Hour)
	now := time.Now().UTC()
	origin := "wallet-master-cartel"
	exitWallet := "wallet-clean-exit"

	// Fan-out: Origin transfers to 5 intermediary wallets
	for i := 1; i <= 5; i++ {
		interWallet := "wallet-smurf-" + string(rune('0'+i))
		engine.RecordTransaction("tx-fanout-"+string(rune('0'+i)), origin, interWallet, 2000, "USD", now.Add(time.Duration(i)*time.Minute))
	}

	// Fan-in: The 5 intermediaries converge into single exit wallet within 2 hours
	var lastAlerts []MuleAlert
	for i := 1; i <= 5; i++ {
		interWallet := "wallet-smurf-" + string(rune('0'+i))
		alerts := engine.RecordTransaction("tx-fanin-"+string(rune('0'+i)), interWallet, exitWallet, 1950, "USD", now.Add(time.Hour+time.Duration(i)*time.Minute))
		if len(alerts) > 0 {
			lastAlerts = alerts
		}
	}

	if len(lastAlerts) == 0 {
		t.Fatalf("expected smurfing network alert upon 5th converging transfer, got none")
	}

	foundSmurfing := false
	for _, a := range lastAlerts {
		if a.Type == AlertTypeSmurfingNetwork {
			foundSmurfing = true
			if a.RiskScore < 0.95 {
				t.Errorf("expected risk score >= 0.95, got %f", a.RiskScore)
			}
		}
	}
	if !foundSmurfing {
		t.Errorf("expected AlertTypeSmurfingNetwork in alerts")
	}
}

func TestMuleGraphEngine_WindowEviction(t *testing.T) {
	window := 10 * time.Minute
	engine := NewMuleGraphEngine(window)
	now := time.Now().UTC()

	// Tx 1 at T=0
	engine.RecordTransaction("tx-old", "wallet-A", "wallet-B", 1000, "USD", now)

	// Tx 2 at T + 15 minutes (should evict Tx 1)
	engine.RecordTransaction("tx-new", "wallet-C", "wallet-D", 1000, "USD", now.Add(15*time.Minute))

	engine.mu.RLock()
	edgeCount := len(engine.edges)
	engine.mu.RUnlock()

	if edgeCount != 1 {
		t.Errorf("expected 1 edge after eviction, got %d", edgeCount)
	}
}
