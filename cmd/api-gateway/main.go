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

	"wallet-system/pkg/auth"
	"wallet-system/pkg/coordinator"
	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	"wallet-system/pkg/tlsutil"
	authv1 "wallet-system/proto/auth"
	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

const defaultJWTSecret = "development-wallet-insecure-secret-key-change-in-prod"

// Gateway routes incoming HTTP requests to the appropriate gRPC backend service.
type Gateway struct {
	mu             sync.RWMutex
	activeTarget   string // "PRIMARY" or "STANDBY" (cached fallback)
	coordinator    coordinator.FailoverCoordinator
	primaryClient  walletv1.WalletServiceClient
	standbyClient  walletv1.WalletServiceClient
	ledgerClient   ledgerv1.LedgerServiceClient
	authClient     authv1.AuthServiceClient // auth-service gRPC client
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
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[TRACE] trace_id=%s step=http_json_decode_failed error=%v", traceID, err)
		return nil, err
	}
	if !db.IsValidCurrency(req.Currency) {
		return nil, fmt.Errorf("Invalid or unsupported currency: %s", req.Currency)
	}
	if req.InitialBalance < 0 {
		return nil, errors.New("Initial balance cannot be negative")
	}
	if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
		if err := ValidateWalletOwnership(claims, req.WalletID); err != nil {
			log.Printf("[SECURITY] IDOR blocked: subject %s tried to create wallet for %s", claims.Subject, req.WalletID)
			return nil, fmt.Errorf("forbidden: %w", err)
		}
	}
	log.Printf("[TRACE] trace_id=%s step=http_json_decoded operation=create_wallet wallet_id=%s", traceID, req.WalletID)
	return &walletv1.CreateWalletRequest{WalletId: req.WalletID, Currency: req.Currency, InitialBalance: req.InitialBalance}, nil
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
	if req.IdempotencyKey != "" {
		correlation.IdempotencyKey = req.IdempotencyKey
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
	})
	traceID = correlation.AssociationID
	log.Printf("[TRACE] trace_id=%s step=http_json_decoded operation=transfer source=%s destination=%s amount=%d currency=%s", traceID, sourceWallet, destWallet, req.Amount, req.Currency)
	protoReq := &walletv1.TransferFundsRequest{
		IdempotencyKey:      req.IdempotencyKey,
		SourceWalletId:      sourceWallet,
		DestinationWalletId: destWallet,
		Amount:              &walletv1.Money{Currency: req.Currency, Units: req.Amount},
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
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
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

// handleAuthToken provides backward-compatible token minting (/api/v1/auth/token)
// supporting legacy Bruno/Postman collections and rapid development testing.
func (g *Gateway) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
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
	if r.Method == http.MethodPost {
		gw.handleCreateWallet(w, r)
	} else if r.Method == http.MethodGet {
		gw.handleGetBalance(w, r)
	} else {
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

func registerGatewayRoutes(mux *http.ServeMux, gw *Gateway) {
	mux.HandleFunc("/api/v1/wallets", gw.handleWallets)
	mux.HandleFunc("/api/v1/transfers", gw.handleTransfer)
	mux.HandleFunc("/api/v1/ledger", gw.handleLedger)
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
}

func wrapGatewayMiddleware(handler http.Handler, authClient authv1.AuthServiceClient) http.Handler {
	publicPaths := map[string]bool{
		"/healthz":               true,
		"/readyz":                true,
		"/metrics":               true,
		"/api/v1/auth/login":     true,
		"/api/v1/auth/refresh":   true,
		"/api/v1/auth/token":     true,
		"/api/v1/cluster/status": true,
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
	dialOpts := []grpc.DialOption{
		dialOpt,
		grpc.WithUnaryInterceptor(observability.UnaryClientTraceInterceptor(apiGatewayServiceName)),
	}

	aConn, err := grpc.NewClient(authAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("failed to dial auth-service: %w", err)
	}
	defer aConn.Close()

	pConn, err := grpc.NewClient(primaryAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("failed to dial primary wallet: %w", err)
	}
	defer pConn.Close()

	sConn, err := grpc.NewClient(standbyAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("failed to dial standby wallet: %w", err)
	}
	defer sConn.Close()

	lConn, err := grpc.NewClient(ledgerAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("failed to dial ledger: %w", err)
	}
	defer lConn.Close()

	coord, cleanupCoord := initFailoverCoordinator(os.Getenv("MONGO_URI"))
	defer cleanupCoord()

	gw := &Gateway{
		activeTarget:   "PRIMARY",
		coordinator:    coord,
		primaryClient:  walletv1.NewWalletServiceClient(pConn),
		standbyClient:  walletv1.NewWalletServiceClient(sConn),
		ledgerClient:   ledgerv1.NewLedgerServiceClient(lConn),
		authClient:     authv1.NewAuthServiceClient(aConn),
		primaryAddress: primaryAddr,
		standbyAddress: standbyAddr,
		logger:         logger,
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
