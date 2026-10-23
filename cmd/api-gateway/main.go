package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"wallet-system/pkg/auth"
	"wallet-system/pkg/coordinator"
	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	"wallet-system/pkg/resilience"
	"wallet-system/pkg/tlsutil"
	"wallet-system/pkg/webhook"
	authv1 "wallet-system/proto/auth"
	fxv1 "wallet-system/proto/fx"
	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

const defaultJWTSecret = "development-wallet-insecure-secret-key-change-in-prod"

// Gateway routes incoming HTTP requests to the appropriate gRPC backend service.
type Gateway struct {
	mu                sync.RWMutex
	activeTarget      string // "PRIMARY" or "STANDBY" (cached fallback)
	coordinator       coordinator.FailoverCoordinator
	primaryClient     walletv1.WalletServiceClient
	standbyClient     walletv1.WalletServiceClient
	ledgerClient      ledgerv1.LedgerServiceClient
	authClient        authv1.AuthServiceClient // auth-service gRPC client
	fxClient          fxv1.FXServiceClient     // fx-service gRPC client
	primaryAddress    string
	standbyAddress    string
	ledgerMgmtURL     string
	httpClient        *http.Client
	logger            observability.Logger
	webhookDispatcher *webhook.Dispatcher
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

func (g *Gateway) getActiveWalletClient(ctx context.Context) (walletv1.WalletServiceClient, string) {
	target := coordinator.TargetPrimary
	if g.coordinator != nil {
		if t, err := g.coordinator.GetActiveTarget(ctx); err == nil && t != "" {
			target = t
		}
	} else {
		g.mu.RLock()
		if g.activeTarget != "" {
			target = g.activeTarget
		}
		g.mu.RUnlock()
	}
	if target == coordinator.TargetStandby {
		return g.standbyClient, coordinator.TargetStandby
	}
	return g.primaryClient, coordinator.TargetPrimary
}

const (
	apiGatewayServiceName   = "api-gateway"
	errMethodNotAllowed     = "Method not allowed"
	msgForbiddenFmt         = "Forbidden: %v"
	eventAPIRequestReceived = "api.request.received"
	eventAPIResponseSent    = "api.response.sent"
	traceWalletGRPCReqFmt   = "[TRACE] trace_id=%s step=wallet_grpc_request target=%s"
	traceWalletGRPCRespFmt  = "[TRACE] trace_id=%s step=wallet_grpc_response error=%v"
)

func parseCreateWalletRequest(r *http.Request, traceID string) (*walletv1.CreateWalletRequest, error) {
	body, err := readClientJSON(traceID, r.Body)
	if err != nil {
		return nil, err
	}
	var req struct {
		WalletID       string `json:"wallet_id"`
		Currency       string `json:"currency"`
		InitialBalance int64  `json:"initial_balance"`
		KYCTier        string `json:"kyc_tier"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_json_decode_failed error=%v", traceID, err)
		return nil, err
	}
	if !db.IsValidCurrency(req.Currency) {
		return nil, fmt.Errorf("invalid or unsupported currency: %s", req.Currency)
	}
	if req.InitialBalance < 0 {
		return nil, errors.New("initial balance cannot be negative")
	}
	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if err := ValidateWalletOwnership(claims, req.WalletID); err != nil {
			log.Printf("[SECURITY] IDOR blocked: subject %s tried to create wallet for %s", claims.Subject, req.WalletID)
			return nil, fmt.Errorf("forbidden: %w", err)
		}
	}
	log.Printf("[TRACE] trace_id=%s step=http_json_decoded operation=create_wallet wallet_id=%s kyc_tier=%s", traceID, req.WalletID, req.KYCTier)
	return &walletv1.CreateWalletRequest{
		WalletId:       req.WalletID,
		Currency:       req.Currency,
		InitialBalance: req.InitialBalance,
		KycTier:        req.KYCTier,
	}, nil
}

func handleWalletGRPCError(w http.ResponseWriter, err error, action string) {
	if st, ok := status.FromError(err); ok {
		if st.Code() == codes.AlreadyExists {
			http.Error(w, st.Message(), http.StatusConflict)
			return
		}
		if st.Code() == codes.InvalidArgument {
			http.Error(w, st.Message(), http.StatusBadRequest)
			return
		}
	}
	http.Error(w, fmt.Sprintf("Error %s: %v", action, err), http.StatusInternalServerError)
}

func (g *Gateway) handleCreateWallet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	correlation := observability.FromHTTPRequest(r)
	ctx := observability.WithCorrelation(r.Context(), correlation)
	traceID := correlation.AssociationID
	g.emit(ctx, eventAPIRequestReceived, observability.LevelInfo, "HTTP request received", map[string]any{"operation": "create_wallet"})

	protoReq, err := parseCreateWalletRequest(r, traceID)
	if err != nil {
		if strings.HasPrefix(err.Error(), "forbidden:") {
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf(msgForbiddenFmt, err))
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	logProto(traceID, "http_json_to_wallet_proto_request", protoReq)

	client, target := g.getActiveWalletClient(ctx)
	log.Printf(traceWalletGRPCReqFmt, traceID, target)
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	resp, err := client.CreateWallet(callCtx, protoReq)
	if err != nil {
		log.Printf(traceWalletGRPCRespFmt, traceID, err)
		handleWalletGRPCError(w, err, "creating wallet")
		return
	}
	logProto(traceID, "wallet_proto_response_received", resp)
	g.emit(ctx, eventAPIResponseSent, observability.LevelInfo, "Wallet creation response sent", map[string]any{"operation": "create_wallet", "routed_target": target})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        resp.Success,
		"message":        resp.Message,
		"routed_gateway": target,
		"handled_region": resp.HandledByRegion,
	})
}

func (g *Gateway) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	walletID := r.URL.Query().Get("id")
	if walletID == "" {
		http.Error(w, "Missing 'id' query parameter", http.StatusBadRequest)
		return
	}
	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if err := ValidateWalletOwnership(claims, walletID); err != nil {
			log.Printf("[SECURITY] IDOR blocked: subject %s tried to view balance of %s", claims.Subject, walletID)
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf(msgForbiddenFmt, err))
			return
		}
	}
	correlation := observability.FromHTTPRequest(r)
	ctx := observability.WithCorrelation(r.Context(), correlation)
	traceID := correlation.AssociationID
	g.emit(ctx, eventAPIRequestReceived, observability.LevelInfo, "HTTP request received", map[string]any{"operation": "get_balance"})
	log.Printf("[TRACE] trace_id=%s step=http_json_decoded operation=get_balance wallet_id=%s", traceID, walletID)
	protoReq := &walletv1.GetBalanceRequest{WalletId: walletID}
	logProto(traceID, "http_json_to_wallet_proto_request", protoReq)

	client, target := g.getActiveWalletClient(ctx)
	log.Printf(traceWalletGRPCReqFmt, traceID, target)
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	resp, err := client.GetBalance(callCtx, protoReq)
	if err != nil {
		log.Printf(traceWalletGRPCRespFmt, traceID, err)
		http.Error(w, fmt.Sprintf("Error retrieving balance: %v", err), http.StatusInternalServerError)
		return
	}
	logProto(traceID, "wallet_proto_response_received", resp)
	g.emit(ctx, eventAPIResponseSent, observability.LevelInfo, "Balance response sent", map[string]any{"operation": "get_balance", "routed_target": target})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"wallet_id":      resp.WalletId,
		"balances":       resp.Balances,
		"status":         resp.Status,
		"routed_gateway": target,
		"handled_region": resp.HandledByRegion,
	})
}

func (g *Gateway) handleTransfer(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
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
		IdempotencyKey      string `json:"idempotency_key"`
		SourceWalletID      string `json:"source_wallet_id"`
		SourceWallet        string `json:"source_wallet"`
		DestinationWalletID string `json:"destination_wallet_id"`
		DestWallet          string `json:"dest_wallet"`
		Amount              int64  `json:"amount"`
		Currency            string `json:"currency"`
		FXQuoteID           string `json:"fx_quote_id"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_json_decode_failed error=%v", traceID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sourceWallet := strings.TrimSpace(req.SourceWalletID)
	if sourceWallet == "" {
		sourceWallet = strings.TrimSpace(req.SourceWallet)
	}
	destWallet := strings.TrimSpace(req.DestinationWalletID)
	if destWallet == "" {
		destWallet = strings.TrimSpace(req.DestWallet)
	}
	if sourceWallet == "" || destWallet == "" {
		http.Error(w, "source_wallet_id and destination_wallet_id are required", http.StatusBadRequest)
		return
	}
	idempKey := strings.TrimSpace(req.IdempotencyKey)
	if idempKey == "" {
		idempKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if idempKey == "" {
		if os.Getenv("ENFORCE_CLIENT_IDEMPOTENCY") == "true" || r.Header.Get("X-Enforce-Idempotency") == "true" {
			http.Error(w, "Idempotency-Key is required for financial mutations", http.StatusBadRequest)
			return
		}
	} else {
		correlation.IdempotencyKey = idempKey
	}
	if err := db.ValidateAmount(req.Amount, req.Currency); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if err := ValidateWalletOwnership(claims, sourceWallet); err != nil {
			log.Printf("[SECURITY] IDOR blocked: subject %s tried to transfer from %s", claims.Subject, sourceWallet)
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf(msgForbiddenFmt, err))
			return
		}
	}
	ctx := observability.WithCorrelation(r.Context(), correlation)
	// Step 1: api.request.received (INFO)
	g.emit(ctx, eventAPIRequestReceived, observability.LevelInfo, "HTTP transfer request received", map[string]any{
		"operation":     "transfer",
		"source_wallet": sourceWallet,
		"dest_wallet":   destWallet,
		"amount":        req.Amount,
		"currency":      req.Currency,
		"fx_quote_id":   req.FXQuoteID,
	})
	traceID = correlation.AssociationID
	log.Printf("[TRACE] trace_id=%s step=http_json_decoded operation=transfer source=%s destination=%s amount=%d currency=%s", traceID, sourceWallet, destWallet, req.Amount, req.Currency)
	protoReq := &walletv1.TransferFundsRequest{
		IdempotencyKey:      req.IdempotencyKey,
		SourceWalletId:      sourceWallet,
		DestinationWalletId: destWallet,
		Amount:              &walletv1.Money{Currency: req.Currency, Units: req.Amount},
		FxQuoteId:           strings.TrimSpace(req.FXQuoteID),
	}
	logProto(traceID, "http_json_to_wallet_proto_request", protoReq)

	client, target := g.getActiveWalletClient(ctx)

	// Step 2: api.wallet_proto.request_created (DEBUG)
	g.emit(ctx, "api.wallet_proto.request_created", observability.LevelDebug, "Wallet protobuf request created", map[string]any{
		"operation":       "transfer",
		"target":          target,
		"idempotency_key": req.IdempotencyKey,
	})

	log.Printf(traceWalletGRPCReqFmt, traceID, target)
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	resp, err := client.TransferFunds(callCtx, protoReq)
	durationMS := time.Since(startTime).Milliseconds()
	if err != nil {
		log.Printf(traceWalletGRPCRespFmt, traceID, err)
		g.emitTerminal(ctx, eventAPIResponseSent, observability.LevelError, "Transfer gRPC failure", durationMS, false, map[string]any{
			"operation": "transfer",
			"error":     err.Error(),
		})
		if st, ok := status.FromError(err); ok && st.Code() == codes.PermissionDenied {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": st.Message()})
			return
		}
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
	g.emitTerminal(ctx, eventAPIResponseSent, observability.LevelInfo, "Transfer response sent", durationMS, isSuccess, map[string]any{
		"operation":      "transfer",
		"routed_target":  target,
		"status":         resp.Status.String(),
		"transaction_id": resp.TransactionId,
	})

	if isSuccess {
		if d := g.getWebhookDispatcher(); d != nil {
			bgCtx := context.WithoutCancel(r.Context())
			// #nosec G118 -- asynchronous webhook dispatch must decouple from HTTP request context cancellation
			go func() {
				_ = d.Dispatch(bgCtx, webhook.EventTransferCompleted, map[string]interface{}{
					"transaction_id":        resp.TransactionId,
					"source_wallet_id":      protoReq.SourceWalletId,
					"destination_wallet_id": protoReq.DestinationWalletId,
					"amount":                protoReq.Amount.Units,
					"currency":              protoReq.Amount.Currency,
					"fx_quote_id":           resp.FxQuoteId,
					"handled_region":        resp.HandledByRegion,
				})
			}()
		}
	}

	respMap := map[string]interface{}{
		"transaction_id": resp.TransactionId,
		"status":         resp.Status.String(),
		"error_message":  resp.ErrorMessage,
		"routed_gateway": target,
		"handled_region": resp.HandledByRegion,
	}
	if resp.FxQuoteId != "" {
		respMap["fx_quote_id"] = resp.FxQuoteId
	}
	if resp.ExchangeRate > 0 {
		respMap["exchange_rate"] = resp.ExchangeRate
	}
	if resp.ConvertedAmount != nil {
		respMap["converted_amount"] = resp.ConvertedAmount
	}
	writeJSON(w, http.StatusOK, respMap)
}

func (g *Gateway) handleWalletExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
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
		WalletID       string `json:"wallet_id"`
		FromCurrency   string `json:"from_currency"`
		ToCurrency     string `json:"to_currency"`
		FromAmount     int64  `json:"from_amount"`
		Amount         int64  `json:"amount"`
		FXQuoteID      string `json:"fx_quote_id"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_json_decode_failed error=%v", traceID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	walletID := strings.TrimSpace(req.WalletID)
	if walletID == "" {
		http.Error(w, "wallet_id is required", http.StatusBadRequest)
		return
	}
	fromAmount := req.FromAmount
	if fromAmount <= 0 {
		fromAmount = req.Amount
	}
	fromCurr := db.NormalizeCurrency(req.FromCurrency)
	toCurr := db.NormalizeCurrency(req.ToCurrency)
	if fromCurr == "" || toCurr == "" {
		http.Error(w, "from_currency and to_currency are required", http.StatusBadRequest)
		return
	}
	if fromCurr == toCurr {
		http.Error(w, "from_currency and to_currency must be different", http.StatusBadRequest)
		return
	}
	if err := db.ValidateAmount(fromAmount, fromCurr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	idempKey := strings.TrimSpace(req.IdempotencyKey)
	if idempKey == "" {
		idempKey = r.Header.Get("Idempotency-Key")
	}
	if idempKey == "" {
		idempKey = "idemp_exch_" + traceID
	}
	correlation.IdempotencyKey = idempKey

	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if err := ValidateWalletOwnership(claims, walletID); err != nil {
			log.Printf("[SECURITY] IDOR blocked: subject %s tried to exchange currency in wallet %s", claims.Subject, walletID)
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf(msgForbiddenFmt, err))
			return
		}
	}

	ctx := observability.WithCorrelation(r.Context(), correlation)
	g.emit(ctx, eventAPIRequestReceived, observability.LevelInfo, "HTTP intra-wallet exchange request received", map[string]any{
		"operation":     "exchange_currency",
		"wallet_id":     walletID,
		"from_currency": fromCurr,
		"to_currency":   toCurr,
		"from_amount":   fromAmount,
		"fx_quote_id":   req.FXQuoteID,
	})

	protoReq := &walletv1.ExchangeCurrencyRequest{
		IdempotencyKey: idempKey,
		WalletId:       walletID,
		FromCurrency:   fromCurr,
		ToCurrency:     toCurr,
		FromAmount:     fromAmount,
		FxQuoteId:      strings.TrimSpace(req.FXQuoteID),
	}

	client, target := g.getActiveWalletClient(ctx)
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	resp, err := client.ExchangeCurrency(callCtx, protoReq)
	if err != nil {
		log.Printf(traceWalletGRPCRespFmt, traceID, err)
		handleWalletGRPCError(w, err, "exchanging currency")
		return
	}

	if resp.Status != walletv1.ExchangeCurrencyResponse_SUCCESS {
		statusHTTP := http.StatusBadRequest
		if resp.Status == walletv1.ExchangeCurrencyResponse_FAILED_INSUFFICIENT_FUNDS {
			statusHTTP = http.StatusUnprocessableEntity
		}
		writeJSON(w, statusHTTP, map[string]interface{}{
			"status":         resp.Status.String(),
			"error_message":  resp.ErrorMessage,
			"transaction_id": resp.TransactionId,
			"routed_gateway": target,
			"handled_region": resp.HandledByRegion,
		})
		return
	}

	g.emit(ctx, eventAPIResponseSent, observability.LevelInfo, "Intra-wallet currency exchange response sent", map[string]any{
		"operation":      "exchange_currency",
		"transaction_id": resp.TransactionId,
		"wallet_id":      walletID,
		"from_currency":  fromCurr,
		"from_amount":    fromAmount,
		"to_currency":    toCurr,
		"exchange_rate":  resp.ExchangeRate,
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":          resp.Status.String(),
		"transaction_id":  resp.TransactionId,
		"wallet_id":       walletID,
		"from_currency":   fromCurr,
		"debited_amount":  resp.GetDebitedAmount().GetUnits(),
		"to_currency":     toCurr,
		"credited_amount": resp.GetCreditedAmount().GetUnits(),
		"exchange_rate":   resp.ExchangeRate,
		"fx_quote_id":     resp.FxQuoteId,
		"routed_gateway":  target,
		"handled_region":  resp.HandledByRegion,
	})
}

func (g *Gateway) handleUpdateWalletKYC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
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
		WalletID string `json:"wallet_id"`
		KYCTier  string `json:"kyc_tier"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	walletID := strings.TrimSpace(req.WalletID)
	if walletID == "" {
		http.Error(w, "wallet_id is required", http.StatusBadRequest)
		return
	}
	tier := strings.ToUpper(strings.TrimSpace(req.KYCTier))
	if tier == "" {
		http.Error(w, "kyc_tier is required (TIER_1, TIER_2, TIER_3)", http.StatusBadRequest)
		return
	}

	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if !claims.HasRole(auth.RoleAdmin) && !claims.HasScope("wallet:kyc:admin") {
			writeAuthError(w, http.StatusForbidden, "Forbidden: KYC tier modification requires admin privileges or wallet:kyc:admin scope")
			return
		}
	}

	ctx := observability.WithCorrelation(r.Context(), correlation)
	client, target := g.getActiveWalletClient(ctx)
	callCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	resp, err := client.UpdateWalletKYC(callCtx, &walletv1.UpdateWalletKYCRequest{
		WalletId: walletID,
		KycTier:  tier,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to update wallet KYC: %v", err), http.StatusInternalServerError)
		return
	}
	if !resp.Success {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": resp.Message,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        resp.Success,
		"wallet_id":      resp.WalletId,
		"previous_tier":  resp.PreviousTier,
		"current_tier":   resp.CurrentTier,
		"message":        resp.Message,
		"routed_gateway": target,
	})
}

