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

const defaultCLIJWTSecret = "development-wallet-insecure-secret-key-change-in-prod"

type tokenOutput struct {
	AccessToken string   `json:"access_token"`
	TokenType   string   `json:"token_type"`
	Subject     string   `json:"subject"`
	Roles       []string `json:"roles"`
	Scopes      []string `json:"scopes"`
	ExpiresIn   int64    `json:"expires_in"`
	ExpiresAt   string   `json:"expires_at"`
}

func parseCommaList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		clean := strings.TrimSpace(p)
		if clean != "" {
			result = append(result, clean)
		}
	}
	return result
}

func runTokenCmd(args []string, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(errOut)

	subject := fs.String("subject", "operator", "Token subject / user ID")
	rolesStr := fs.String("roles", "admin", "Comma-separated roles (e.g. user,admin)")
	scopesStr := fs.String("scopes", "wallet:read,wallet:write,wallet:transfer,cluster:admin", "Comma-separated scopes")
	ttl := fs.Duration("ttl", 1*time.Hour, "Token validity lifetime")
	secret := fs.String("secret", "", "HMAC-SHA256 secret (defaults to JWT_SECRET env var or development key)")
	remote := fs.Bool("remote", false, "Request token from API Gateway /api/v1/auth/token endpoint")
	gatewayURL := fs.String("gateway-url", "http://localhost:8080", "API Gateway base URL (when using --remote)")
	jsonOutput := fs.Bool("json", false, "Output token details in JSON format")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	roles := parseCommaList(*rolesStr)
	scopes := parseCommaList(*scopesStr)

	if *remote {
		base := strings.TrimRight(*gatewayURL, "/")
		reqBody, _ := json.Marshal(map[string]interface{}{
			"subject": *subject,
			"roles":   roles,
			"scopes":  scopes,
		})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/auth/token", bytes.NewReader(reqBody))
		if err != nil {
			fmt.Fprintf(errOut, "Error building remote token request: %v\n", err)
			return 1
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintf(errOut, "Remote token request failed: %v\n", err)
			return 1
		}
		defer resp.Body.Close()

		respBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(errOut, "Gateway returned HTTP %d: %s\n", resp.StatusCode, string(respBytes))
			return 1
		}

		if *jsonOutput {
			fmt.Fprintln(out, string(respBytes))
			return 0
		}

		var parsed tokenOutput
		if err := json.Unmarshal(respBytes, &parsed); err == nil && parsed.AccessToken != "" {
			fmt.Fprintln(out, parsed.AccessToken)
			return 0
		}
		fmt.Fprintln(out, string(respBytes))
		return 0
	}

	// Offline token generation
	sec := *secret
	if sec == "" {
		sec = os.Getenv("JWT_SECRET")
	}
	if sec == "" {
		sec = defaultCLIJWTSecret
	}

	now := time.Now().UTC()
	exp := now.Add(*ttl).Unix()

	claims := auth.Claims{
		Subject:   *subject,
		Roles:     roles,
		Scopes:    scopes,
		IssuedAt:  now.Unix(),
		ExpiresAt: exp,
		Issuer:    auth.DefaultIssuer,
		Audience:  auth.DefaultAudience,
	}

	tokenStr, err := auth.GenerateToken(claims, sec)
	if err != nil {
		fmt.Fprintf(errOut, "Error generating token: %v\n", err)
		return 1
	}

	if *jsonOutput {
		tOut := tokenOutput{
			AccessToken: tokenStr,
			TokenType:   "Bearer",
			Subject:     *subject,
			Roles:       roles,
			Scopes:      scopes,
			ExpiresIn:   int64(ttl.Seconds()),
			ExpiresAt:   time.Unix(exp, 0).UTC().Format(time.RFC3339),
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		// #nosec G117 -- access token is intentionally printed as CLI output upon user request
		_ = enc.Encode(tOut)
	} else {
		fmt.Fprintln(out, tokenStr)
	}

	return 0
}
