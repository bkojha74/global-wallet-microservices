package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"wallet-system/pkg/observability"
	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

type Gateway struct {
	mu             sync.RWMutex
	activeTarget   string // "PRIMARY" or "STANDBY"
	primaryClient  walletv1.WalletServiceClient
	standbyClient  walletv1.WalletServiceClient
	ledgerClient   ledgerv1.LedgerServiceClient
	primaryAddress string
	standbyAddress string
	logger         observability.Logger
}

func (g *Gateway) emit(ctx context.Context, eventType, level, message string, attributes map[string]any) {
	g.emitFull(ctx, eventType, level, message, 0, nil, attributes)
}

func (g *Gateway) emitTerminal(ctx context.Context, eventType, level, message string, durationMS int64, success bool, attributes map[string]any) {
	g.emitFull(ctx, eventType, level, message, durationMS, &success, attributes)
}

func (g *Gateway) emitFull(ctx context.Context, eventType, level, message string, durationMS int64, success *bool, attributes map[string]any) {
	if g.logger == nil {
		return
	}
	correlation := observability.FromContext(ctx)
	g.logger.Emit(ctx, observability.Event{
		Level:          level,
		EventType:      eventType,
		Message:        message,
		AssociationID:  correlation.AssociationID,
		TransactionID:  correlation.TransactionID,
		IdempotencyKey: correlation.IdempotencyKey,
		DurationMS:     durationMS,
		Success:        success,
		Attributes:     observability.RedactAttributes(attributes),
	})
}

func logProto(traceID, step string, message proto.Message) {
	payload, err := protojson.Marshal(message)
	if err != nil {
		log.Printf("[TRACE] trace_id=%s step=%s proto_json_marshal_error=%v", traceID, step, err)
		return
	}
	log.Printf("[TRACE] trace_id=%s step=%s proto=%s", traceID, step, payload)
}

func readClientJSON(traceID string, body io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(body)
	if err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_json_received_read_failed error=%v", traceID, err)
		return nil, err
	}
	log.Printf("[TRACE] trace_id=%s step=http_json_received body=%q", traceID, payload)
	return payload, nil
}

func (g *Gateway) getActiveWalletClient() (walletv1.WalletServiceClient, string) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.activeTarget == "STANDBY" {
		return g.standbyClient, "STANDBY"
	}
	return g.primaryClient, "PRIMARY"
}

