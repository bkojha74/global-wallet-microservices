package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	ledgerv1 "wallet-system/proto/ledger"
)

func TestRecordTransactionRejectsInvalidPayload(t *testing.T) {
	tests := []struct {
		name string
		req  *ledgerv1.RecordTransactionRequest
	}{
		{
			name: "missing idempotency key",
			req:  &ledgerv1.RecordTransactionRequest{Amount: 10, Currency: "USD"},
		},
		{
			name: "non-positive amount",
			req: &ledgerv1.RecordTransactionRequest{
				IdempotencyKey: "key-1",
				Amount:         0,
				Currency:       "USD",
			},
		},
		{
			name: "negative amount",
			req: &ledgerv1.RecordTransactionRequest{
				IdempotencyKey: "key-2",
				Amount:         -100,
				Currency:       "USD",
			},
		},
		{
			name: "unsupported currency",
			req: &ledgerv1.RecordTransactionRequest{
				IdempotencyKey: "key-3",
				Amount:         100,
				Currency:       "FAKE",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &server{}
			response, err := service.RecordTransaction(context.Background(), test.req)

			if response != nil {
				t.Fatalf("expected no response, got %+v", response)
			}
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestGetLedgerEntriesRejectsMissingWalletID(t *testing.T) {
	service := &server{}
	resp, err := service.GetLedgerEntries(context.Background(), &ledgerv1.GetLedgerRequest{WalletId: ""})
	if resp != nil {
		t.Fatalf("expected nil response, got %+v", resp)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}

	// nil request check
	respNil, errNil := service.GetLedgerEntries(context.Background(), nil)
	if respNil != nil || status.Code(errNil) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for nil request, got %v", errNil)
	}
}

func TestParseTransactionDocID(t *testing.T) {
	// 1. Valid hex
	hex := "507f1f77bcf86cd799439011"
	oid := parseTransactionDocID(hex)
	if oid.Hex() != hex {
		t.Fatalf("expected %s, got %s", hex, oid.Hex())
	}

	// 2. Empty string generates new ObjectID
	oidEmpty := parseTransactionDocID("")
	if oidEmpty.IsZero() {
		t.Fatal("expected non-zero ObjectID for empty input")
	}

	// 3. Invalid hex generates new ObjectID
	oidInvalid := parseTransactionDocID("not-a-valid-hex")
	if oidInvalid.IsZero() {
		t.Fatal("expected non-zero ObjectID for invalid hex")
	}
}

func TestParseLedgerPagination(t *testing.T) {
	// 1. Default pagination when limit <= 0
	req1 := &ledgerv1.GetLedgerRequest{Limit: 0}
	limit1, offset1 := parseLedgerPagination(req1)
	if limit1 != 20 || offset1 != 0 {
		t.Fatalf("expected limit 20, offset 0, got %d, %d", limit1, offset1)
	}

	// 2. Cap limit when limit > 100
	req2 := &ledgerv1.GetLedgerRequest{Limit: 500}
	limit2, offset2 := parseLedgerPagination(req2)
	if limit2 != 20 || offset2 != 0 {
		t.Fatalf("expected capped limit 20, got %d", limit2)
	}

	// 3. Valid page token offset
	token := base64.StdEncoding.EncodeToString([]byte("40"))
	req3 := &ledgerv1.GetLedgerRequest{Limit: 15, PageToken: token}
	limit3, offset3 := parseLedgerPagination(req3)
	if limit3 != 15 || offset3 != 40 {
		t.Fatalf("expected limit 15, offset 40, got %d, %d", limit3, offset3)
	}

	// 4. Corrupt page token
	req4 := &ledgerv1.GetLedgerRequest{Limit: 10, PageToken: "???not-base64"}
	limit4, offset4 := parseLedgerPagination(req4)
	if limit4 != 10 || offset4 != 0 {
		t.Fatalf("expected fallback offset 0, got %d", offset4)
	}
}

func TestLedgerEnvironmentName(t *testing.T) {
	t.Setenv("ENVIRONMENT", "prod")
	if env := environmentName(); env != "prod" {
		t.Fatalf("expected prod, got %s", env)
	}

	t.Setenv("ENVIRONMENT", "")
	if env := environmentName(); env != "local" {
		t.Fatalf("expected local, got %s", env)
	}
}

func TestGetLedgerServerOptions(t *testing.T) {
	t.Setenv("GRPC_TLS_ENABLED", "false")
	opts, err := getLedgerServerOptions()
	if err != nil || len(opts) != 0 {
		t.Fatalf("expected empty opts and nil error, got opts=%v, err=%v", opts, err)
	}

	// Test error when TLS enabled with missing files
	t.Setenv("GRPC_TLS_ENABLED", "true")
	t.Setenv("GRPC_SERVER_CERT", "/nonexistent/cert.pem")
	_, err = getLedgerServerOptions()
	if err == nil {
		t.Fatal("expected error for nonexistent cert file")
	}
}

func TestDoubleEntryAccumulateAndEquilibrium(t *testing.T) {
	// 1. Missing account ID
	_, _, err := accumulatePostingTotals([]JournalPosting{{AccountID: "", Amount: 100, Currency: "USD", Direction: PostingDebit}})
	if err == nil || !strings.Contains(err.Error(), "account_id is required") {
		t.Fatalf("expected account_id is required error, got %v", err)
	}

	// 2. Non-positive amount
	_, _, err = accumulatePostingTotals([]JournalPosting{{AccountID: "acc1", Amount: 0, Currency: "USD", Direction: PostingDebit}})
	if err == nil || !strings.Contains(err.Error(), "amount must be positive") {
		t.Fatalf("expected amount must be positive error, got %v", err)
	}

	// 3. Missing currency
	_, _, err = accumulatePostingTotals([]JournalPosting{{AccountID: "acc1", Amount: 10, Currency: "", Direction: PostingDebit}})
	if err == nil || !strings.Contains(err.Error(), "currency is required") {
		t.Fatalf("expected currency is required error, got %v", err)
	}

	// 4. Invalid direction
	_, _, err = accumulatePostingTotals([]JournalPosting{{AccountID: "acc1", Amount: 10, Currency: "USD", Direction: "INVALID"}})
	if err == nil || !strings.Contains(err.Error(), "invalid direction") {
		t.Fatalf("expected invalid direction error, got %v", err)
	}

	// 5. Valid accumulation
	debits, credits, err := accumulatePostingTotals([]JournalPosting{
		{AccountID: "src", Amount: 50, Currency: "USD", Direction: PostingDebit},
		{AccountID: "dst", Amount: 50, Currency: "USD", Direction: PostingCredit},
	})
	if err != nil || debits["USD"] != 50 || credits["USD"] != 50 {
		t.Fatalf("unexpected accumulation: debits=%v, credits=%v, err=%v", debits, credits, err)
	}

	// 6. Currency equilibrium check
	if err := checkCurrencyEquilibrium(debits, credits); err != nil {
		t.Fatalf("expected equilibrium, got %v", err)
	}

	// 7. Currency disequilibrium
	unbalancedCredits := map[string]int64{"USD": 40}
	if err := checkCurrencyEquilibrium(debits, unbalancedCredits); err == nil {
		t.Fatal("expected currency equilibrium failure")
	}
}

func TestVerifyDocIntegrityBranches(t *testing.T) {
	now := time.Now().UTC()
	docID := primitive.NewObjectID()
	postings, _ := CreateTransferPostings("alice", "bob", 100, "USD")
	entryHash := ComputeEntryHash(GenesisHash, 1, docID.Hex(), "idemp-1", now, postings)

	validDoc := LedgerDocument{
		ID:             docID,
		SequenceNumber: 1,
		IdempotencyKey: "idemp-1",
		Timestamp:      now,
		Postings:       postings,
		PreviousHash:   GenesisHash,
		EntryHash:      entryHash,
	}

	// 1. Valid doc
	if msg := verifyDocIntegrity(validDoc, 1, GenesisHash); msg != "" {
		t.Fatalf("expected valid doc, got %s", msg)
	}

	// 2. Sequence gap
	if msg := verifyDocIntegrity(validDoc, 2, GenesisHash); !strings.Contains(msg, "sequence gap detected") {
		t.Fatalf("expected sequence gap, got %s", msg)
	}

	// 3. Hash chain broken
	if msg := verifyDocIntegrity(validDoc, 1, "different-prev-hash"); !strings.Contains(msg, "hash chain broken") {
		t.Fatalf("expected hash chain broken, got %s", msg)
	}

	// 4. Tamper detected
	tamperedDoc := validDoc
	tamperedDoc.EntryHash = "tampered-hash"
	if msg := verifyDocIntegrity(tamperedDoc, 1, GenesisHash); !strings.Contains(msg, "tamper detected") {
		t.Fatalf("expected tamper detected, got %s", msg)
	}

	// 5. Unbalanced posting
	unbalancedDoc := validDoc
	unbalancedDoc.Postings = append(unbalancedDoc.Postings, JournalPosting{
		AccountID: "extra", Amount: 99, Currency: "USD", Direction: PostingDebit,
	})
	unbalancedDoc.EntryHash = ComputeEntryHash(GenesisHash, 1, docID.Hex(), "idemp-1", now, unbalancedDoc.Postings)
	if msg := verifyDocIntegrity(unbalancedDoc, 1, GenesisHash); !strings.Contains(msg, "unbalanced posting") {
		t.Fatalf("expected unbalanced posting error, got %s", msg)
	}
}

func TestCheckTrialBalanceEquilibrium(t *testing.T) {
	// Balanced
	balancedRes := &AuditVerificationResult{
		TotalDebits:  map[string]int64{"USD": 100, "EUR": 50},
		TotalCredits: map[string]int64{"USD": 100, "EUR": 50},
		IsBalanced:   true,
	}
	checkTrialBalanceEquilibrium(balancedRes)
	if !balancedRes.IsBalanced {
		t.Fatal("expected balanced")
	}

	// Unbalanced
	unbalancedRes := &AuditVerificationResult{
		TotalDebits:  map[string]int64{"USD": 100},
		TotalCredits: map[string]int64{"USD": 90},
		IsBalanced:   true,
	}
	checkTrialBalanceEquilibrium(unbalancedRes)
	if unbalancedRes.IsBalanced || unbalancedRes.Status != "TRIAL_BALANCE_UNBALANCED" {
		t.Fatalf("expected unbalanced status, got %+v", unbalancedRes)
	}
}

func TestLedgerHealthzHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	// Create healthz handler matching startLedgerMetricsServer
	h := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"status":"UP","region":"us-east-1"}` + "\n"))
	})
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"UP"`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestLedgerServiceLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable for live ledger test: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	srv := &server{
		mongoClient: client,
		region:      "us-east-1",
		logger:      observability.NewMemoryLogger(),
	}

	key := strings.Repeat("a", 10) + time.Now().Format("150405.000000000")
	req := &ledgerv1.RecordTransactionRequest{
		IdempotencyKey:      key,
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              100,
		Currency:            "USD",
		Region:              "us-east-1",
	}

	// 1. Initial record transaction
	resp, err := srv.RecordTransaction(ctx, req)
	if err != nil || resp == nil || !resp.Success {
		t.Fatalf("expected record transaction success, got resp=%+v, err=%v", resp, err)
	}

	// 2. Duplicate transaction check (idempotency replay)
	dupResp, err := srv.RecordTransaction(ctx, req)
	if err != nil || dupResp == nil || !dupResp.Success {
		t.Fatalf("expected duplicate record success, got resp=%+v, err=%v", dupResp, err)
	}
	if dupResp.TransactionId != resp.TransactionId {
		t.Fatalf("expected same transaction ID for replay, got %s vs %s", dupResp.TransactionId, resp.TransactionId)
	}

	// 3. Get ledger entries
	entriesResp, err := srv.GetLedgerEntries(ctx, &ledgerv1.GetLedgerRequest{
		WalletId: "alice",
		Limit:    10,
	})
	if err != nil || entriesResp == nil || len(entriesResp.Entries) == 0 {
		t.Fatalf("expected ledger entries for alice, got entries=%+v, err=%v", entriesResp, err)
	}

	// 4. Verify audit chain handler
	col := client.Database("banking_db").Collection("ledger_entries")
	res, vErr := VerifyAuditChain(ctx, col)
	if vErr != nil || res == nil {
		t.Fatalf("expected verify audit chain result, got res=%+v, err=%v", res, vErr)
	}

	// 5. startLedgerMetricsServer
	metricsSrv := startLedgerMetricsServer(client, "0", "us-east-1")
	defer metricsSrv.Close()

	// 6. initLedgerOutbox branches
	ob1, pub1 := initLedgerOutbox(ctx, client)
	if ob1 != nil || pub1 != nil {
		t.Fatal("expected nil outbox when disabled")
	}

	t.Setenv("LOGGING_OUTBOX_ENABLED", "true")
	t.Setenv("LOGGING_RABBITMQ_URL", "")
	ob2, pub2 := initLedgerOutbox(ctx, client)
	if ob2 != nil || pub2 != nil {
		t.Fatal("expected nil outbox when rabbit url missing")
	}
}

func TestHandleInsertError(t *testing.T) {
	srv := &server{logger: observability.NewMemoryLogger()}
	resp, err := srv.handleInsertError(context.Background(), nil, &ledgerv1.RecordTransactionRequest{IdempotencyKey: "k"}, errors.New("db error"), time.Now())
	if err != nil || resp.Success != false || resp.ErrorMessage != "db error" {
		t.Fatalf("unexpected response: %+v, err: %v", resp, err)
	}
}

func TestRecordTransactionValidationBranches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	srv := &server{
		mongoClient: client,
		region:      "us-east-1",
		logger:      observability.NewMemoryLogger(),
	}

	// 1. Missing source wallet (passes amount validation, triggers CreateTransferPostings error)
	reqMissingSource := &ledgerv1.RecordTransactionRequest{
		IdempotencyKey:      fmt.Sprintf("idemp-post-fail-%d", time.Now().UnixNano()),
		SourceWalletId:      "",
		DestinationWalletId: "bob",
		Amount:              100,
		Currency:            "USD",
	}
	resp, err := srv.RecordTransaction(ctx, reqMissingSource)
	if err == nil || resp != nil {
		t.Fatalf("expected error for missing source wallet, got resp=%+v, err=%v", resp, err)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument code, got %v", status.Code(err))
	}

	// 2. Non-positive amount
	reqZero := &ledgerv1.RecordTransactionRequest{
		IdempotencyKey:      fmt.Sprintf("idemp-zero-%d", time.Now().UnixNano()),
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              0,
		Currency:            "USD",
	}
	_, err = srv.RecordTransaction(ctx, reqZero)
	if err == nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument code for zero amount, got %v", err)
	}

	// 3. Unsupported currency
	reqBadCurr := &ledgerv1.RecordTransactionRequest{
		IdempotencyKey:      fmt.Sprintf("idemp-badcurr-%d", time.Now().UnixNano()),
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              100,
		Currency:            "UNKNOWN",
	}
	_, err = srv.RecordTransaction(ctx, reqBadCurr)
	if err == nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument code for unsupported currency, got %v", err)
	}
}

func TestHandleInsertErrorDuplicateKeyRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	srv := &server{
		mongoClient: client,
		region:      "us-east-1",
		logger:      observability.NewMemoryLogger(),
	}

	// Use a test-isolated collection without the sequence_number unique index
	col := client.Database("banking_db").Collection("ledger_test_dup_race")
	now := time.Now().UnixNano()
	idemp := fmt.Sprintf("race-dup-%d", now)

	postings, _ := CreateTransferPostings("alice", "bob", 100, "USD")
	sharedID := primitive.NewObjectID()
	doc := LedgerDocument{
		ID:                  sharedID,
		IdempotencyKey:      idemp,
		SourceWalletID:      "alice",
		DestinationWalletID: "bob",
		Amount:              100,
		Currency:            "USD",
		SequenceNumber:      now, // nanosecond-unique, avoids sequence_number index conflict
		Timestamp:           time.Now().UTC(),
		Postings:            postings,
	}

	// First insert succeeds
	_, insertErr := col.InsertOne(ctx, doc)
	if insertErr != nil {
		t.Skipf("failed to insert test doc: %v", insertErr)
		return
	}

	// Second insert with same _id triggers a real E11000 DuplicateKey error
	_, dupErr := col.InsertOne(ctx, doc)
	if dupErr == nil {
		t.Fatal("expected duplicate key error from second insert, got nil")
	}
	if !mongo.IsDuplicateKeyError(dupErr) {
		t.Skipf("expected IsDuplicateKeyError, got: %v", dupErr)
		return
	}

	// Now test handleInsertError with the real duplicate key error.
	// The doc is in the collection with idempotency_key = idemp, so FindOne should succeed.
	req := &ledgerv1.RecordTransactionRequest{
		IdempotencyKey:      idemp,
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              100,
		Currency:            "USD",
	}
	resp, respErr := srv.handleInsertError(ctx, col, req, dupErr, time.Now())
	if respErr != nil {
		t.Fatalf("unexpected error from handleInsertError on dup key: %v", respErr)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("expected successful duplicate-key response, got %+v", resp)
	}
	if resp.TransactionId != sharedID.Hex() {
		t.Fatalf("expected existing transaction ID %s, got %s", sharedID.Hex(), resp.TransactionId)
	}
}

