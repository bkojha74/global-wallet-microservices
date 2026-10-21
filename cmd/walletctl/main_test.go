package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wallet-system/pkg/auth"
	"wallet-system/pkg/webhook"
)

func TestRunCLI_VersionAndHelp(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantSubstr string
	}{
		{
			name:       "empty args shows usage",
			args:       []string{"walletctl"},
			wantExit:   0,
			wantSubstr: "walletctl - Unified Operator & Developer CLI",
		},
		{
			name:       "help command",
			args:       []string{"walletctl", "help"},
			wantExit:   0,
			wantSubstr: "Available Commands:",
		},
		{
			name:       "help flag",
			args:       []string{"walletctl", "--help"},
			wantExit:   0,
			wantSubstr: "Available Commands:",
		},
		{
			name:       "version command",
			args:       []string{"walletctl", "version"},
			wantExit:   0,
			wantSubstr: "walletctl version",
		},
		{
			name:       "version flag",
			args:       []string{"walletctl", "--version"},
			wantExit:   0,
			wantSubstr: "walletctl version",
		},
		{
			name:       "unknown command",
			args:       []string{"walletctl", "invalid-subcmd"},
			wantExit:   1,
			wantSubstr: "Unknown command",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			errOut := &bytes.Buffer{}
			code := runCLI(tc.args, out, errOut)

			if code != tc.wantExit {
				t.Fatalf("expected exit code %d, got %d", tc.wantExit, code)
			}
			combined := out.String() + errOut.String()
			if !strings.Contains(combined, tc.wantSubstr) {
				t.Errorf("expected output to contain %q, got: %s", tc.wantSubstr, combined)
			}
		})
	}
}

