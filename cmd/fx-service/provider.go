package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"wallet-system/pkg/db"
)

// RateSnapshot represents a point-in-time set of mid-market exchange rates anchored to BaseCurrency.
type RateSnapshot struct {
	BaseCurrency string             `json:"base_currency"`
	Rates        map[string]float64 `json:"rates"`
	ProviderName string             `json:"provider_name"`
	FetchedAt    time.Time          `json:"fetched_at"`
}

// RateProvider defines the contract for upstream 3rd-party Foreign Exchange rate feeds.
type RateProvider interface {
	Name() string
	FetchRates(ctx context.Context, baseCurrency string) (*RateSnapshot, error)
}

// InstitutionalBaselineRates provides deterministic reference mid-market rates relative to 1 USD.
// Used both to fill any currency gaps (e.g. USD-pegged BHD in ECB feeds) and as an offline/CI fallback.
var InstitutionalBaselineRates = map[string]float64{
	"USD": 1.0000,
	"EUR": 0.9200,
	"GBP": 0.7900,
	"CAD": 1.3600,
	"AUD": 1.5200,
	"JPY": 150.0000,
	"CHF": 0.8800,
	"SGD": 1.3400,
	"INR": 83.5000,
	"BHD": 0.3760,
}

// FrankfurterProvider fetches live institutional reference rates published by the
// European Central Bank (ECB) via the public Frankfurter API (no API key required).
type FrankfurterProvider struct {
	Endpoint   string
	HTTPClient *http.Client
}

