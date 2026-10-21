package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type healthCheckResult struct {
	Component string        `json:"component"`
	Endpoint  string        `json:"endpoint"`
	Status    string        `json:"status"`
	Latency   time.Duration `json:"latency_ms"`
	Details   string        `json:"details,omitempty"`
}

type clusterStatusPayload struct {
	CurrentRoutedTarget string `json:"current_routed_target"`
	PrimaryRegion       struct {
		Address string      `json:"address"`
		Status  interface{} `json:"status"`
	} `json:"primary_region"`
	StandbyRegion struct {
		Address string      `json:"address"`
		Status  interface{} `json:"status"`
	} `json:"standby_region"`
}

type fullHealthReport struct {
	OverallStatus string              `json:"overall_status"`
	ProbedAt      string              `json:"probed_at"`
	ActiveTarget  string              `json:"active_target,omitempty"`
	Checks        []healthCheckResult `json:"checks"`
}

func probeEndpoint(ctx context.Context, client *http.Client, url string) (int, []byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return 0, nil, latency, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, latency, nil
}

func runHealthCmd(args []string, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(errOut)

	gatewayURL := fs.String("gateway-url", "http://localhost:8080", "API Gateway base URL")
	timeout := fs.Duration("timeout", 5*time.Second, "HTTP timeout for health probes")
	jsonOutput := fs.Bool("json", false, "Output results in JSON format")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	base := strings.TrimRight(*gatewayURL, "/")
	client := &http.Client{Timeout: *timeout}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var checks []healthCheckResult
	overallHealthy := true

	// 1. Probe /healthz
	code, _, lat, err := probeEndpoint(ctx, client, base+"/healthz")
	if err != nil || code != http.StatusOK {
		overallHealthy = false
		checks = append(checks, healthCheckResult{
			Component: "Gateway Liveness",
			Endpoint:  base + "/healthz",
			Status:    "DOWN",
			Latency:   lat,
			Details:   fmt.Sprintf("error: %v (code %d)", err, code),
		})
	} else {
		checks = append(checks, healthCheckResult{
			Component: "Gateway Liveness",
			Endpoint:  base + "/healthz",
			Status:    "UP",
			Latency:   lat,
		})
	}

	// 2. Probe /readyz
	code, body, lat, err := probeEndpoint(ctx, client, base+"/readyz")
	if err != nil || code != http.StatusOK {
		overallHealthy = false
		checks = append(checks, healthCheckResult{
			Component: "Gateway Readiness",
			Endpoint:  base + "/readyz",
			Status:    "DEGRADED",
			Latency:   lat,
			Details:   fmt.Sprintf("error: %v (code %d)", err, code),
		})
	} else {
		var readyData map[string]interface{}
		_ = json.Unmarshal(body, &readyData)
		details := ""
		if t, ok := readyData["active_target"].(string); ok {
			details = "Target: " + t
		}
		checks = append(checks, healthCheckResult{
			Component: "Gateway Readiness",
			Endpoint:  base + "/readyz",
			Status:    "READY",
			Latency:   lat,
			Details:   details,
		})
	}

	// 3. Probe /api/v1/cluster/status
	activeTarget := "UNKNOWN"
	code, body, lat, err = probeEndpoint(ctx, client, base+"/api/v1/cluster/status")
	if err == nil && code == http.StatusOK {
		var cs clusterStatusPayload
		if err := json.Unmarshal(body, &cs); err == nil {
			activeTarget = cs.CurrentRoutedTarget
			checks = append(checks, healthCheckResult{
				Component: "Cluster Routing",
				Endpoint:  base + "/api/v1/cluster/status",
				Status:    "ACTIVE: " + activeTarget,
				Latency:   lat,
				Details:   fmt.Sprintf("Primary: %s | Standby: %s", cs.PrimaryRegion.Address, cs.StandbyRegion.Address),
			})
		}
	} else {
		checks = append(checks, healthCheckResult{
			Component: "Cluster Routing",
			Endpoint:  base + "/api/v1/cluster/status",
			Status:    "UNAVAILABLE",
			Latency:   lat,
			Details:   fmt.Sprintf("error: %v", err),
		})
	}

	overallStatus := "HEALTHY"
	if !overallHealthy {
		overallStatus = "UNHEALTHY"
	}

	report := fullHealthReport{
		OverallStatus: overallStatus,
		ProbedAt:      time.Now().UTC().Format(time.RFC3339),
		ActiveTarget:  activeTarget,
		Checks:        checks,
	}

	if *jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Fprintf(out, "Global Wallet Health Check - Status: %s (Target: %s)\n", overallStatus, activeTarget)
		fmt.Fprintf(out, "%-22s %-12s %-10s %s\n", "COMPONENT", "STATUS", "LATENCY", "DETAILS")
		fmt.Fprintln(out, strings.Repeat("-", 75))
		for _, c := range checks {
			latStr := fmt.Sprintf("%dms", c.Latency.Milliseconds())
			fmt.Fprintf(out, "%-22s %-12s %-10s %s\n", c.Component, c.Status, latStr, c.Details)
		}
	}

	if overallHealthy {
		return 0
	}
	return 1
}