func (g *Gateway) handleCreateWallet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	correlation := observability.FromHTTPRequest(r)
	ctx := observability.WithCorrelation(r.Context(), correlation)
	traceID := correlation.AssociationID
	g.emit(ctx, "api.request.received", observability.LevelInfo, "HTTP request received", map[string]any{"operation": "create_wallet"})
	body, err := readClientJSON(traceID, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req struct {
		WalletID       string `json:"wallet_id"`
		Currency       string `json:"currency"`
		InitialBalance int64  `json:"initial_balance"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_json_decode_failed error=%v", traceID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[TRACE] trace_id=%s step=http_json_decoded operation=create_wallet wallet_id=%s", traceID, req.WalletID)
	protoReq := &walletv1.CreateWalletRequest{WalletId: req.WalletID, Currency: req.Currency, InitialBalance: req.InitialBalance}
	logProto(traceID, "http_json_to_wallet_proto_request", protoReq)

	client, target := g.getActiveWalletClient()
	log.Printf("[TRACE] trace_id=%s step=wallet_grpc_request target=%s", traceID, target)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ctx = observability.WithOutgoingMetadata(ctx, correlation)
	resp, err := client.CreateWallet(ctx, protoReq)
	if err != nil {
		log.Printf("[TRACE] trace_id=%s step=wallet_grpc_response error=%v", traceID, err)
		http.Error(w, fmt.Sprintf("Error creating wallet: %v", err), http.StatusInternalServerError)
		return
	}
	logProto(traceID, "wallet_proto_response_received", resp)
	g.emit(ctx, "api.response.sent", observability.LevelInfo, "Wallet creation response sent", map[string]any{"operation": "create_wallet", "routed_target": target})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        resp.Success,
		"message":        resp.Message,
		"routed_gateway": target,
		"handled_region": resp.HandledByRegion,
	})
}

func (g *Gateway) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	walletID := r.URL.Query().Get("id")
	if walletID == "" {
		http.Error(w, "Missing 'id' query parameter", http.StatusBadRequest)
		return
	}
	correlation := observability.FromHTTPRequest(r)
	ctx := observability.WithCorrelation(r.Context(), correlation)
	traceID := correlation.AssociationID
	g.emit(ctx, "api.request.received", observability.LevelInfo, "HTTP request received", map[string]any{"operation": "get_balance"})
	log.Printf("[TRACE] trace_id=%s step=http_json_decoded operation=get_balance wallet_id=%s", traceID, walletID)
	protoReq := &walletv1.GetBalanceRequest{WalletId: walletID}
	logProto(traceID, "http_json_to_wallet_proto_request", protoReq)

	client, target := g.getActiveWalletClient()
	log.Printf("[TRACE] trace_id=%s step=wallet_grpc_request target=%s", traceID, target)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ctx = observability.WithOutgoingMetadata(ctx, correlation)
	resp, err := client.GetBalance(ctx, protoReq)
	if err != nil {
		log.Printf("[TRACE] trace_id=%s step=wallet_grpc_response error=%v", traceID, err)
		http.Error(w, fmt.Sprintf("Error retrieving balance: %v", err), http.StatusInternalServerError)
		return
	}
	logProto(traceID, "wallet_proto_response_received", resp)
	g.emit(ctx, "api.response.sent", observability.LevelInfo, "Balance response sent", map[string]any{"operation": "get_balance", "routed_target": target})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"wallet_id":      resp.WalletId,
		"balances":       resp.Balances,
		"routed_gateway": target,
		"handled_region": resp.HandledByRegion,
	})
}

func (g *Gateway) handleTransfer(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	correlation := observability.FromHTTPRequest(r)
	traceID := correlation.AssociationID
	body, err := readClientJSON(traceID, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req struct {
		IdempotencyKey string `json:"idempotency_key"`
		SourceWallet   string `json:"source_wallet_id"`
		DestWallet     string `json:"destination_wallet_id"`
		Amount         int64  `json:"amount"`
		Currency       string `json:"currency"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_json_decode_failed error=%v", traceID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.IdempotencyKey != "" {
		correlation.IdempotencyKey = req.IdempotencyKey
	}
	ctx := observability.WithCorrelation(r.Context(), correlation)
	// Step 1: api.request.received (INFO)
	g.emit(ctx, "api.request.received", observability.LevelInfo, "HTTP transfer request received", map[string]any{
		"operation":     "transfer",
		"source_wallet": req.SourceWallet,
		"dest_wallet":   req.DestWallet,
		"amount":        req.Amount,
		"currency":      req.Currency,
	})
	traceID = correlation.AssociationID
	log.Printf("[TRACE] trace_id=%s step=http_json_decoded operation=transfer source=%s destination=%s amount=%d currency=%s", traceID, req.SourceWallet, req.DestWallet, req.Amount, req.Currency)
	protoReq := &walletv1.TransferFundsRequest{
		IdempotencyKey:      req.IdempotencyKey,
		SourceWalletId:      req.SourceWallet,
		DestinationWalletId: req.DestWallet,
		Amount:              &walletv1.Money{Currency: req.Currency, Units: req.Amount},
	}
	logProto(traceID, "http_json_to_wallet_proto_request", protoReq)

	client, target := g.getActiveWalletClient()

	// Step 2: api.wallet_proto.request_created (DEBUG)
	g.emit(ctx, "api.wallet_proto.request_created", observability.LevelDebug, "Wallet protobuf request created", map[string]any{
		"operation":       "transfer",
		"target":          target,
		"idempotency_key": req.IdempotencyKey,
	})

	log.Printf("[TRACE] trace_id=%s step=wallet_grpc_request target=%s", traceID, target)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	ctx = observability.WithOutgoingMetadata(ctx, correlation)
	resp, err := client.TransferFunds(ctx, protoReq)
	durationMS := time.Since(startTime).Milliseconds()
	if err != nil {
		log.Printf("[TRACE] trace_id=%s step=wallet_grpc_response error=%v", traceID, err)
		g.emitTerminal(ctx, "api.response.sent", observability.LevelError, "Transfer gRPC failure", durationMS, false, map[string]any{
			"operation": "transfer",
			"error":     err.Error(),
		})
		http.Error(w, fmt.Sprintf("Transfer gRPC failure: %v", err), http.StatusInternalServerError)
		return
	}
	logProto(traceID, "wallet_proto_response_received", resp)
	if resp.TransactionId != "" {
		correlation.TransactionID = resp.TransactionId
		ctx = observability.WithCorrelation(ctx, correlation)
	}

	// Step 12: api.response.sent (INFO)
	isSuccess := resp.Status == walletv1.TransferFundsResponse_SUCCESS
	g.emitTerminal(ctx, "api.response.sent", observability.LevelInfo, "Transfer response sent", durationMS, isSuccess, map[string]any{
		"operation":      "transfer",
		"routed_target":  target,
		"status":         resp.Status.String(),
		"transaction_id": resp.TransactionId,
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"transaction_id": resp.TransactionId,
		"status":         resp.Status.String(),
		"error_message":  resp.ErrorMessage,
		"routed_gateway": target,
		"handled_region": resp.HandledByRegion,
	})
}