func (g *Gateway) handleGetWalletLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	walletID := strings.TrimSpace(r.URL.Query().Get("id"))
	if walletID == "" {
		http.Error(w, "Missing 'id' query parameter", http.StatusBadRequest)
		return
	}

	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if err := ValidateWalletOwnership(claims, walletID); err != nil && !claims.HasRole(auth.RoleAdmin) {
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf(msgForbiddenFmt, err))
			return
		}
	}

	correlation := observability.FromHTTPRequest(r)
	ctx := observability.WithCorrelation(r.Context(), correlation)
	client, target := g.getActiveWalletClient(ctx)
	callCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	resp, err := client.GetWalletLimits(callCtx, &walletv1.GetWalletLimitsRequest{
		WalletId: walletID,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get wallet limits: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"wallet_id":                resp.WalletId,
		"kyc_tier":                 resp.KycTier,
		"single_transaction_limit": resp.SingleTransactionLimit,
		"daily_rolling_limit":      resp.DailyRollingLimit,
		"daily_rolling_spent":      resp.DailyRollingSpent,
		"daily_rolling_remaining":  resp.DailyRollingRemaining,
		"hourly_rate_limit":        resp.HourlyRateLimit,
		"hourly_rate_spent":        resp.HourlyRateSpent,
		"hourly_rate_remaining":    resp.HourlyRateRemaining,
		"routed_gateway":           target,
	})
}