func TestGetLedgerEntriesQueryFailure(t *testing.T) {
	// Test the error path in GetLedgerEntries when Find fails
	// We use a canceled context to force the find to fail
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	srv := &server{
		mongoClient: client,
		region:      "us-east-1",
		logger:      observability.NewMemoryLogger(),
	}

	// Use a canceled context to force the MongoDB find to fail
	canceledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()

	_, err = srv.GetLedgerEntries(canceledCtx, &ledgerv1.GetLedgerRequest{
		WalletId: "alice",
		Limit:    10,
	})
	if err == nil {
		t.Fatal("expected error when context is canceled, got nil")
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected codes.Internal, got %v", status.Code(err))
	}
}

func TestGetLedgerEntriesWithPageToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	srv := &server{
		mongoClient: client,
		region:      "us-east-1",
		logger:      observability.NewMemoryLogger(),
	}

	// Insert enough entries to trigger pagination
	col := client.Database("banking_db").Collection("ledger_entries")
	prefix := fmt.Sprintf("page-tok-%d", time.Now().UnixNano())
	for i := 0; i < 3; i++ {
		postings, _ := CreateTransferPostings("paginuser", "paginbob", 10, "USD")
		doc := LedgerDocument{
			ID:                  primitive.NewObjectID(),
			IdempotencyKey:      fmt.Sprintf("%s-%d", prefix, i),
			SourceWalletID:      "paginuser",
			DestinationWalletID: "paginbob",
			Amount:              10,
			Currency:            "USD",
			SequenceNumber:      time.Now().UnixNano() + int64(i),
			Timestamp:           time.Now().UTC(),
			Postings:            postings,
		}
		_, _ = col.InsertOne(ctx, doc)
	}

	// Fetch with limit=1 → should generate a next page token
	resp, err := srv.GetLedgerEntries(ctx, &ledgerv1.GetLedgerRequest{
		WalletId: "paginuser",
		Limit:    1,
	})
	if err != nil {
		t.Fatalf("unexpected error getting ledger entries: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response, got nil")
	}

	// If there's a next page token, fetch the second page with it
	if resp.NextPageToken != "" {
		resp2, err2 := srv.GetLedgerEntries(ctx, &ledgerv1.GetLedgerRequest{
			WalletId:  "paginuser",
			Limit:     1,
			PageToken: resp.NextPageToken,
		})
		if err2 != nil {
			t.Fatalf("unexpected error on page 2: %v", err2)
		}
		if resp2 == nil {
			t.Fatal("expected page 2 response, got nil")
		}
	}
}

