package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"wallet-system/pkg/auth"
)

func runFailoverCmd(args []string, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("failover", flag.ContinueOnError)
	fs.SetOutput(errOut)

	gatewayURL := fs.String("gateway-url", "http://localhost:8080", "API Gateway base URL")
	target := fs.String("target", "", "Target region to route to: PRIMARY or STANDBY (leave empty to query)")
	token := fs.String("token", "", "Admin Bearer token for authentication")
	timeout := fs.Duration("timeout", 5*time.Second, "HTTP request timeout")
	jsonOutput := fs.Bool("json", false, "Output results in JSON format")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	base := strings.TrimRight(*gatewayURL, "/")
	client := &http.Client{Timeout: *timeout}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// If no target specified, query status
	if *target == "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/cluster/status", nil)
		if err != nil {
			fmt.Fprintf(errOut, "Error building status request: %v\n", err)
			return 1
		}

		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(errOut, "Failed to connect to gateway at %s: %v\n", base, err)
			return 1
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(errOut, "Gateway returned HTTP %d: %s\n", resp.StatusCode, string(body))
			return 1
		}

		if *jsonOutput {
			fmt.Fprintln(out, string(body))
			return 0
		}

		var cs clusterStatusPayload
		_ = json.Unmarshal(body, &cs)
		fmt.Fprintf(out, "Active Cluster Target: %s\n", cs.CurrentRoutedTarget)
		fmt.Fprintf(out, "  Primary Region (%s): %v\n", cs.PrimaryRegion.Address, cs.PrimaryRegion.Status)
		fmt.Fprintf(out, "  Standby Region (%s): %v\n", cs.StandbyRegion.Address, cs.StandbyRegion.Status)
		return 0
	}

	normTarget := strings.ToUpper(strings.TrimSpace(*target))
	if normTarget != "PRIMARY" && normTarget != "STANDBY" {
		fmt.Fprintf(errOut, "Invalid target %q: must be either PRIMARY or STANDBY\n", *target)
		return 1
	}

	// Use provided token, or auto-mint administrative token if none provided
	authToken := *token
	if authToken == "" {
		sec := os.Getenv("JWT_SECRET")
		if sec == "" {
			sec = defaultCLIJWTSecret
		}
		minted, err := auth.GenerateToken(auth.Claims{
			Subject:   "walletctl-operator",
			Roles:     []string{"admin"},
			Scopes:    []string{"cluster:admin"},
			IssuedAt:  time.Now().Unix(),
			ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
		}, sec)
		if err == nil {
			authToken = minted
		}
	}

	payload, _ := json.Marshal(map[string]string{"target": normTarget})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/cluster/failover", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(errOut, "Error building failover request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(errOut, "Failover request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(errOut, "Failover failed with HTTP %d: %s\n", resp.StatusCode, string(body))
		return 1
	}

	if *jsonOutput {
		fmt.Fprintln(out, string(body))
		return 0
	}

	fmt.Fprintf(out, "Failover successfully executed. New active target: %s\n", normTarget)
	return 0
}