func parseProtoPaymentRail(railStr string) walletv1.PaymentRail {
	switch strings.ToUpper(strings.TrimSpace(railStr)) {
	case "CARD":
		return walletv1.PaymentRail_CARD
	case "ACH":
		return walletv1.PaymentRail_ACH
	case "SEPA":
		return walletv1.PaymentRail_SEPA
	case "WIRE":
		return walletv1.PaymentRail_WIRE
	default:
		return walletv1.PaymentRail_PAYMENT_RAIL_UNSPECIFIED
	}
}

func (gw *Gateway) handleWalletSubroutes(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	// /api/v1/wallets/{id}/deposit or /api/v1/wallets/{id}/withdraw
	if len(parts) >= 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "wallets" {
		walletID := parts[3]
		action := parts[4]
		if action == "deposit" {
			gw.executeDeposit(w, r, walletID)
			return
		}
		if action == "withdraw" {
			gw.executeWithdraw(w, r, walletID)
			return
		}
	}
	http.Error(w, "Not Found", http.StatusNotFound)
}

func (gw *Gateway) handleDepositFunds(w http.ResponseWriter, r *http.Request) {
	gw.executeDeposit(w, r, "")
}

func (gw *Gateway) handleWithdrawFunds(w http.ResponseWriter, r *http.Request) {
	gw.executeWithdraw(w, r, "")
}