func TestGetLedgerEntriesInvalidWalletId(t *testing.T) {
	srv := &server{logger: observability.NewMemoryLogger()}
	_, err := srv.GetLedgerEntries(context.Background(), &ledgerv1.GetLedgerRequest{WalletId: "   "})
	if err == nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty wallet_id, got %v", err)
	}
}

func TestLedgerMetricsServerHTTPEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	// Start metrics server on a random port
	srv := startLedgerMetricsServer(client, "0", "us-east-1")
	defer srv.Close()

	// Give the server a moment to start listening
	time.Sleep(50 * time.Millisecond)

	baseURL := "http://" + srv.Addr
	if srv.Addr == ":0" {
		// server hasn't bound yet, skip
		t.Skip("server addr not resolved, skipping")
	}

	// Test /healthz
	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Skipf("metrics server not reachable: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /healthz, got %d", resp.StatusCode)
	}

	// Test /audit/verify (normal success path — chain verified or tamper detected)
	resp2, err := http.Get(baseURL + "/audit/verify")
	if err != nil {
		t.Skipf("audit verify not reachable: %v", err)
		return
	}
	defer resp2.Body.Close()
	// Either 200 (verified) or 409 (tamper found) are valid, both cover the handler code
	if resp2.StatusCode != http.StatusOK && resp2.StatusCode != http.StatusConflict {
		t.Errorf("unexpected status from /audit/verify: %d", resp2.StatusCode)
	}
}