func NewFrankfurterProvider(endpoint string, timeout time.Duration) *FrankfurterProvider {
	if endpoint == "" {
		endpoint = "https://api.frankfurter.dev/v1/latest"
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &FrankfurterProvider{
		Endpoint:   strings.TrimRight(endpoint, "/"),
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

func (p *FrankfurterProvider) Name() string {
	return "frankfurter-ecb"
}

func (p *FrankfurterProvider) FetchRates(ctx context.Context, baseCurrency string) (*RateSnapshot, error) {
	base := db.NormalizeCurrency(baseCurrency)
	if base == "" {
		base = "USD"
	}
	reqURL := fmt.Sprintf("%s?base=%s", p.Endpoint, base)
	// #nosec G704 -- outbound request to public ECB rate feed with normalized ISO-4217 base currency
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "global-wallet-fx-engine/1.0")

	// #nosec G704 -- outbound request to public ECB rate feed
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("frankfurter request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("frankfurter returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var payload struct {
		Base  string             `json:"base"`
		Date  string             `json:"date"`
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("frankfurter JSON decode failed: %w", err)
	}
	if len(payload.Rates) == 0 {
		return nil, fmt.Errorf("frankfurter returned empty rates map")
	}

	rates := make(map[string]float64, len(payload.Rates)+1)
	rates[base] = 1.0
	for k, v := range payload.Rates {
		code := db.NormalizeCurrency(k)
		if v > 0 {
			rates[code] = v
		}
	}

	return &RateSnapshot{
		BaseCurrency: base,
		Rates:        rates,
		ProviderName: p.Name(),
		FetchedAt:    time.Now().UTC(),
	}, nil
}

// ExchangeRateAPIProvider fetches live global FX rates from Open ExchangeRate-API (open.er-api.com)
// or commercial ExchangeRate-API v6 when EXCHANGERATE_API_KEY is configured.
type ExchangeRateAPIProvider struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

func NewExchangeRateAPIProvider(baseURL, apiKey string, timeout time.Duration) *ExchangeRateAPIProvider {
	if baseURL == "" {
		if apiKey != "" {
			baseURL = fmt.Sprintf("https://v6.exchangerate-api.com/v6/%s/latest", apiKey)
		} else {
			baseURL = "https://open.er-api.com/v6/latest"
		}
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ExchangeRateAPIProvider{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

func (p *ExchangeRateAPIProvider) Name() string {
	return "open-exchangerate-api"
}

func (p *ExchangeRateAPIProvider) FetchRates(ctx context.Context, baseCurrency string) (*RateSnapshot, error) {
	base := db.NormalizeCurrency(baseCurrency)
	if base == "" {
		base = "USD"
	}
	reqURL := fmt.Sprintf("%s/%s", p.BaseURL, base)
	// #nosec G704 -- outbound request to configured ExchangeRate-API provider
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "global-wallet-fx-engine/1.0")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	// #nosec G704 -- outbound request to configured ExchangeRate-API provider
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchangerate-api request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exchangerate-api returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var payload struct {
		Result          string             `json:"result"`
		BaseCode        string             `json:"base_code"`
		Base            string             `json:"base"`
		Rates           map[string]float64 `json:"rates"`
		ConversionRates map[string]float64 `json:"conversion_rates"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("exchangerate-api JSON decode failed: %w", err)
	}

	rawRates := payload.Rates
	if len(rawRates) == 0 {
		rawRates = payload.ConversionRates
	}
	if len(rawRates) == 0 {
		return nil, fmt.Errorf("exchangerate-api returned empty rates map")
	}

	rates := make(map[string]float64, len(rawRates)+1)
	rates[base] = 1.0
	for k, v := range rawRates {
		code := db.NormalizeCurrency(k)
		if v > 0 {
			rates[code] = v
		}
	}

	return &RateSnapshot{
		BaseCurrency: base,
		Rates:        rates,
		ProviderName: p.Name(),
		FetchedAt:    time.Now().UTC(),
	}, nil
}

// BaselineProvider returns deterministic institutional reference rates.
type BaselineProvider struct{}

func (b *BaselineProvider) Name() string {
	return "institutional-baseline"
}

func (b *BaselineProvider) FetchRates(_ context.Context, baseCurrency string) (*RateSnapshot, error) {
	base := db.NormalizeCurrency(baseCurrency)
	if base == "" {
		base = "USD"
	}
	baseToUSD, ok := InstitutionalBaselineRates[base]
	if !ok || baseToUSD <= 0 {
		baseToUSD = 1.0
		base = "USD"
	}

	rates := make(map[string]float64, len(InstitutionalBaselineRates))
	for curr, usdRate := range InstitutionalBaselineRates {
		rates[curr] = usdRate / baseToUSD
	}
	return &RateSnapshot{
		BaseCurrency: base,
		Rates:        rates,
		ProviderName: b.Name(),
		FetchedAt:    time.Now().UTC(),
	}, nil
}

// CompositeRateProvider chains multiple 3rd-party FX providers in priority order,
// merging live market rates with institutional baseline coverage for pegged currencies.
type CompositeRateProvider struct {
	Providers []RateProvider
	Fallback  *BaselineProvider
}

func NewCompositeRateProviderFromEnv() *CompositeRateProvider {
	timeout := 5 * time.Second
	if tStr := os.Getenv("FX_PROVIDER_TIMEOUT_MS"); tStr != "" {
		if d, err := time.ParseDuration(tStr + "ms"); err == nil && d > 0 {
			timeout = d
		}
	}

	var providers []RateProvider

	// 1. Optional custom 3rd-party provider URL
	if customURL := strings.TrimSpace(os.Getenv("FX_PROVIDER_URL")); customURL != "" {
		apiKey := strings.TrimSpace(os.Getenv("FX_PROVIDER_API_KEY"))
		providers = append(providers, NewExchangeRateAPIProvider(customURL, apiKey, timeout))
	}

	// 2. Unless offline mode is forced, register live public 3rd-party FX APIs
	if strings.ToLower(os.Getenv("FX_OFFLINE_MODE")) != "true" {
		erAPIKey := strings.TrimSpace(os.Getenv("EXCHANGERATE_API_KEY"))
		providers = append(providers,
			NewExchangeRateAPIProvider("", erAPIKey, timeout),
			NewFrankfurterProvider("", timeout),
		)
	}

	return &CompositeRateProvider{
		Providers: providers,
		Fallback:  &BaselineProvider{},
	}
}

func (c *CompositeRateProvider) Name() string {
	return "composite-3rd-party-fx"
}

func (c *CompositeRateProvider) FetchRates(ctx context.Context, baseCurrency string) (*RateSnapshot, error) {
	baseSnap, _ := c.Fallback.FetchRates(ctx, baseCurrency)
	merged := make(map[string]float64, len(baseSnap.Rates))
	for k, v := range baseSnap.Rates {
		merged[k] = v
	}

	var activeProvider string
	var fetchedAt time.Time

	for _, p := range c.Providers {
		snap, err := p.FetchRates(ctx, baseCurrency)
		if err != nil {
			log.Printf("[FX-PROVIDER] Provider %s failed: %v (trying next fallback)", p.Name(), err)
			continue
		}
		for curr, rate := range snap.Rates {
			if db.IsValidCurrency(curr) && rate > 0 {
				merged[curr] = rate
			}
		}
		activeProvider = snap.ProviderName
		fetchedAt = snap.FetchedAt
		log.Printf("[FX-PROVIDER] Successfully synced live FX rates from 3rd-party provider: %s (%d currencies)", activeProvider, len(snap.Rates))
		break
	}

	if activeProvider == "" {
		activeProvider = baseSnap.ProviderName
		fetchedAt = baseSnap.FetchedAt
	}

	return &RateSnapshot{
		BaseCurrency: baseSnap.BaseCurrency,
		Rates:        merged,
		ProviderName: activeProvider,
		FetchedAt:    fetchedAt,
	}, nil
}