func (g *Gateway) executeDeposit(w http.ResponseWriter, r *http.Request, pathWalletID string) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	correlation := observability.FromHTTPRequest(r)
	traceID := correlation.AssociationID

	body, err := readClientJSON(traceID, r.Body)
	if err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_read_body_failed error=%v", traceID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req struct {
		IdempotencyKey string `json:"idempotency_key"`
		WalletID       string `json:"wallet_id"`
		Amount         int64  `json:"amount"`
		Currency       string `json:"currency"`
		PaymentRail    string `json:"payment_rail"`
		Reference      string `json:"reference"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_json_decode_failed error=%v", traceID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	walletID := strings.TrimSpace(pathWalletID)
	if walletID == "" {
		walletID = strings.TrimSpace(req.WalletID)
	}
	if walletID == "" {
		walletID = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if walletID == "" {
		walletID = strings.TrimSpace(r.URL.Query().Get("wallet_id"))
	}
	if walletID == "" {
		http.Error(w, "wallet_id is required", http.StatusBadRequest)
		return
	}

	curr := db.NormalizeCurrency(req.Currency)
	if curr == "" {
		http.Error(w, "currency is required", http.StatusBadRequest)
		return
	}
	if req.Amount <= 0 {
		http.Error(w, "amount must be strictly positive", http.StatusBadRequest)
		return
	}
	if err := db.ValidateAmount(req.Amount, curr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	railProto := parseProtoPaymentRail(req.PaymentRail)
	if railProto == walletv1.PaymentRail_PAYMENT_RAIL_UNSPECIFIED {
		http.Error(w, "payment_rail must be one of CARD, ACH, SEPA, WIRE", http.StatusBadRequest)
		return
	}

	idempKey := strings.TrimSpace(req.IdempotencyKey)
	if idempKey == "" {
		idempKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if idempKey == "" {
		if os.Getenv("ENFORCE_CLIENT_IDEMPOTENCY") == "true" || r.Header.Get("X-Enforce-Idempotency") == "true" {
			http.Error(w, "Idempotency-Key is required for financial mutations", http.StatusBadRequest)
			return
		}
		idempKey = "idemp_dep_" + traceID
	}
	correlation.IdempotencyKey = idempKey

	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if err := ValidateWalletOwnership(claims, walletID); err != nil && !claims.HasRole(auth.RoleAdmin) {
			log.Printf("[SECURITY] IDOR blocked: subject %s tried to deposit into wallet %s", claims.Subject, walletID)
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf(msgForbiddenFmt, err))
			return
		}
	}

	ctx := observability.WithCorrelation(r.Context(), correlation)
	g.emit(ctx, eventAPIRequestReceived, observability.LevelInfo, "HTTP deposit funds request received", map[string]any{
		"operation":    "deposit_funds",
		"wallet_id":    walletID,
		"currency":     curr,
		"amount":       req.Amount,
		"payment_rail": req.PaymentRail,
		"reference":    req.Reference,
	})

	protoReq := &walletv1.DepositFundsRequest{
		IdempotencyKey: idempKey,
		WalletId:       walletID,
		Amount: &walletv1.Money{
			Currency: curr,
			Units:    req.Amount,
		},
		PaymentRail: railProto,
		Reference:   req.Reference,
	}

	client, target := g.getActiveWalletClient(ctx)
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	resp, err := client.DepositFunds(callCtx, protoReq)
	if err != nil {
		log.Printf(traceWalletGRPCRespFmt, traceID, err)
		handleWalletGRPCError(w, err, "depositing funds")
		return
	}

	if resp.Status != walletv1.DepositFundsResponse_SUCCESS {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"status":         resp.Status.String(),
			"error_message":  resp.ErrorMessage,
			"transaction_id": resp.TransactionId,
			"routed_gateway": target,
			"handled_region": resp.HandledByRegion,
		})
		return
	}

	g.emit(ctx, eventAPIResponseSent, observability.LevelInfo, "Deposit funds response sent", map[string]any{
		"operation":      "deposit_funds",
		"transaction_id": resp.TransactionId,
		"wallet_id":      walletID,
		"currency":       curr,
		"amount":         req.Amount,
		"payment_rail":   req.PaymentRail,
	})

	if d := g.getWebhookDispatcher(); d != nil {
		bgCtx := context.WithoutCancel(r.Context())
		// #nosec G118 -- asynchronous webhook dispatch must decouple from HTTP request context cancellation
		go func() {
			_ = d.Dispatch(bgCtx, webhook.EventDepositCompleted, map[string]interface{}{
				"transaction_id":     resp.TransactionId,
				"wallet_id":          walletID,
				"amount":             resp.GetCreditedAmount().GetUnits(),
				"currency":           resp.GetCreditedAmount().GetCurrency(),
				"payment_rail":       resp.PaymentRail.String(),
				"external_reference": resp.ExternalReference,
				"handled_region":     resp.HandledByRegion,
			})
		}()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":             resp.Status.String(),
		"transaction_id":     resp.TransactionId,
		"wallet_id":          walletID,
		"amount":             resp.GetCreditedAmount().GetUnits(),
		"currency":           resp.GetCreditedAmount().GetCurrency(),
		"payment_rail":       resp.PaymentRail.String(),
		"external_reference": resp.ExternalReference,
		"routed_gateway":     target,
		"handled_region":     resp.HandledByRegion,
	})
}

func (g *Gateway) executeWithdraw(w http.ResponseWriter, r *http.Request, pathWalletID string) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	correlation := observability.FromHTTPRequest(r)
	traceID := correlation.AssociationID

	body, err := readClientJSON(traceID, r.Body)
	if err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_read_body_failed error=%v", traceID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var req struct {
		IdempotencyKey     string `json:"idempotency_key"`
		WalletID           string `json:"wallet_id"`
		Amount             int64  `json:"amount"`
		Currency           string `json:"currency"`
		PaymentRail        string `json:"payment_rail"`
		BeneficiaryDetails string `json:"beneficiary_details"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_json_decode_failed error=%v", traceID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	walletID := strings.TrimSpace(pathWalletID)
	if walletID == "" {
		walletID = strings.TrimSpace(req.WalletID)
	}
	if walletID == "" {
		walletID = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if walletID == "" {
		walletID = strings.TrimSpace(r.URL.Query().Get("wallet_id"))
	}
	if walletID == "" {
		http.Error(w, "wallet_id is required", http.StatusBadRequest)
		return
	}

	curr := db.NormalizeCurrency(req.Currency)
	if curr == "" {
		http.Error(w, "currency is required", http.StatusBadRequest)
		return
	}
	if req.Amount <= 0 {
		http.Error(w, "amount must be strictly positive", http.StatusBadRequest)
		return
	}
	if err := db.ValidateAmount(req.Amount, curr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	railProto := parseProtoPaymentRail(req.PaymentRail)
	if railProto == walletv1.PaymentRail_PAYMENT_RAIL_UNSPECIFIED {
		http.Error(w, "payment_rail must be one of CARD, ACH, SEPA, WIRE", http.StatusBadRequest)
		return
	}

	idempKey := strings.TrimSpace(req.IdempotencyKey)
	if idempKey == "" {
		idempKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if idempKey == "" {
		if os.Getenv("ENFORCE_CLIENT_IDEMPOTENCY") == "true" || r.Header.Get("X-Enforce-Idempotency") == "true" {
			http.Error(w, "Idempotency-Key is required for financial mutations", http.StatusBadRequest)
			return
		}
		idempKey = "idemp_wth_" + traceID
	}
	correlation.IdempotencyKey = idempKey

	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if err := ValidateWalletOwnership(claims, walletID); err != nil && !claims.HasRole(auth.RoleAdmin) {
			log.Printf("[SECURITY] IDOR blocked: subject %s tried to withdraw from wallet %s", claims.Subject, walletID)
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf(msgForbiddenFmt, err))
			return
		}
	}

	ctx := observability.WithCorrelation(r.Context(), correlation)
	g.emit(ctx, eventAPIRequestReceived, observability.LevelInfo, "HTTP withdraw funds request received", map[string]any{
		"operation":           "withdraw_funds",
		"wallet_id":           walletID,
		"currency":            curr,
		"amount":              req.Amount,
		"payment_rail":        req.PaymentRail,
		"beneficiary_details": req.BeneficiaryDetails,
	})

	protoReq := &walletv1.WithdrawFundsRequest{
		IdempotencyKey: idempKey,
		WalletId:       walletID,
		Amount: &walletv1.Money{
			Currency: curr,
			Units:    req.Amount,
		},
		PaymentRail:        railProto,
		BeneficiaryDetails: req.BeneficiaryDetails,
	}

	client, target := g.getActiveWalletClient(ctx)
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	resp, err := client.WithdrawFunds(callCtx, protoReq)
	if err != nil {
		log.Printf(traceWalletGRPCRespFmt, traceID, err)
		handleWalletGRPCError(w, err, "withdrawing funds")
		return
	}

	if resp.Status != walletv1.WithdrawFundsResponse_SUCCESS {
		statusHTTP := http.StatusBadRequest
		switch resp.Status {
		case walletv1.WithdrawFundsResponse_FAILED_INSUFFICIENT_FUNDS:
			statusHTTP = http.StatusUnprocessableEntity
		case walletv1.WithdrawFundsResponse_FAILED_VELOCITY_LIMIT_EXCEEDED:
			statusHTTP = http.StatusTooManyRequests
		}
		writeJSON(w, statusHTTP, map[string]interface{}{
			"status":         resp.Status.String(),
			"error_message":  resp.ErrorMessage,
			"transaction_id": resp.TransactionId,
			"routed_gateway": target,
			"handled_region": resp.HandledByRegion,
		})
		return
	}

	g.emit(ctx, eventAPIResponseSent, observability.LevelInfo, "Withdraw funds response sent", map[string]any{
		"operation":      "withdraw_funds",
		"transaction_id": resp.TransactionId,
		"wallet_id":      walletID,
		"currency":       curr,
		"amount":         req.Amount,
		"payment_rail":   req.PaymentRail,
	})

	if d := g.getWebhookDispatcher(); d != nil {
		bgCtx := context.WithoutCancel(r.Context())
		// #nosec G118 -- asynchronous webhook dispatch must decouple from HTTP request context cancellation
		go func() {
			_ = d.Dispatch(bgCtx, webhook.EventWithdrawalCompleted, map[string]interface{}{
				"transaction_id":      resp.TransactionId,
				"wallet_id":           walletID,
				"amount":              resp.GetDebitedAmount().GetUnits(),
				"currency":            resp.GetDebitedAmount().GetCurrency(),
				"payment_rail":        resp.PaymentRail.String(),
				"external_reference":  resp.ExternalReference,
				"beneficiary_details": req.BeneficiaryDetails,
				"handled_region":      resp.HandledByRegion,
			})
		}()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":             resp.Status.String(),
		"transaction_id":     resp.TransactionId,
		"wallet_id":          walletID,
		"amount":             resp.GetDebitedAmount().GetUnits(),
		"currency":           resp.GetDebitedAmount().GetCurrency(),
		"payment_rail":       resp.PaymentRail.String(),
		"external_reference": resp.ExternalReference,
		"routed_gateway":     target,
		"handled_region":     resp.HandledByRegion,
	})
}

func (g *Gateway) getWebhookDispatcher() *webhook.Dispatcher {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.webhookDispatcher
}