func TestRunCLI_Health(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"UP"}`))
		case "/readyz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"READY"}`))
		case "/api/v1/cluster/status":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"current_routed_target":"PRIMARY","primary_region":{"address":"http://gw-primary:8080"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	t.Run("human readable health check", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{"walletctl", "health", "--gateway-url", ts.URL, "--timeout", "2s"}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected exit code 0, got %d. errOut: %s", code, errOut.String())
		}
		output := out.String()
		if !strings.Contains(output, "PRIMARY") {
			t.Errorf("expected output to mention PRIMARY, got: %s", output)
		}
		if !strings.Contains(output, "HEALTHY") {
			t.Errorf("expected output to have HEALTHY status, got: %s", output)
		}
	})

	t.Run("json output health check", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{"walletctl", "health", "--gateway-url", ts.URL, "--json"}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected exit code 0, got %d. errOut: %s", code, errOut.String())
		}

		var report fullHealthReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("failed to parse json output: %v, raw: %s", err, out.String())
		}
		if report.OverallStatus != "HEALTHY" {
			t.Errorf("expected HEALTHY status, got %s", report.OverallStatus)
		}
		if report.ActiveTarget != "PRIMARY" {
			t.Errorf("expected active target PRIMARY, got %s", report.ActiveTarget)
		}
		if len(report.Checks) < 2 {
			t.Errorf("expected at least 2 checks, got %d", len(report.Checks))
		}
	})

	t.Run("failing health probe returns exit code 1", func(t *testing.T) {
		failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"DOWN"}`))
		}))
		defer failServer.Close()

		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{"walletctl", "health", "--gateway-url", failServer.URL}

		code := runCLI(args, out, errOut)
		if code != 1 {
			t.Fatalf("expected exit code 1 for degraded cluster, got %d", code)
		}
	})
}

func TestRunCLI_Token(t *testing.T) {
	testSecret := "super-secure-test-secret-key-32-chars-long"

	t.Run("offline token minting plain output", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "token",
			"--subject", "usr_998877",
			"--roles", "user,operator",
			"--scopes", "wallet:read,wallet:transfer",
			"--secret", testSecret,
			"--ttl", "15m",
		}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}

		rawToken := strings.TrimSpace(out.String())
		if rawToken == "" {
			t.Fatal("expected non-empty token string")
		}

		claims, err := auth.ValidateToken(rawToken, testSecret)
		if err != nil {
			t.Fatalf("failed to validate minted token: %v", err)
		}
		if claims.Subject != "usr_998877" {
			t.Errorf("expected subject usr_998877, got %s", claims.Subject)
		}
		if len(claims.Roles) != 2 || claims.Roles[0] != "user" || claims.Roles[1] != "operator" {
			t.Errorf("unexpected roles: %v", claims.Roles)
		}
		if len(claims.Scopes) != 2 || claims.Scopes[0] != "wallet:read" {
			t.Errorf("unexpected scopes: %v", claims.Scopes)
		}
	})

	t.Run("offline token minting json output", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "token",
			"--subject", "usr_json_test",
			"--secret", testSecret,
			"--json",
		}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}

		var tokOut tokenOutput
		if err := json.Unmarshal(out.Bytes(), &tokOut); err != nil {
			t.Fatalf("failed to decode json token: %v, raw: %s", err, out.String())
		}
		if tokOut.Subject != "usr_json_test" {
			t.Errorf("expected subject usr_json_test, got %s", tokOut.Subject)
		}
		if tokOut.AccessToken == "" {
			t.Error("expected non-empty access token in json")
		}
	})

	t.Run("remote token minting via mock gateway", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auth/token" && r.Method == http.MethodPost {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"access_token":"mock_remote_jwt_token_123","token_type":"Bearer","expires_in":3600}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer ts.Close()

		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "token",
			"--remote",
			"--gateway-url", ts.URL,
			"--subject", "partner_corp",
		}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}
		if !strings.Contains(out.String(), "mock_remote_jwt_token_123") {
			t.Errorf("expected remote token in output, got: %s", out.String())
		}
	})
}

func TestRunCLI_Failover(t *testing.T) {
	var receivedFailoverReq map[string]string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cluster/status":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"current_routed_target":"PRIMARY","primary_region":{"address":"http://primary:8080"}}`))

		case "/api/v1/cluster/failover":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			authHdr := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHdr, "Bearer ") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&receivedFailoverReq)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"SUCCESS","previous_target":"PRIMARY","current_target":"STANDBY","message":"failover complete"}`))

		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	t.Run("query active failover status", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{"walletctl", "failover", "--gateway-url", ts.URL}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}
		if !strings.Contains(out.String(), "PRIMARY") {
			t.Errorf("expected output to mention PRIMARY, got: %s", out.String())
		}
	})

	t.Run("query active failover status json", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{"walletctl", "failover", "--gateway-url", ts.URL, "--json"}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}
		if !strings.Contains(out.String(), `"current_routed_target"`) {
			t.Errorf("expected json output, got: %s", out.String())
		}
	})

	t.Run("trigger failover to STANDBY", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "failover",
			"--gateway-url", ts.URL,
			"--target", "STANDBY",
		}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}
		if !strings.Contains(out.String(), "STANDBY") {
			t.Errorf("expected output to mention STANDBY, got: %s", out.String())
		}
		if receivedFailoverReq["target"] != "STANDBY" {
			t.Errorf("expected target STANDBY in request, got %s", receivedFailoverReq["target"])
		}
	})

	t.Run("invalid failover target returns error", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{"walletctl", "failover", "--target", "INVALID_REGION"}

		code := runCLI(args, out, errOut)
		if code != 1 {
			t.Fatalf("expected code 1 for invalid target, got %d", code)
		}
		if !strings.Contains(errOut.String(), "Invalid target") {
			t.Errorf("expected error message to mention invalid target, got: %s", errOut.String())
		}
	})
}

func TestRunCLI_LedgerVerify(t *testing.T) {
	t.Run("missing wallet-id returns error", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		code := runCLI([]string{"walletctl", "ledger", "verify"}, out, errOut)
		if code != 1 {
			t.Fatalf("expected code 1, got %d", code)
		}
		if !strings.Contains(errOut.String(), "--wallet-id is required") {
			t.Errorf("expected missing wallet-id message, got: %s", errOut.String())
		}
	})

	t.Run("unknown ledger subcommand returns error", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		code := runCLI([]string{"walletctl", "ledger", "delete"}, out, errOut)
		if code != 1 {
			t.Fatalf("expected code 1, got %d", code)
		}
	})

	t.Run("valid ledger audit chain", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := ledgerResponseDTO{
				WalletID:   "w_audit_test",
				TotalCount: 2,
				Entries: []ledgerEntryDTO{
					{
						TransactionID:  "tx_1",
						SequenceNumber: 1,
						Hash:           "hash_1",
						PreviousHash:   "",
						Postings: []ledgerPostingDTO{
							{AccountID: "w_audit_test", Direction: "DEBIT", Amount: 1000, Currency: "USD"},
							{AccountID: "w_peer", Direction: "CREDIT", Amount: 1000, Currency: "USD"},
						},
					},
					{
						TransactionID:  "tx_2",
						SequenceNumber: 2,
						Hash:           "hash_2",
						PreviousHash:   "hash_1",
						Postings: []ledgerPostingDTO{
							{AccountID: "w_audit_test", Direction: "CREDIT", Amount: 500, Currency: "USD"},
							{AccountID: "w_peer", Direction: "DEBIT", Amount: 500, Currency: "USD"},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer ts.Close()

		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "ledger", "verify",
			"--gateway-url", ts.URL,
			"--wallet-id", "w_audit_test",
		}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}
		output := out.String()
		if !strings.Contains(output, "Chain Status:     VALID") {
			t.Errorf("expected VALID chain status, got: %s", output)
		}
		if !strings.Contains(output, "GAAP Balanced:    true") {
			t.Errorf("expected balance equilibrium, got: %s", output)
		}
	})

	t.Run("broken ledger hash chain is detected", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := ledgerResponseDTO{
				WalletID:   "w_tampered",
				TotalCount: 2,
				Entries: []ledgerEntryDTO{
					{
						TransactionID:  "tx_1",
						SequenceNumber: 1,
						Hash:           "legit_hash_1",
						PreviousHash:   "",
						Postings:       []ledgerPostingDTO{},
					},
					{
						TransactionID:  "tx_2",
						SequenceNumber: 2,
						Hash:           "legit_hash_2",
						PreviousHash:   "corrupted_tampered_hash", // Broken link!
						Postings:       []ledgerPostingDTO{},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer ts.Close()

		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "ledger", "verify",
			"--gateway-url", ts.URL,
			"--wallet-id", "w_tampered",
		}

		code := runCLI(args, out, errOut)
		if code != 1 {
			t.Fatalf("expected code 1 for broken chain, got %d", code)
		}
		output := out.String()
		if !strings.Contains(output, "BROKEN_CHAIN") {
			t.Errorf("expected BROKEN_CHAIN status, got: %s", output)
		}
	})

	t.Run("direct ledger service audit verify endpoint", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/audit/verify") {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"VALID","entries_verified":10}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer ts.Close()

		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "ledger", "verify",
			"--ledger-url", ts.URL,
			"--wallet-id", "w_direct_test",
		}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}
	})
}

func TestRunCLI_WebhookPing(t *testing.T) {
	testSecret := "whsec_test_secret_for_unit_tests_99"

	t.Run("webhook help and missing args", func(t *testing.T) {
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		code := runCLI([]string{"walletctl", "webhook", "--help"}, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0 for help, got %d", code)
		}

		code = runCLI([]string{"walletctl", "webhook", "unknown"}, out, errOut)
		if code != 1 {
			t.Fatalf("expected code 1 for unknown subcommand, got %d", code)
		}

		code = runCLI([]string{"walletctl", "webhook", "ping"}, out, errOut)
		if code != 1 {
			t.Fatalf("expected code 1 for missing --url, got %d", code)
		}
	})

	t.Run("successful webhook ping probe with signature verification", func(t *testing.T) {
		var receivedSig string
		var receivedBody []byte
		var receivedEvent string
		var receivedDelivery string

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedSig = r.Header.Get("X-Wallet-Signature")
			receivedEvent = r.Header.Get("X-Wallet-Event")
			receivedDelivery = r.Header.Get("X-Wallet-Delivery")
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r.Body)
			receivedBody = buf.Bytes()

			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"received":true}`))
		}))
		defer ts.Close()

		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "webhook", "ping",
			"--url", ts.URL,
			"--secret", testSecret,
			"--event", "wallet.balance_updated",
		}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}

		// Verify HTTP headers set by CLI
		if receivedEvent != "wallet.balance_updated" {
			t.Errorf("expected event wallet.balance_updated, got %s", receivedEvent)
		}
		if !strings.HasPrefix(receivedDelivery, "whping_") {
			t.Errorf("expected delivery id starting with whping_, got %s", receivedDelivery)
		}

		// Verify cryptographic signature matches payload and secret
		tsHeader, hexSig, err := webhook.ParseSignatureHeader(receivedSig)
		if err != nil {
			t.Fatalf("failed to parse signature header: %v", err)
		}
		expectedHex := webhook.ComputeSignature(testSecret, tsHeader, receivedBody)
		if hexSig != expectedHex {
			t.Errorf("signature mismatch: expected %s, got %s", expectedHex, hexSig)
		}

		output := out.String()
		if !strings.Contains(output, "[PASS]") {
			t.Errorf("expected [PASS] in output, got: %s", output)
		}
	})

	t.Run("webhook ping json report", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"accepted":true}`))
		}))
		defer ts.Close()

		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "webhook", "ping",
			"--url", ts.URL,
			"--secret", testSecret,
			"--json",
		}

		code := runCLI(args, out, errOut)
		if code != 0 {
			t.Fatalf("expected code 0, got %d. errOut: %s", code, errOut.String())
		}

		var report webhookPingReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("failed to decode json report: %v, raw: %s", err, out.String())
		}
		if !report.Success {
			t.Errorf("expected success true, got false")
		}
		if report.StatusCode != http.StatusAccepted {
			t.Errorf("expected status 202, got %d", report.StatusCode)
		}
		if report.Signature == "" {
			t.Error("expected non-empty signature")
		}
	})

	t.Run("webhook ping failure report on 500 error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"partner internal crash"}`))
		}))
		defer ts.Close()

		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		args := []string{
			"walletctl", "webhook", "ping",
			"--url", ts.URL,
		}

		code := runCLI(args, out, errOut)
		if code != 1 {
			t.Fatalf("expected code 1 on partner failure, got %d", code)
		}
		if !strings.Contains(out.String(), "[FAIL]") {
			t.Errorf("expected [FAIL] in output, got: %s", out.String())
		}
	})
}