func (g *Gateway) handleLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	walletID := r.URL.Query().Get("wallet_id")
	correlation := observability.FromHTTPRequest(r)
	ctx := observability.WithCorrelation(r.Context(), correlation)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ctx = observability.WithOutgoingMetadata(ctx, correlation)
	resp, err := g.ledgerClient.GetLedgerEntries(ctx, &ledgerv1.GetLedgerRequest{WalletId: walletID})
	if err != nil {
		http.Error(w, fmt.Sprintf("Ledger query failure: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, resp.Entries)
}

func (g *Gateway) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	correlation := observability.FromHTTPRequest(r)
	ctx := observability.WithCorrelation(r.Context(), correlation)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	ctx = observability.WithOutgoingMetadata(ctx, correlation)
	pHealth, _ := g.primaryClient.HealthCheck(ctx, &walletv1.HealthRequest{})
	sHealth, _ := g.standbyClient.HealthCheck(ctx, &walletv1.HealthRequest{})

	g.mu.RLock()
	currentActive := g.activeTarget
	g.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"current_routed_target": currentActive,
		"primary_region": map[string]interface{}{
			"address": g.primaryAddress,
			"status":  pHealth,
		},
		"standby_region": map[string]interface{}{
			"address": g.standbyAddress,
			"status":  sHealth,
		},
	})
}

func (g *Gateway) handleFailover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	g.mu.Lock()
	if g.activeTarget == "PRIMARY" {
		g.activeTarget = "STANDBY"
	} else {
		g.activeTarget = "PRIMARY"
	}
	newTarget := g.activeTarget
	g.mu.Unlock()

	log.Printf("[GATEWAY-FAILOVER] Switched traffic route to: %s", newTarget)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":              "Failover routing triggered",
		"active_routed_target": newTarget,
	})
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		log.Printf("[TRACE] step=http_json_response_encode_failed status=%d error=%v", status, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[TRACE] step=http_json_response_sent status=%d body=%s", status, payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

func main() {
	httpPort := os.Getenv("HTTP_PORT")
	if httpPort == "" {
		httpPort = "8080"
	}
	primaryAddr := os.Getenv("PRIMARY_WALLET_ADDR")
	if primaryAddr == "" {
		if _, err := net.LookupHost("wallet-primary"); err == nil {
			primaryAddr = "wallet-primary:50051"
		} else {
			primaryAddr = "127.0.0.1:50051"
		}
	}
	standbyAddr := os.Getenv("STANDBY_WALLET_ADDR")
	if standbyAddr == "" {
		if _, err := net.LookupHost("wallet-standby"); err == nil {
			standbyAddr = "wallet-standby:50053"
		} else {
			standbyAddr = "127.0.0.1:50053"
		}
	}
	ledgerAddr := os.Getenv("LEDGER_ADDR")
	if ledgerAddr == "" {
		if _, err := net.LookupHost("ledger-service"); err == nil {
			ledgerAddr = "ledger-service:50052"
		} else {
			ledgerAddr = "127.0.0.1:50052"
		}
	}

	log.Println("[API-GATEWAY] Establishing gRPC connections...")
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "local"
	}
	logger := observability.LoggerFromEnvironment("api-gateway", environment, "", os.Stdout)

	pConn, err := grpc.NewClient(primaryAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to dial primary wallet: %v", err)
	}
	defer pConn.Close()

	sConn, err := grpc.NewClient(standbyAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to dial standby wallet: %v", err)
	}
	defer sConn.Close()

	lConn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to dial ledger: %v", err)
	}
	defer lConn.Close()

	gw := &Gateway{
		activeTarget:   "PRIMARY",
		primaryClient:  walletv1.NewWalletServiceClient(pConn),
		standbyClient:  walletv1.NewWalletServiceClient(sConn),
		ledgerClient:   ledgerv1.NewLedgerServiceClient(lConn),
		primaryAddress: primaryAddr,
		standbyAddress: standbyAddr,
		logger:         logger,
	}

	http.HandleFunc("/api/v1/wallets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			gw.handleCreateWallet(w, r)
		} else if r.Method == http.MethodGet {
			gw.handleGetBalance(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
	http.HandleFunc("/api/v1/transfers", gw.handleTransfer)
	http.HandleFunc("/api/v1/ledger", gw.handleLedger)
	http.HandleFunc("/api/v1/cluster/status", gw.handleClusterStatus)
	http.HandleFunc("/api/v1/cluster/failover", gw.handleFailover)

	// Expose Prometheus metrics (GAP-07)
	http.Handle("/metrics", observability.DefaultMetrics.Handler())

	log.Printf("[API-GATEWAY] HTTP REST Gateway listening on :%s", httpPort)
	if err := http.ListenAndServe(":"+httpPort, nil); err != nil {
		log.Fatalf("Gateway server failure: %v", err)
	}
}