func (g *Gateway) handleWebhookSubscriptions(w http.ResponseWriter, r *http.Request) {
	d := g.getWebhookDispatcher()
	if d == nil {
		http.Error(w, "Webhook subsystem unavailable", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodPost:
		var req struct {
			URL       string   `json:"url"`
			TargetURL string   `json:"target_url"`
			Events    []string `json:"events"`
			Secret    string   `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		targetURL := req.URL
		if targetURL == "" {
			targetURL = req.TargetURL
		}
		if targetURL == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		if err := webhook.ValidateWebhookURL(targetURL); err != nil {
			http.Error(w, fmt.Sprintf("invalid webhook url: %v", err), http.StatusBadRequest)
			return
		}
		if len(req.Events) == 0 {
			req.Events = []string{"*"}
		}

		ownerID := ""
		if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
			ownerID = claims.Subject
		}

		sub := &webhook.WebhookSubscription{
			URL:     targetURL,
			Events:  req.Events,
			Secret:  req.Secret,
			OwnerID: ownerID,
		}

		if err := d.RegisterSubscription(r.Context(), sub); err != nil {
			http.Error(w, fmt.Sprintf("failed to register webhook: %v", err), http.StatusBadRequest)
			return
		}

		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"id":         sub.ID.Hex(),
			"url":        sub.URL,
			"events":     sub.Events,
			"secret":     sub.Secret,
			"is_active":  sub.IsActive,
			"created_at": sub.CreatedAt.Format(time.RFC3339),
		})

	case http.MethodGet:
		ownerID := ""
		if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
			if !claims.HasRole(auth.RoleAdmin) {
				ownerID = claims.Subject
			}
		}

		subs, err := d.ListSubscriptions(r.Context(), ownerID)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to list webhooks: %v", err), http.StatusInternalServerError)
			return
		}

		type subView struct {
			ID         string    `json:"id"`
			URL        string    `json:"url"`
			Events     []string  `json:"events"`
			SecretMask string    `json:"secret_mask"`
			IsActive   bool      `json:"is_active"`
			CreatedAt  time.Time `json:"created_at"`
		}

		views := make([]subView, 0, len(subs))
		for _, s := range subs {
			masked := "whsec_****"
			if len(s.Secret) > 10 {
				masked = s.Secret[:6] + "..." + s.Secret[len(s.Secret)-4:]
			}
			views = append(views, subView{
				ID:         s.ID.Hex(),
				URL:        s.URL,
				Events:     s.Events,
				SecretMask: masked,
				IsActive:   s.IsActive,
				CreatedAt:  s.CreatedAt,
			})
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"count":         len(views),
			"subscriptions": views,
		})

	case http.MethodDelete:
		idStr := strings.TrimSpace(r.URL.Query().Get("id"))
		if idStr == "" {
			http.Error(w, "missing 'id' query parameter", http.StatusBadRequest)
			return
		}
		oid, err := primitive.ObjectIDFromHex(idStr)
		if err != nil {
			http.Error(w, "invalid subscription id", http.StatusBadRequest)
			return
		}
		if err := d.DeleteSubscription(r.Context(), oid); err != nil {
			if err == mongo.ErrNoDocuments {
				http.Error(w, "subscription not found", http.StatusNotFound)
				return
			}
			http.Error(w, fmt.Sprintf("failed to delete subscription: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "subscription deactivated successfully"})

	default:
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
	}
}

func (g *Gateway) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	d := g.getWebhookDispatcher()
	if d == nil {
		http.Error(w, "Webhook subsystem unavailable", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		URL       string `json:"url"`
		TargetURL string `json:"target_url"`
		Secret    string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	targetURL := req.URL
	if targetURL == "" {
		targetURL = req.TargetURL
	}
	if targetURL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	if err := webhook.ValidateWebhookURL(targetURL); err != nil {
		http.Error(w, fmt.Sprintf("invalid webhook url: %v", err), http.StatusBadRequest)
		return
	}
	if req.Secret == "" {
		req.Secret = "whsec_test_secret_for_ping"
	}

	code, latency, err := d.TestEndpoint(r.Context(), targetURL, req.Secret)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"success":    false,
			"url":        targetURL,
			"error":      err.Error(),
			"latency_ms": latency.Milliseconds(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":     code >= 200 && code < 300,
		"url":         targetURL,
		"status_code": code,
		"latency_ms":  latency.Milliseconds(),
	})
}

func (g *Gateway) handleLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	walletID := strings.TrimSpace(r.URL.Query().Get("wallet_id"))
	if walletID == "" {
		http.Error(w, "wallet_id query parameter is required", http.StatusBadRequest)
		return
	}
	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if err := ValidateWalletOwnership(claims, walletID); err != nil {
			log.Printf("[SECURITY] IDOR blocked: subject %s tried to view ledger of %s", claims.Subject, walletID)
			writeAuthError(w, http.StatusForbidden, fmt.Sprintf(msgForbiddenFmt, err))
			return
		}
	}
	limitStr := r.URL.Query().Get("limit")
	pageToken := r.URL.Query().Get("page_token")

	var limit int32 = 20
	if limitStr != "" {
		if parsedLimit, err := strconv.ParseInt(limitStr, 10, 32); err == nil && parsedLimit > 0 {
			limit = int32(parsedLimit)
		}
	}

	correlation := observability.FromHTTPRequest(r)
	ctx := observability.WithCorrelation(r.Context(), correlation)
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	resp, err := g.ledgerClient.GetLedgerEntries(callCtx, &ledgerv1.GetLedgerRequest{
		WalletId:  walletID,
		Limit:     limit,
		PageToken: pageToken,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Ledger query failure: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"wallet_id":       walletID,
		"entries":         resp.Entries,
		"next_page_token": resp.NextPageToken,
		"total_count":     resp.TotalCount,
	})
}

// DEV-03: handleLedgerMerkle proxies cryptographic Merkle checkpoint and audit proof requests.
func (g *Gateway) handleLedgerMerkle(w http.ResponseWriter, r *http.Request) {
	mgmtURL := g.ledgerMgmtURL
	if mgmtURL == "" {
		mgmtURL = resolveLedgerMgmtURL()
	}
	mgmtURL = strings.TrimRight(mgmtURL, "/")

	subPath := strings.TrimPrefix(r.URL.Path, "/api/v1/ledger/merkle")
	if subPath == "" || subPath == "/" {
		subPath = "/latest"
	}
	var allowedAuditSubpaths = map[string]bool{
		"/latest":      true,
		"/checkpoints": true,
		"/proof":       true,
		"/checkpoint":  true,
	}
	if !allowedAuditSubpaths[subPath] {
		http.Error(w, "Invalid audit merkle subpath", http.StatusBadRequest)
		return
	}
	targetURL := mgmtURL + "/audit/merkle" + subPath
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	client := g.httpClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// #nosec G704 -- internal gateway proxy to ledger-service with strictly allowlisted subpaths
	proxyReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to build proxy request: %v", err), http.StatusInternalServerError)
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")

	// #nosec G704 -- internal proxy dispatch to verified ledger management backend
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Ledger service unavailable: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (g *Gateway) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	correlation := observability.FromHTTPRequest(r)
	ctx := observability.WithCorrelation(r.Context(), correlation)
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	callCtx = observability.WithOutgoingMetadata(callCtx, correlation)
	pHealth, _ := g.primaryClient.HealthCheck(callCtx, &walletv1.HealthRequest{})
	sHealth, _ := g.standbyClient.HealthCheck(callCtx, &walletv1.HealthRequest{})

	currentActive := coordinator.TargetPrimary
	if g.coordinator != nil {
		if t, err := g.coordinator.GetActiveTarget(callCtx); err == nil && t != "" {
			currentActive = t
		}
	} else {
		g.mu.RLock()
		if g.activeTarget != "" {
			currentActive = g.activeTarget
		}
		g.mu.RUnlock()
	}

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

func (g *Gateway) resolveCurrentTarget(ctx context.Context) string {
	if g.coordinator != nil {
		if t, err := g.coordinator.GetActiveTarget(ctx); err == nil && t != "" {
			return t
		}
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.activeTarget != "" {
		return g.activeTarget
	}
	return coordinator.TargetPrimary
}

func (g *Gateway) updateFailoverTarget(ctx context.Context, newTarget string) error {
	if g.coordinator != nil {
		if err := g.coordinator.SetActiveTarget(ctx, newTarget); err != nil {
			return err
		}
	}
	g.mu.Lock()
	g.activeTarget = newTarget
	g.mu.Unlock()
	return nil
}

func (g *Gateway) handleFailover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if !claims.HasRole(auth.RoleAdmin) && !claims.HasScope(auth.ScopeClusterAdmin) {
			log.Printf("[SECURITY] Failover unauthorized attempt by subject: %s", claims.Subject)
			writeAuthError(w, http.StatusForbidden, "Forbidden: cluster failover requires admin privileges")
			return
		}
	}

	currentTarget := g.resolveCurrentTarget(r.Context())
	newTarget := coordinator.TargetStandby
	if currentTarget == coordinator.TargetStandby {
		newTarget = coordinator.TargetPrimary
	}

	if err := g.updateFailoverTarget(r.Context(), newTarget); err != nil {
		log.Printf("[GATEWAY-FAILOVER] Failed to persist failover target: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist failover target"})
		return
	}

	observability.DefaultMetrics.SetClusterActiveTarget(apiGatewayServiceName, newTarget)
	log.Printf("[GATEWAY-FAILOVER] Switched traffic route to: %s", newTarget)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":              "Failover routing triggered",
		"active_routed_target": newTarget,
	})
}

// handleLogin authenticates user credentials via the auth-service and returns a JWT token pair.
// This replaces the old in-process handleAuthToken which issued tokens without credential verification.
func (g *Gateway) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		Scopes   []string `json:"scopes,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp, err := g.authClient.IssueToken(ctx, &authv1.IssueTokenRequest{
		Username:        req.Username,
		Password:        req.Password,
		RequestedScopes: req.Scopes,
	})
	if err != nil {
		if isUnauthenticated(err) {
			writeAuthError(w, http.StatusUnauthorized, "Invalid credentials")
			return
		}
		http.Error(w, fmt.Sprintf("Login failed: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"access_token":  resp.AccessToken,
		"refresh_token": resp.RefreshToken,
		"token_type":    resp.TokenType,
		"expires_in":    resp.ExpiresIn,
		"scopes":        resp.GrantedScopes,
		"subject":       resp.Subject,
	})
}

// handleRefresh exchanges a refresh token for a new access token.
func (g *Gateway) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RefreshToken == "" {
		http.Error(w, "refresh_token is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp, err := g.authClient.RefreshToken(ctx, &authv1.RefreshTokenRequest{RefreshToken: req.RefreshToken})
	if err != nil {
		if isUnauthenticated(err) {
			writeAuthError(w, http.StatusUnauthorized, "Invalid or expired refresh token")
			return
		}
		http.Error(w, fmt.Sprintf("Token refresh failed: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"access_token":  resp.AccessToken,
		"refresh_token": resp.RefreshToken,
		"token_type":    resp.TokenType,
		"expires_in":    resp.ExpiresIn,
		"scopes":        resp.GrantedScopes,
		"subject":       resp.Subject,
	})
}

// handleLogout revokes the caller's token via the auth-service.
func (g *Gateway) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	authHeader := r.Header.Get("Authorization")
	tokenStr, err := auth.ExtractBearerToken(authHeader)
	if err != nil {
		writeAuthError(w, http.StatusUnauthorized, "Bearer token required for logout")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	_, _ = g.authClient.RevokeToken(ctx, &authv1.RevokeTokenRequest{
		Token:  tokenStr,
		Reason: "logout",
	})

	writeJSON(w, http.StatusOK, map[string]string{"message": "logged out successfully"})
}

func parseAuthTokenParams(r *http.Request) (string, string, string) {
	sub := r.URL.Query().Get("sub")
	role := r.URL.Query().Get("role")
	scope := r.URL.Query().Get("scope")

	if r.Method == http.MethodPost && r.Body != nil {
		var req struct {
			Subject string   `json:"subject"`
			Roles   []string `json:"roles"`
			Scopes  []string `json:"scopes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			if req.Subject != "" {
				sub = req.Subject
			}
			if len(req.Roles) > 0 {
				role = req.Roles[0]
			}
			if len(req.Scopes) > 0 {
				scope = strings.Join(req.Scopes, ",")
			}
		}
	}

	if sub == "" {
		sub = "alice"
	}
	if role == "" {
		role = auth.RoleUser
	}
	return sub, role, scope
}

func buildAuthTokenClaims(sub, role, scope string) (auth.Claims, []string, []string) {
	var roles []string
	if role != "" {
		roles = []string{role}
	}
	var scopes []string
	if scope != "" {
		scopes = strings.Split(scope, ",")
	} else if role == auth.RoleAdmin {
		scopes = []string{auth.ScopeClusterAdmin, auth.ScopeLedgerAudit, auth.ScopeWalletTransfer, auth.ScopeWalletRead}
	} else {
		scopes = []string{auth.ScopeWalletTransfer, auth.ScopeWalletRead}
	}

	return auth.Claims{
		Subject: sub,
		Roles:   roles,
		Scopes:  scopes,
	}, roles, scopes
}

// handleAuthToken provides development-only token minting (/api/v1/auth/token).
// In production, this endpoint is strictly disabled (VULN-01 fix); clients must use /api/v1/auth/login.
func (g *Gateway) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}

	if os.Getenv("ENABLE_DEV_TOKEN_ENDPOINT") != "true" {
		writeAuthError(w, http.StatusForbidden, "Token minting endpoint /api/v1/auth/token is disabled. Set ENABLE_DEV_TOKEN_ENDPOINT=true in development or use /api/v1/auth/login.")
		return
	}

	sub, role, scope := parseAuthTokenParams(r)
	claims, roles, scopes := buildAuthTokenClaims(sub, role, scope)

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = defaultJWTSecret
	}

	token, err := auth.GenerateToken(claims, secret)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to generate token: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":        token,
		"access_token": token,
		"token_type":   "Bearer",
		"subject":      sub,
		"roles":        roles,
		"scopes":       scopes,
		"expires_in":   3600,
	})
}

// isUnauthenticated returns true if the gRPC error code is Unauthenticated.
func isUnauthenticated(err error) bool {
	if st, ok := status.FromError(err); ok {
		return st.Code() == codes.Unauthenticated
	}
	return false
}

func getClientDialOption() (grpc.DialOption, error) {
	if os.Getenv("GRPC_TLS_ENABLED") == "true" {
		certFile := os.Getenv("GRPC_CLIENT_CERT")
		keyFile := os.Getenv("GRPC_CLIENT_KEY")
		caFile := os.Getenv("GRPC_CA_CERT")
		serverName := os.Getenv("GRPC_SERVER_NAME")
		creds, err := tlsutil.NewClientTransportCredentials(certFile, keyFile, caFile, serverName)
		if err != nil {
			return nil, fmt.Errorf("failed to load client mTLS credentials: %w", err)
		}
		log.Println("[SECURITY] Outbound gRPC dialed with mTLS transport credentials")
		return grpc.WithTransportCredentials(creds), nil
	}
	return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
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

func resolveServiceAddress(envVar, hostName, defaultPort string) string {
	if addr := os.Getenv(envVar); addr != "" {
		return addr
	}
	if _, err := net.LookupHost(hostName); err == nil {
		return hostName + ":" + defaultPort
	}
	return "127.0.0.1:" + defaultPort
}

func resolveGatewayServiceAddresses() (primary, standby, ledger, auth string) {
	primary = resolveServiceAddress("PRIMARY_WALLET_ADDR", "wallet-primary", "50051")
	standby = resolveServiceAddress("STANDBY_WALLET_ADDR", "wallet-standby", "50053")
	ledger = resolveServiceAddress("LEDGER_ADDR", "ledger-service", "50052")
	auth = resolveServiceAddress("AUTH_SERVICE_ADDR", "auth-service", "50054")
	return primary, standby, ledger, auth
}

func resolveLedgerMgmtURL() string {
	if u := os.Getenv("LEDGER_MGMT_URL"); u != "" {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return "http://" + u
		}
		return u
	}
	host := resolveServiceAddress("LEDGER_MGMT_ADDR", "ledger-service", "9092")
	return "http://" + host
}

func initFailoverCoordinator(mongoURI string) (coordinator.FailoverCoordinator, func()) {
	if mongoURI != "" {
		mCtx, mCancel := context.WithTimeout(context.Background(), 3*time.Second)
		mClient, mErr := db.ConnectWithRetry(mCtx, mongoURI, 3)
		mCancel()
		if mErr == nil {
			coord := coordinator.NewMongoFailoverCoordinator(mClient.Database("banking_db"), 1*time.Second)
			log.Println("[API-GATEWAY] Distributed MongoFailoverCoordinator connected")
			return coord, func() {
				coord.Close()
				_ = mClient.Disconnect(context.Background())
			}
		}
		log.Printf("[API-GATEWAY] Mongo connection failed for failover coordinator (%v), falling back to in-memory coordinator", mErr)
	}
	coord := coordinator.NewMemoryFailoverCoordinator(coordinator.TargetPrimary)
	return coord, func() { coord.Close() }
}

func (gw *Gateway) handleWallets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		gw.handleCreateWallet(w, r)
	case http.MethodGet:
		gw.handleGetBalance(w, r)
	default:
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
	}
}

func (gw *Gateway) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	client, activeTarget := gw.getActiveWalletClient(ctx)
	wHealth, wErr := client.HealthCheck(ctx, &walletv1.HealthRequest{})
	if wErr != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"status":        "DEGRADED",
			"active_target": activeTarget,
			"error":         fmt.Sprintf("active wallet health check failed: %v", wErr),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "READY",
		"active_target": activeTarget,
		"wallet_status": wHealth.Status,
	})
}

func (gw *Gateway) handleFXCurrencies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}
	currencies := db.ListSupportedCurrencies()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"standard":   "ISO-4217",
		"count":      len(currencies),
		"currencies": currencies,
	})
}

func (gw *Gateway) handleFXRates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}
	if gw.fxClient == nil {
		http.Error(w, "FX service unavailable", http.StatusServiceUnavailable)
		return
	}
	base := strings.TrimSpace(r.URL.Query().Get("base"))
	if base == "" {
		base = strings.TrimSpace(r.URL.Query().Get("base_currency"))
	}
	if base == "" {
		base = "USD"
	}
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target == "" {
		target = strings.TrimSpace(r.URL.Query().Get("target_currency"))
	}

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()

	if target != "" {
		resp, err := gw.fxClient.GetExchangeRate(ctx, &fxv1.GetExchangeRateRequest{
			BaseCurrency:   base,
			TargetCurrency: target,
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("FX rate lookup failed: %v", err), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, resp.Rate)
		return
	}

	listResp, err := gw.fxClient.ListExchangeRates(ctx, &fxv1.ListExchangeRatesRequest{
		BaseCurrency: base,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("FX rate list failed: %v", err), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, listResp)
}

func (gw *Gateway) handleFXQuotes(w http.ResponseWriter, r *http.Request) {
	if gw.fxClient == nil {
		http.Error(w, "FX service unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()

	if r.Method == http.MethodGet {
		quoteID := strings.TrimSpace(r.URL.Query().Get("quote_id"))
		if quoteID == "" {
			quoteID = strings.TrimSpace(r.URL.Query().Get("id"))
		}
		resp, err := gw.fxClient.GetQuote(ctx, &fxv1.GetQuoteRequest{QuoteId: quoteID})
		if err != nil {
			http.Error(w, fmt.Sprintf("FX quote lookup failed: %v", err), http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, resp.Quote)
		return
	}

	if r.Method == http.MethodPost {
		var req struct {
			BaseCurrency   string `json:"base_currency"`
			TargetCurrency string `json:"target_currency"`
			SourceAmount   int64  `json:"source_amount"`
			Amount         int64  `json:"amount"`
			ClientID       string `json:"client_id"`
			TTLSeconds     int32  `json:"ttl_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		amt := req.SourceAmount
		if amt <= 0 {
			amt = req.Amount
		}
		clientID := req.ClientID
		if clientID == "" {
			if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
				clientID = claims.Subject
			}
		}
		resp, err := gw.fxClient.CreateQuote(ctx, &fxv1.CreateQuoteRequest{
			BaseCurrency:   req.BaseCurrency,
			TargetCurrency: req.TargetCurrency,
			SourceAmount:   amt,
			ClientId:       clientID,
			TtlSeconds:     req.TTLSeconds,
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("FX quote creation failed: %v", err), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, resp.Quote)
		return
	}

	http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
}

func (gw *Gateway) handleFXConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}
	if gw.fxClient == nil {
		http.Error(w, "FX service unavailable", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		IdempotencyKey string `json:"idempotency_key"`
		QuoteID        string `json:"quote_id"`
		BaseCurrency   string `json:"base_currency"`
		TargetCurrency string `json:"target_currency"`
		SourceAmount   int64  `json:"source_amount"`
		Amount         int64  `json:"amount"`
		ClientID       string `json:"client_id"`
		ReferenceID    string `json:"reference_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	amt := req.SourceAmount
	if amt <= 0 {
		amt = req.Amount
	}
	clientID := req.ClientID
	if clientID == "" {
		if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
			clientID = claims.Subject
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()

	resp, err := gw.fxClient.ConvertCurrency(ctx, &fxv1.ConvertCurrencyRequest{
		IdempotencyKey: req.IdempotencyKey,
		QuoteId:        req.QuoteID,
		BaseCurrency:   req.BaseCurrency,
		TargetCurrency: req.TargetCurrency,
		SourceAmount:   amt,
		ClientId:       clientID,
		ReferenceId:    req.ReferenceID,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("FX conversion failed: %v", err), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (gw *Gateway) handleFXRatesRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
		return
	}
	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if !claims.HasRole(auth.RoleAdmin) && !claims.HasScope(auth.ScopeClusterAdmin) {
			writeAuthError(w, http.StatusForbidden, "Forbidden: FX rate refresh/override requires admin privileges")
			return
		}
	}
	if gw.fxClient == nil {
		http.Error(w, "FX service unavailable", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		BaseCurrency        string  `json:"base_currency"`
		TargetCurrency      string  `json:"target_currency"`
		MidRate             float64 `json:"mid_rate"`
		SpreadBps           int32   `json:"spread_bps"`
		TriggerUpstreamSync bool    `json:"trigger_upstream_sync"`
		SourceProvider      string  `json:"source_provider"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.BaseCurrency == "" && req.TargetCurrency == "" && req.MidRate == 0 {
		req.TriggerUpstreamSync = true
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	resp, err := gw.fxClient.UpdateExchangeRate(ctx, &fxv1.UpdateExchangeRateRequest{
		BaseCurrency:        req.BaseCurrency,
		TargetCurrency:      req.TargetCurrency,
		MidRate:             req.MidRate,
		SpreadBps:           req.SpreadBps,
		TriggerUpstreamSync: req.TriggerUpstreamSync,
		SourceProvider:      req.SourceProvider,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("FX rate update failed: %v", err), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func registerGatewayRoutes(mux *http.ServeMux, gw *Gateway) {
	mux.HandleFunc("/api/v1/wallets", gw.handleWallets)
	mux.HandleFunc("/api/v1/wallets/", gw.handleWalletSubroutes)
	mux.HandleFunc("/api/v1/wallets/deposit", gw.handleDepositFunds)
	mux.HandleFunc("/api/v1/wallets/withdraw", gw.handleWithdrawFunds)
	mux.HandleFunc("/api/v1/wallets/exchange", gw.handleWalletExchange)
	mux.HandleFunc("/api/v1/wallets/kyc", gw.handleUpdateWalletKYC)
	mux.HandleFunc("/api/v1/wallets/limits", gw.handleGetWalletLimits)
	mux.HandleFunc("/api/v1/webhooks/subscriptions", gw.handleWebhookSubscriptions)
	mux.HandleFunc("/api/v1/webhooks/test", gw.handleWebhookTest)
	mux.HandleFunc("/api/v1/transfers", gw.handleTransfer)
	mux.HandleFunc("/api/v1/ledger", gw.handleLedger)
	mux.HandleFunc("/api/v1/ledger/merkle", gw.handleLedgerMerkle)
	mux.HandleFunc("/api/v1/ledger/merkle/", gw.handleLedgerMerkle)
	mux.HandleFunc("/api/v1/fx/currencies", gw.handleFXCurrencies)
	mux.HandleFunc("/api/v1/fx/rates", gw.handleFXRates)
	mux.HandleFunc("/api/v1/fx/quotes", gw.handleFXQuotes)
	mux.HandleFunc("/api/v1/fx/convert", gw.handleFXConvert)
	mux.HandleFunc("/api/v1/fx/rates/refresh", gw.handleFXRatesRefresh)
	mux.HandleFunc("/api/v1/cluster/status", gw.handleClusterStatus)
	mux.HandleFunc("/api/v1/cluster/failover", gw.handleFailover)
	mux.HandleFunc("/api/v1/auth/login", gw.handleLogin)
	mux.HandleFunc("/api/v1/auth/refresh", gw.handleRefresh)
	mux.HandleFunc("/api/v1/auth/logout", gw.handleLogout)
	mux.HandleFunc("/api/v1/auth/token", gw.handleAuthToken)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	})
	mux.HandleFunc("/readyz", gw.handleReadyz)
	mux.Handle("/metrics", observability.DefaultMetrics.Handler())

	// DEV-01: OpenAPI 3.0 & Embedded Swagger UI
	mux.HandleFunc("/swagger", handleSwaggerRedirect)
	mux.HandleFunc("/swagger/", handleSwaggerUI)
	mux.HandleFunc("/docs", handleSwaggerRedirect)
	mux.HandleFunc("/docs/", handleSwaggerUI)
	mux.HandleFunc("/api-docs/openapi.json", handleOpenAPISpec)
}

func wrapGatewayMiddleware(handler http.Handler, authClient authv1.AuthServiceClient) http.Handler {
	publicPaths := map[string]bool{
		"/healthz":                          true,
		"/readyz":                           true,
		"/metrics":                          true,
		"/swagger":                          true,
		"/swagger/":                         true,
		"/docs":                             true,
		"/docs/":                            true,
		"/api-docs/openapi.json":            true,
		"/api/v1/auth/login":                true,
		"/api/v1/auth/refresh":              true,
		"/api/v1/cluster/status":            true,
		"/api/v1/ledger/merkle":             true,
		"/api/v1/ledger/merkle/latest":      true,
		"/api/v1/ledger/merkle/checkpoints": true,
		"/api/v1/ledger/merkle/proof":       true,
		"/api/v1/fx/currencies":             true,
		"/api/v1/fx/rates":                  true,
	}

	if os.Getenv("ENABLE_DEV_TOKEN_ENDPOINT") == "true" {
		publicPaths["/api/v1/auth/token"] = true
	}

	cacheFingerprintSecret := os.Getenv("CACHE_FINGERPRINT_SECRET")
	if cacheFingerprintSecret == "" {
		cacheFingerprintSecret = "gateway-cache-fingerprint-key"
	}
	claimsC := newClaimsCache(cacheFingerprintSecret)
	rateLimiter := NewRateLimiter(60, 100)

	wrapped := MaxBytesMiddleware(1<<20, handler)
	wrapped = CORSMiddleware(wrapped)
	wrapped = SecurityHeadersMiddleware(wrapped)
	wrapped = RateLimitMiddleware(rateLimiter, wrapped)
	wrapped = AuthMiddleware(authClient, claimsC, publicPaths, wrapped)
	return observability.TraceHTTPMiddleware(apiGatewayServiceName, wrapped)
}

func buildGatewayRouter(gw *Gateway) http.Handler {
	mux := http.NewServeMux()
	registerGatewayRoutes(mux, gw)
	return wrapGatewayMiddleware(mux, gw.authClient)
}

func runGatewayServer(ctx context.Context) error {
	httpPort := os.Getenv("HTTP_PORT")
	if httpPort == "" {
		httpPort = "8080"
	}
	primaryAddr, standbyAddr, ledgerAddr, authAddr := resolveGatewayServiceAddresses()
	fxAddr := resolveServiceAddress("FX_SERVICE_ADDR", "fx-service", "50055")

	log.Println("[API-GATEWAY] Establishing gRPC connections...")
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "local"
	}
	logger := observability.LoggerFromEnvironment(apiGatewayServiceName, environment, "", os.Stdout)

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	shutdownTracer, err := observability.InitTracer(apiGatewayServiceName)
	if err == nil && shutdownTracer != nil {
		defer func() { _ = shutdownTracer(context.Background()) }()
	}

	dialOpt, err := getClientDialOption()
	if err != nil {
		return fmt.Errorf("failed to initialize gRPC dial credentials: %w", err)
	}

	makeDialOpts := func(serviceName string) []grpc.DialOption {
		cb := resilience.NewClientCircuitBreaker(resilience.DefaultCircuitBreakerConfig(serviceName))
		return []grpc.DialOption{
			dialOpt,
			grpc.WithChainUnaryInterceptor(
				observability.UnaryClientTraceInterceptor(apiGatewayServiceName),
				resilience.UnaryClientCircuitBreakerInterceptor(cb),
			),
		}
	}

	aConn, err := grpc.NewClient(authAddr, makeDialOpts("auth-service")...)
	if err != nil {
		return fmt.Errorf("failed to dial auth-service: %w", err)
	}
	defer aConn.Close()

	pConn, err := grpc.NewClient(primaryAddr, makeDialOpts("wallet-primary")...)
	if err != nil {
		return fmt.Errorf("failed to dial primary wallet: %w", err)
	}
	defer pConn.Close()

	sConn, err := grpc.NewClient(standbyAddr, makeDialOpts("wallet-standby")...)
	if err != nil {
		return fmt.Errorf("failed to dial standby wallet: %w", err)
	}
	defer sConn.Close()

	lConn, err := grpc.NewClient(ledgerAddr, makeDialOpts("ledger-service")...)
	if err != nil {
		return fmt.Errorf("failed to dial ledger: %w", err)
	}
	defer lConn.Close()

	fxConn, fxErr := grpc.NewClient(fxAddr, makeDialOpts("fx-service")...)
	var fxClient fxv1.FXServiceClient
	if fxErr == nil {
		defer fxConn.Close()
		fxClient = fxv1.NewFXServiceClient(fxConn)
	}

	mongoURI := os.Getenv("MONGO_URI")
	coord, cleanupCoord := initFailoverCoordinator(mongoURI)
	defer cleanupCoord()

	var whDispatcher *webhook.Dispatcher
	if mongoURI != "" {
		mCtx, mCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if mClient, err := db.ConnectWithRetry(mCtx, mongoURI, 3); err == nil {
			database := mClient.Database("banking_db")
			_ = db.EnsureWebhookIndexes(context.Background(), database)
			whDispatcher = webhook.NewDispatcher(database, nil)
			go whDispatcher.StartRetryWorker(subCtx, 5*time.Second)
		}
		mCancel()
	}
	if whDispatcher == nil {
		whDispatcher = webhook.NewDispatcher(nil, nil)
	}

	gw := &Gateway{
		activeTarget:      "PRIMARY",
		coordinator:       coord,
		primaryClient:     walletv1.NewWalletServiceClient(pConn),
		standbyClient:     walletv1.NewWalletServiceClient(sConn),
		ledgerClient:      ledgerv1.NewLedgerServiceClient(lConn),
		authClient:        authv1.NewAuthServiceClient(aConn),
		fxClient:          fxClient,
		primaryAddress:    primaryAddr,
		standbyAddress:    standbyAddr,
		ledgerMgmtURL:     resolveLedgerMgmtURL(),
		httpClient:        &http.Client{Timeout: 5 * time.Second},
		logger:            logger,
		webhookDispatcher: whDispatcher,
	}

	if initTarget, err := coord.GetActiveTarget(subCtx); err == nil {
		observability.DefaultMetrics.SetClusterActiveTarget(apiGatewayServiceName, initTarget)
	}

	handler := buildGatewayRouter(gw)

	server := &http.Server{
		Addr:              ":" + httpPort,
		Handler:           handler,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("[API-GATEWAY] HTTP REST Gateway listening on :%s (Zero-Trust Security, Distributed Failover & OTel enabled)", httpPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Gateway server error: %v", err)
		}
	}()

	<-subCtx.Done()
	log.Printf("[API-GATEWAY] Context cancelled, initiating graceful shutdown...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[API-GATEWAY] HTTP server graceful shutdown error: %v", err)
	}
	log.Println("[API-GATEWAY] Graceful shutdown completed cleanly.")
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runGatewayServer(ctx); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
}