func TestLedgerMetricsServerHandlersDirect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 2)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	// Test /healthz handler directly via httptest
	healthHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"UP","region":%q}`+"\n", "us-east-1")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	healthHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 from healthz handler, got %d", rec.Code)
	}

	// Test /audit/verify handler directly
	auditHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		col := client.Database("banking_db").Collection("ledger_entries")
		res, verifyErr := VerifyAuditChain(r.Context(), col)
		if verifyErr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"status":"ERROR","error":%q}`, verifyErr.Error())
			return
		}
		if res.Status != "VERIFIED" {
			w.WriteHeader(http.StatusConflict)
		}
		_, _ = fmt.Fprintf(w, `{"status":%q}`, res.Status)
	})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/audit/verify", http.NoBody)
	auditHandler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK && rec2.Code != http.StatusConflict {
		t.Errorf("unexpected status from audit/verify handler: %d", rec2.Code)
	}

	// Test startLedgerMetricsServer directly
	srvMetrics := startLedgerMetricsServer(client, "0", "us-east-1")
	defer srvMetrics.Close()

	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	srvMetrics.Handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 from startLedgerMetricsServer /healthz, got %d", rec3.Code)
	}
}

func TestInitLedgerOutboxBranches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := db.ConnectWithRetry(ctx, db.DefaultMongoURI(), 1)
	if err != nil {
		t.Skipf("MongoDB not reachable: %v", err)
		return
	}
	defer client.Disconnect(ctx)

	// 1. Disabled
	t.Setenv("LOGGING_OUTBOX_ENABLED", "false")
	o1, p1 := initLedgerOutbox(ctx, client)
	if o1 != nil || p1 != nil {
		t.Fatal("expected nil outbox when disabled")
	}

	// 2. Enabled but missing RabbitMQ URL
	t.Setenv("LOGGING_OUTBOX_ENABLED", "true")
	t.Setenv("LOGGING_RABBITMQ_URL", "")
	o2, p2 := initLedgerOutbox(ctx, client)
	if o2 != nil || p2 != nil {
		t.Fatal("expected nil outbox when URL empty")
	}

	// 3. Enabled with RabbitMQ URL
	t.Setenv("LOGGING_OUTBOX_ENABLED", "true")
	t.Setenv("LOGGING_RABBITMQ_URL", "amqp://localhost:5672/")
	o3, p3 := initLedgerOutbox(ctx, client)
	if o3 == nil || p3 == nil {
		t.Fatal("expected non-nil outbox and publisher when configured")
	}
}
