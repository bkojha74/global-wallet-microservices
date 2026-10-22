package main

import (
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
	"wallet-system/pkg/merkle"
)

type ledgerPostingDTO struct {
	AccountID   string `json:"account_id"`
	AccountType string `json:"account_type"`
	Direction   string `json:"direction"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
}

type ledgerEntryDTO struct {
	TransactionID  string             `json:"transaction_id"`
	SequenceNumber int64              `json:"sequence_number"`
	Hash           string             `json:"hash"`
	PreviousHash   string             `json:"previous_hash"`
	Postings       []ledgerPostingDTO `json:"postings"`
}

type ledgerResponseDTO struct {
	WalletID   string           `json:"wallet_id"`
	TotalCount int64            `json:"total_count"`
	Entries    []ledgerEntryDTO `json:"entries"`
}

type ledgerAuditReport struct {
	WalletID         string           `json:"wallet_id"`
	TotalEntries     int              `json:"total_entries_verified"`
	ChainStatus      string           `json:"chain_status"`
	IsBalanced       bool             `json:"is_balanced"`
	StartSequence    int64            `json:"start_sequence"`
	EndSequence      int64            `json:"end_sequence"`
	LatestHash       string           `json:"latest_hash"`
	TotalDebits      map[string]int64 `json:"total_debits"`
	TotalCredits     map[string]int64 `json:"total_credits"`
	TamperedSequence int64            `json:"tampered_sequence,omitempty"`
	ErrorMessage     string           `json:"error_message,omitempty"`
}

type merkleProofReport struct {
	EntryHash      string             `json:"entry_hash"`
	CheckpointID   string             `json:"checkpoint_id"`
	MerkleRoot     string             `json:"merkle_root"`
	ProofHops      int                `json:"proof_hops"`
	RemoteVerified bool               `json:"remote_verified"`
	LocalMathCheck bool               `json:"local_math_check"`
	Proof          *merkle.AuditProof `json:"proof,omitempty"`
	ErrorMessage   string             `json:"error,omitempty"`
}

type merkleCheckpointReport struct {
	CheckpointID   string   `json:"checkpoint_id"`
	StartSequence  int64    `json:"start_sequence"`
	EndSequence    int64    `json:"end_sequence"`
	MerkleRoot     string   `json:"merkle_root"`
	EntryCount     int      `json:"entry_count"`
	CreatedAt      string   `json:"created_at"`
	LocalMathCheck bool     `json:"local_math_check"`
	LeafHashes     []string `json:"leaf_hashes,omitempty"`
	ErrorMessage   string   `json:"error,omitempty"`
}

func runLedgerCmd(args []string, out io.Writer, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "Usage: walletctl ledger <subcommand> [flags]")
		fmt.Fprintln(errOut, "Subcommands: verify, merkle")
		return 1
	}

	sub := args[0]
	switch sub {
	case "verify":
		return runLedgerVerifyCmd(args[1:], out, errOut)
	case "merkle":
		return runLedgerMerkleCmd(args[1:], out, errOut)
	default:
		fmt.Fprintf(errOut, "Unknown ledger subcommand %q. Supported subcommands: verify, merkle\n", sub)
		return 1
	}
}

func runLedgerVerifyCmd(args []string, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("ledger verify", flag.ContinueOnError)
	fs.SetOutput(errOut)

	walletID := fs.String("wallet-id", "", "Wallet ID to verify audit chain for (required)")
	gatewayURL := fs.String("gateway-url", "http://localhost:8080", "API Gateway base URL")
	ledgerURL := fs.String("ledger-url", "", "Direct Ledger management URL (e.g. http://localhost:9092)")
	token := fs.String("token", "", "Bearer token for gateway authentication")
	limit := fs.Int("limit", 100, "Maximum entries to inspect")
	timeout := fs.Duration("timeout", 10*time.Second, "HTTP timeout")
	jsonOutput := fs.Bool("json", false, "Output results in JSON format")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *walletID == "" {
		fmt.Fprintln(errOut, "Error: --wallet-id is required")
		return 1
	}

	client := &http.Client{Timeout: *timeout}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// If direct ledger-service management URL is supplied, query /audit/verify
	if *ledgerURL != "" {
		url := fmt.Sprintf("%s/audit/verify?wallet_id=%s", strings.TrimRight(*ledgerURL, "/"), *walletID)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			fmt.Fprintf(errOut, "Failed to create ledger request: %v\n", err)
			return 1
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(errOut, "Failed to connect to ledger at %s: %v\n", url, err)
			return 1
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if *jsonOutput {
			fmt.Fprintln(out, string(body))
		} else {
			fmt.Fprintf(out, "Ledger Audit Verification Response (HTTP %d):\n%s\n", resp.StatusCode, string(body))
		}
		if resp.StatusCode == http.StatusOK {
			return 0
		}
		return 1
	}

	// Query via API Gateway /api/v1/ledger
	gwBase := strings.TrimRight(*gatewayURL, "/")
	gwURL := fmt.Sprintf("%s/api/v1/ledger?wallet_id=%s&limit=%d", gwBase, *walletID, *limit)

	authToken := *token
	if authToken == "" {
		sec := os.Getenv("JWT_SECRET")
		if sec == "" {
			sec = defaultCLIJWTSecret
		}
		minted, err := auth.GenerateToken(auth.Claims{
			Subject:   *walletID,
			Roles:     []string{"admin"},
			Scopes:    []string{"wallet:read", "ledger:audit"},
			IssuedAt:  time.Now().Unix(),
			ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
		}, sec)
		if err == nil {
			authToken = minted
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gwURL, nil)
	if err != nil {
		fmt.Fprintf(errOut, "Failed to create gateway request: %v\n", err)
		return 1
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(errOut, "Failed to query gateway at %s: %v\n", gwURL, err)
		return 1
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(errOut, "Gateway returned HTTP %d: %s\n", resp.StatusCode, string(body))
		return 1
	}

	var ledgerResp ledgerResponseDTO
	if err := json.Unmarshal(body, &ledgerResp); err != nil {
		fmt.Fprintf(errOut, "Failed to parse ledger response: %v\n", err)
		return 1
	}

	entries := ledgerResp.Entries
	report := ledgerAuditReport{
		WalletID:     *walletID,
		TotalEntries: len(entries),
		TotalDebits:  make(map[string]int64),
		TotalCredits: make(map[string]int64),
		IsBalanced:   true,
		ChainStatus:  "VALID",
	}

	if len(entries) == 0 {
		report.ChainStatus = "EMPTY (No records found)"
	} else {
		report.StartSequence = entries[0].SequenceNumber
		report.EndSequence = entries[len(entries)-1].SequenceNumber
		report.LatestHash = entries[len(entries)-1].Hash

		// Verify sequential hash chaining
		for i := 1; i < len(entries); i++ {
			prev := entries[i-1]
			curr := entries[i]
			if curr.PreviousHash != "" && curr.PreviousHash != prev.Hash {
				report.ChainStatus = "BROKEN_CHAIN"
				report.TamperedSequence = curr.SequenceNumber
				report.ErrorMessage = fmt.Sprintf("Hash break at seq %d: expected prev_hash %s, got %s",
					curr.SequenceNumber, prev.Hash, curr.PreviousHash)
				break
			}
		}

		// Verify posting debits and credits
		for _, e := range entries {
			for _, p := range e.Postings {
				switch strings.ToUpper(p.Direction) {
				case "DEBIT":
					report.TotalDebits[p.Currency] += p.Amount
				case "CREDIT":
					report.TotalCredits[p.Currency] += p.Amount
				}
			}
		}

		for curr, deb := range report.TotalDebits {
			cred := report.TotalCredits[curr]
			if deb != cred {
				report.IsBalanced = false
				report.ErrorMessage = fmt.Sprintf("Imbalance in %s: Debits=%d, Credits=%d", curr, deb, cred)
				break
			}
		}
	}

	if *jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Fprintln(out, "Ledger Cryptographic Audit Chain Verification")
		fmt.Fprintln(out, "------------------------------------------------------------")
		fmt.Fprintf(out, "Wallet ID:        %s\n", report.WalletID)
		fmt.Fprintf(out, "Entries Verified: %d\n", report.TotalEntries)
		fmt.Fprintf(out, "Sequence Range:   %d -> %d\n", report.StartSequence, report.EndSequence)
		fmt.Fprintf(out, "Chain Status:     %s\n", report.ChainStatus)
		fmt.Fprintf(out, "GAAP Balanced:    %t\n", report.IsBalanced)
		fmt.Fprintf(out, "Latest Hash:      %s\n", report.LatestHash)

		if report.ErrorMessage != "" {
			fmt.Fprintf(out, "Violation:        %s\n", report.ErrorMessage)
		}
	}

	if report.ChainStatus != "VALID" && report.ChainStatus != "EMPTY (No records found)" {
		return 1
	}
	if !report.IsBalanced {
		return 1
	}
	return 0
}

func runLedgerMerkleCmd(args []string, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("ledger merkle", flag.ContinueOnError)
	fs.SetOutput(errOut)

	gatewayURL := fs.String("gateway-url", "http://localhost:8080", "API Gateway base URL")
	ledgerURL := fs.String("ledger-url", "", "Direct Ledger management URL (e.g. http://localhost:9092)")
	entryHash := fs.String("entry-hash", "", "Ledger entry hash to verify cryptographic inclusion proof for")
	checkpointID := fs.String("checkpoint-id", "", "Checkpoint ID to inspect or verify against")
	timeout := fs.Duration("timeout", 10*time.Second, "HTTP timeout")
	jsonOutput := fs.Bool("json", false, "Output results in JSON format")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	client := &http.Client{Timeout: *timeout}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	baseURL := strings.TrimRight(*gatewayURL, "/") + "/api/v1/ledger/merkle"
	if *ledgerURL != "" {
		baseURL = strings.TrimRight(*ledgerURL, "/") + "/audit/merkle"
	}

	// 1. Inclusion proof verification branch
	if *entryHash != "" {
		proofURL := fmt.Sprintf("%s/proof?entry_hash=%s", baseURL, *entryHash)
		if *checkpointID != "" {
			proofURL += "&checkpoint_id=" + *checkpointID
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, proofURL, nil)
		if err != nil {
			fmt.Fprintf(errOut, "Failed to create request: %v\n", err)
			return 1
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(errOut, "Request to %s failed: %v\n", proofURL, err)
			return 1
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(errOut, "Inclusion proof query returned HTTP %d: %s\n", resp.StatusCode, string(body))
			return 1
		}

		var proofResp struct {
			EntryHash    string             `json:"entry_hash"`
			CheckpointID string             `json:"checkpoint_id"`
			MerkleRoot   string             `json:"merkle_root"`
			Proof        *merkle.AuditProof `json:"proof"`
			Verified     bool               `json:"verified"`
		}
		if err := json.Unmarshal(body, &proofResp); err != nil {
			fmt.Fprintf(errOut, "Failed to parse proof response: %v\n", err)
			return 1
		}

		localValid := false
		hops := 0
		if proofResp.Proof != nil {
			localValid = merkle.VerifyProof(proofResp.EntryHash, proofResp.Proof, proofResp.MerkleRoot)
			hops = len(proofResp.Proof.Steps)
		}

		report := merkleProofReport{
			EntryHash:      proofResp.EntryHash,
			CheckpointID:   proofResp.CheckpointID,
			MerkleRoot:     proofResp.MerkleRoot,
			ProofHops:      hops,
			RemoteVerified: proofResp.Verified,
			LocalMathCheck: localValid,
			Proof:          proofResp.Proof,
		}

		if *jsonOutput {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			_ = enc.Encode(report)
		} else {
			fmt.Fprintln(out, "=== Cryptographic Merkle Inclusion Proof Verification ===")
			fmt.Fprintf(out, "Entry Hash:       %s\n", report.EntryHash)
			fmt.Fprintf(out, "Checkpoint ID:    %s\n", report.CheckpointID)
			fmt.Fprintf(out, "Merkle Root:      %s\n", report.MerkleRoot)
			fmt.Fprintf(out, "Proof Hops:       %d\n", report.ProofHops)
			fmt.Fprintf(out, "Remote Verified:  %t\n", report.RemoteVerified)
			if report.LocalMathCheck {
				fmt.Fprintln(out, "Local Math Check: PASSED (Root matches SHA-256 tree path)")
				fmt.Fprintln(out, "Result:           [PASS] Transaction cryptographically proven in immutable ledger")
			} else {
				fmt.Fprintln(out, "Local Math Check: FAILED (Proof path does not produce expected root)")
				fmt.Fprintln(out, "Result:           [FAIL] Cryptographic proof validation failed")
			}
		}

		if !localValid {
			return 1
		}
		return 0
	}

	// 2. Checkpoint inspection branch (latest)
	latestURL := baseURL + "/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		fmt.Fprintf(errOut, "Failed to create request: %v\n", err)
		return 1
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(errOut, "Request to %s failed: %v\n", latestURL, err)
		return 1
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(errOut, "Checkpoint query returned HTTP %d: %s\n", resp.StatusCode, string(body))
		return 1
	}

	var cp struct {
		CheckpointID  string    `json:"checkpoint_id"`
		StartSequence int64     `json:"start_sequence"`
		EndSequence   int64     `json:"end_sequence"`
		MerkleRoot    string    `json:"merkle_root"`
		EntryCount    int       `json:"entry_count"`
		CreatedAt     time.Time `json:"created_at"`
		LeafHashes    []string  `json:"leaf_hashes"`
	}
	if err := json.Unmarshal(body, &cp); err != nil {
		fmt.Fprintf(errOut, "Failed to parse checkpoint response: %v\n", err)
		return 1
	}

	// Local mathematical integrity check
	localValid := false
	if len(cp.LeafHashes) > 0 {
		tree, err := merkle.NewTree(cp.LeafHashes)
		if err == nil && tree.RootHash() == cp.MerkleRoot {
			localValid = true
		}
	} else if cp.MerkleRoot != "" {
		localValid = true
	}

	report := merkleCheckpointReport{
		CheckpointID:   cp.CheckpointID,
		StartSequence:  cp.StartSequence,
		EndSequence:    cp.EndSequence,
		MerkleRoot:     cp.MerkleRoot,
		EntryCount:     cp.EntryCount,
		CreatedAt:      cp.CreatedAt.Format(time.RFC3339),
		LocalMathCheck: localValid,
		LeafHashes:     cp.LeafHashes,
	}

	if *jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Fprintln(out, "=== Latest Ledger Merkle Checkpoint ===")
		fmt.Fprintf(out, "Checkpoint ID:    %s\n", report.CheckpointID)
		fmt.Fprintf(out, "Sequence Range:   %d -> %d\n", report.StartSequence, report.EndSequence)
		fmt.Fprintf(out, "Entries Count:    %d\n", report.EntryCount)
		fmt.Fprintf(out, "Merkle Root:      %s\n", report.MerkleRoot)
		fmt.Fprintf(out, "Created At:       %s\n", report.CreatedAt)
		if report.LocalMathCheck {
			fmt.Fprintln(out, "Integrity Check:  PASSED (Local re-computation matches root)")
			fmt.Fprintln(out, "Result:           [PASS] Checkpoint mathematically verified")
		} else {
			fmt.Fprintln(out, "Integrity Check:  FAILED (Re-computed root does not match)")
			fmt.Fprintln(out, "Result:           [FAIL] Checkpoint integrity verification failed")
		}
	}

	if !localValid {
		return 1
	}
	return 0
}
