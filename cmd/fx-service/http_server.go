package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	fxv1 "wallet-system/proto/fx"
)

func writeFXJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func fxCORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Client-ID, X-Idempotency-Key, X-Association-ID")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func fxPartnerAuthMiddleware(apiKey string, next http.Handler) http.Handler {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return next
	}
	validKeys := make(map[string]bool)
	for _, k := range strings.Split(apiKey, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			validKeys[k] = true
		}
	}
	if len(validKeys) == 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			headerKey := strings.TrimSpace(r.Header.Get("X-API-Key"))
			authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
			var bearer string
			if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
				bearer = strings.TrimSpace(authHeader[7:])
			} else if authHeader != "" {
				bearer = authHeader
			}

			if !validKeys[headerKey] && !validKeys[bearer] {
				writeFXJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "Unauthorized: valid X-API-Key or Authorization Bearer token required for 3rd-party FX access",
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func buildFXHTTPHandler(srv *fxServer, apiKey string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		prov, curCount, pairCount, syncedAt := srv.engine.HealthSummary()
		writeFXJSON(w, http.StatusOK, map[string]any{
			"status":               "SERVING",
			"service":              fxServiceName,
			"source_provider":      prov,
			"supported_currencies": curCount,
			"active_pairs":         pairCount,
			"last_synced_at":       syncedAt,
		})
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		prov, _, pairCount, syncedAt := srv.engine.HealthSummary()
		if pairCount == 0 {
			writeFXJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "NOT_READY"})
			return
		}
		writeFXJSON(w, http.StatusOK, map[string]any{
			"status":          "READY",
			"source_provider": prov,
			"active_pairs":    pairCount,
			"last_synced_at":  syncedAt,
		})
	})

	mux.HandleFunc("/api/v1/fx/currencies", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeFXJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
			return
		}
		currencies := db.ListSupportedCurrencies()
		writeFXJSON(w, http.StatusOK, map[string]any{
			"standard":   "ISO-4217",
			"count":      len(currencies),
			"currencies": currencies,
		})
	})

	mux.HandleFunc("/api/v1/fx/rates", srv.handleHTTPRates)
	mux.HandleFunc("/api/v1/fx/quotes", srv.handleHTTPQuotes)
	mux.HandleFunc("/api/v1/fx/convert", srv.handleHTTPConvert)
	mux.HandleFunc("/api/v1/fx/rates/refresh", srv.handleHTTPRefreshRates)

	wrapped := fxPartnerAuthMiddleware(apiKey, mux)
	wrapped = fxCORSMiddleware(wrapped)
	return observability.TraceHTTPMiddleware(fxServiceName, wrapped)
}

func (s *fxServer) handleHTTPRates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeFXJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
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

	if target != "" {
		resp, err := s.GetExchangeRate(r.Context(), &fxv1.GetExchangeRateRequest{
			BaseCurrency:   base,
			TargetCurrency: target,
		})
		if err != nil {
			writeFXJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeFXJSON(w, http.StatusOK, resp.Rate)
		return
	}

	listResp, err := s.ListExchangeRates(r.Context(), &fxv1.ListExchangeRatesRequest{
		BaseCurrency: base,
	})
	if err != nil {
		writeFXJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeFXJSON(w, http.StatusOK, listResp)
}

func (s *fxServer) handleHTTPQuotes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		quoteID := strings.TrimSpace(r.URL.Query().Get("quote_id"))
		if quoteID == "" {
			quoteID = strings.TrimSpace(r.URL.Query().Get("id"))
		}
		resp, err := s.GetQuote(r.Context(), &fxv1.GetQuoteRequest{QuoteId: quoteID})
		if err != nil {
			writeFXJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeFXJSON(w, http.StatusOK, resp.Quote)
		return
	}

	if r.Method == http.MethodPost {
		var body struct {
			BaseCurrency   string `json:"base_currency"`
			TargetCurrency string `json:"target_currency"`
			SourceAmount   int64  `json:"source_amount"`
			Amount         int64  `json:"amount"`
			ClientID       string `json:"client_id"`
			TTLSeconds     int32  `json:"ttl_seconds"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeFXJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
			return
		}
		amt := body.SourceAmount
		if amt <= 0 {
			amt = body.Amount
		}
		clientID := strings.TrimSpace(body.ClientID)
		if clientID == "" {
			clientID = strings.TrimSpace(r.Header.Get("X-Client-ID"))
		}
		resp, err := s.CreateQuote(r.Context(), &fxv1.CreateQuoteRequest{
			BaseCurrency:   body.BaseCurrency,
			TargetCurrency: body.TargetCurrency,
			SourceAmount:   amt,
			ClientId:       clientID,
			TtlSeconds:     body.TTLSeconds,
		})
		if err != nil {
			writeFXJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeFXJSON(w, http.StatusCreated, resp.Quote)
		return
	}

	writeFXJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
}

func (s *fxServer) handleHTTPConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeFXJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	var body struct {
		IdempotencyKey string `json:"idempotency_key"`
		QuoteID        string `json:"quote_id"`
		BaseCurrency   string `json:"base_currency"`
		TargetCurrency string `json:"target_currency"`
		SourceAmount   int64  `json:"source_amount"`
		Amount         int64  `json:"amount"`
		ClientID       string `json:"client_id"`
		ReferenceID    string `json:"reference_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeFXJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	amt := body.SourceAmount
	if amt <= 0 {
		amt = body.Amount
	}
	clientID := strings.TrimSpace(body.ClientID)
	if clientID == "" {
		clientID = strings.TrimSpace(r.Header.Get("X-Client-ID"))
	}
	if clientID == "" {
		clientID = "3rd-party-http-client"
	}

	resp, err := s.ConvertCurrency(r.Context(), &fxv1.ConvertCurrencyRequest{
		IdempotencyKey: body.IdempotencyKey,
		QuoteId:        body.QuoteID,
		BaseCurrency:   body.BaseCurrency,
		TargetCurrency: body.TargetCurrency,
		SourceAmount:   amt,
		ClientId:       clientID,
		ReferenceId:    body.ReferenceID,
	})
	if err != nil {
		writeFXJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeFXJSON(w, http.StatusOK, resp)
}

func (s *fxServer) handleHTTPRefreshRates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeFXJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	var body struct {
		BaseCurrency        string  `json:"base_currency"`
		TargetCurrency      string  `json:"target_currency"`
		MidRate             float64 `json:"mid_rate"`
		SpreadBps           int32   `json:"spread_bps"`
		TriggerUpstreamSync bool    `json:"trigger_upstream_sync"`
		SourceProvider      string  `json:"source_provider"`
	}
	// Empty body defaults to triggering an immediate 3rd-party upstream sync
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	}
	if body.BaseCurrency == "" && body.TargetCurrency == "" && body.MidRate == 0 {
		body.TriggerUpstreamSync = true
	}

	resp, err := s.UpdateExchangeRate(r.Context(), &fxv1.UpdateExchangeRateRequest{
		BaseCurrency:        body.BaseCurrency,
		TargetCurrency:      body.TargetCurrency,
		MidRate:             body.MidRate,
		SpreadBps:           body.SpreadBps,
		TriggerUpstreamSync: body.TriggerUpstreamSync,
		SourceProvider:      body.SourceProvider,
	})
	if err != nil {
		writeFXJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeFXJSON(w, http.StatusOK, resp)
}
