package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	"wallet-system/pkg/tlsutil"
	fxv1 "wallet-system/proto/fx"
)

const fxServiceName = "fx-service"

type fxServer struct {
	fxv1.UnimplementedFXServiceServer
	engine *FXEngine
	logger observability.Logger
}

func (s *fxServer) emit(ctx context.Context, eventType, level, message string, attrs map[string]any) {
	if s.logger == nil {
		return
	}
	correlation := observability.FromContext(ctx)
	s.logger.Emit(ctx, observability.Event{
		Level:          level,
		EventType:      eventType,
		Message:        message,
		AssociationID:  correlation.AssociationID,
		TransactionID:  correlation.TransactionID,
		IdempotencyKey: correlation.IdempotencyKey,
		Attributes:     observability.RedactAttributes(attrs),
	})
}

func (s *fxServer) GetExchangeRate(ctx context.Context, req *fxv1.GetExchangeRateRequest) (*fxv1.GetExchangeRateResponse, error) {
	if req == nil || strings.TrimSpace(req.BaseCurrency) == "" || strings.TrimSpace(req.TargetCurrency) == "" {
		return nil, status.Error(codes.InvalidArgument, "base_currency and target_currency are required")
	}
	rate, err := s.engine.GetRate(ctx, req.BaseCurrency, req.TargetCurrency)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return &fxv1.GetExchangeRateResponse{Rate: rate}, nil
}

func (s *fxServer) ListExchangeRates(ctx context.Context, req *fxv1.ListExchangeRatesRequest) (*fxv1.ListExchangeRatesResponse, error) {
	base := "USD"
	if req != nil && strings.TrimSpace(req.BaseCurrency) != "" {
		base = req.BaseCurrency
	}
	rates, prov, ts, err := s.engine.ListRates(ctx, base)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return &fxv1.ListExchangeRatesResponse{
		BaseCurrency:   db.NormalizeCurrency(base),
		Rates:          rates,
		SourceProvider: prov,
		UpdatedAt:      ts,
	}, nil
}

func (s *fxServer) CreateQuote(ctx context.Context, req *fxv1.CreateQuoteRequest) (*fxv1.CreateQuoteResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "create quote request is required")
	}
	correlation := observability.FromIncomingContext(ctx)
	ctx = observability.WithCorrelation(ctx, correlation)

	quote, err := s.engine.CreateQuote(ctx, req.BaseCurrency, req.TargetCurrency, req.SourceAmount, req.ClientId, req.TtlSeconds)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	s.emit(ctx, "fx.quote.created", observability.LevelInfo, "FX fixed-rate quote created", map[string]any{
		"quote_id":        quote.QuoteId,
		"base_currency":   quote.BaseCurrency,
		"target_currency": quote.TargetCurrency,
		"source_amount":   quote.SourceAmount,
		"target_amount":   quote.TargetAmount,
		"effective_rate":  quote.EffectiveRate,
		"client_id":       quote.ClientId,
	})
	return &fxv1.CreateQuoteResponse{Quote: quote}, nil
}

func (s *fxServer) GetQuote(ctx context.Context, req *fxv1.GetQuoteRequest) (*fxv1.GetQuoteResponse, error) {
	if req == nil || strings.TrimSpace(req.QuoteId) == "" {
		return nil, status.Error(codes.InvalidArgument, "quote_id is required")
	}
	quote, err := s.engine.GetQuote(ctx, req.QuoteId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%v", err)
	}
	return &fxv1.GetQuoteResponse{Quote: quote}, nil
}

func (s *fxServer) ConvertCurrency(ctx context.Context, req *fxv1.ConvertCurrencyRequest) (*fxv1.ConvertCurrencyResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "convert currency request is required")
	}
	correlation := observability.FromIncomingContext(ctx)
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = req.IdempotencyKey
	}
	ctx = observability.WithCorrelation(ctx, correlation)

	resp, err := s.engine.ConvertCurrency(ctx, req)
	if err != nil {
		s.emit(ctx, "fx.conversion.failed", observability.LevelError, "FX currency conversion failed", map[string]any{
			"idempotency_key": req.IdempotencyKey,
			"quote_id":        req.QuoteId,
			"error":           err.Error(),
		})
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	s.emit(ctx, "fx.conversion.completed", observability.LevelAudit, "FX currency conversion executed", map[string]any{
		"conversion_id":   resp.ConversionId,
		"quote_id":        resp.QuoteId,
		"base_currency":   resp.BaseCurrency,
		"target_currency": resp.TargetCurrency,
		"source_amount":   resp.SourceAmount,
		"target_amount":   resp.TargetAmount,
		"effective_rate":  resp.EffectiveRate,
		"status":          resp.Status,
	})
	return resp, nil
}

func (s *fxServer) UpdateExchangeRate(ctx context.Context, req *fxv1.UpdateExchangeRateRequest) (*fxv1.UpdateExchangeRateResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "update request is required")
	}

	if req.TriggerUpstreamSync {
		pairs, prov, err := s.engine.SyncUpstreamRates(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "upstream 3rd-party rate sync failed: %v", err)
		}
		s.emit(ctx, "fx.rates.synced", observability.LevelInfo, "Upstream 3rd-party FX rates synced", map[string]any{
			"pairs_updated":   pairs,
			"source_provider": prov,
		})
		return &fxv1.UpdateExchangeRateResponse{
			Success: true,
			Message: fmt.Sprintf("Synced %d FX pairs from %s", pairs, prov),
			// #nosec G115 -- pairs count is bounded by supported currency pairs count
			PairsUpdated:   int32(pairs),
			SourceProvider: prov,
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		}, nil
	}

	rate, err := s.engine.SetPairRate(ctx, req.BaseCurrency, req.TargetCurrency, req.MidRate, req.SpreadBps, req.SourceProvider)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	s.emit(ctx, "fx.rate.overridden", observability.LevelAudit, "Manual FX rate override applied", map[string]any{
		"base_currency":   rate.BaseCurrency,
		"target_currency": rate.TargetCurrency,
		"mid_rate":        rate.MidRate,
		"effective_rate":  rate.EffectiveRate,
		"spread_bps":      rate.SpreadBps,
	})
	return &fxv1.UpdateExchangeRateResponse{
		Success:        true,
		Message:        fmt.Sprintf("Updated FX pair %s/%s (mid=%.6f, effective=%.6f)", rate.BaseCurrency, rate.TargetCurrency, rate.MidRate, rate.EffectiveRate),
		PairsUpdated:   1,
		SourceProvider: rate.SourceProvider,
		UpdatedAt:      rate.UpdatedAt,
	}, nil
}

func (s *fxServer) HealthCheck(_ context.Context, _ *fxv1.FXHealthRequest) (*fxv1.FXHealthResponse, error) {
	prov, curCount, pairCount, syncedAt := s.engine.HealthSummary()
	return &fxv1.FXHealthResponse{
		Status:              "SERVING",
		SourceProvider:      prov,
		SupportedCurrencies: curCount,
		ActivePairs:         pairCount,
		LastSyncedAt:        syncedAt,
	}, nil
}

type fxConfig struct {
	grpcPort     string
	httpPort     string
	metricsPort  string
	mongoURI     string
	environment  string
	spreadBps    int32
	feeBps       int32
	syncInterval time.Duration
	partnerKey   string
}

func parseFXConfig() fxConfig {
	grpcPort := os.Getenv("FX_GRPC_PORT")
	if grpcPort == "" {
		grpcPort = os.Getenv("PORT")
	}
	if grpcPort == "" {
		grpcPort = "50055"
	}
	httpPort := os.Getenv("FX_HTTP_PORT")
	if httpPort == "" {
		httpPort = "8086"
	}
	metricsPort := os.Getenv("METRICS_PORT")
	if metricsPort == "" {
		metricsPort = "9096"
	}
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "local"
	}
	spreadBps := int32(defaultSpreadBps)
	if sVal, err := strconv.ParseInt(os.Getenv("FX_DEFAULT_SPREAD_BPS"), 10, 32); err == nil && sVal >= 0 {
		// #nosec G115 -- bounded by ParseInt bitSize 32
		spreadBps = int32(sVal)
	}
	feeBps := int32(0)
	if fVal, err := strconv.ParseInt(os.Getenv("FX_FEE_BPS"), 10, 32); err == nil && fVal >= 0 {
		// #nosec G115 -- bounded by ParseInt bitSize 32
		feeBps = int32(fVal)
	}
	syncInterval := 5 * time.Minute
	secStr := os.Getenv("FX_SYNC_INTERVAL_SEC")
	if secStr == "" {
		secStr = os.Getenv("FX_SYNC_INTERVAL_SECONDS")
	}
	if secVal, err := strconv.ParseInt(secStr, 10, 64); err == nil && secVal > 0 {
		syncInterval = time.Duration(secVal) * time.Second
	}
	partnerKey := os.Getenv("FX_B2B_API_KEYS")
	if partnerKey == "" {
		partnerKey = os.Getenv("FX_PARTNER_API_KEY")
	}
	if partnerKey == "" {
		partnerKey = os.Getenv("FX_API_KEY")
	}

	return fxConfig{
		grpcPort:     grpcPort,
		httpPort:     httpPort,
		metricsPort:  metricsPort,
		mongoURI:     db.DefaultMongoURI(),
		environment:  environment,
		spreadBps:    spreadBps,
		feeBps:       feeBps,
		syncInterval: syncInterval,
		partnerKey:   partnerKey,
	}
}

func getFXServerOptions() ([]grpc.ServerOption, error) {
	if os.Getenv("GRPC_TLS_ENABLED") == "true" {
		certFile := os.Getenv("GRPC_SERVER_CERT")
		keyFile := os.Getenv("GRPC_SERVER_KEY")
		caFile := os.Getenv("GRPC_CA_CERT")
		creds, err := tlsutil.NewServerTransportCredentials(certFile, keyFile, caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load server mTLS credentials: %w", err)
		}
		log.Println("[SECURITY] FX gRPC server configured with mTLS transport credentials")
		return []grpc.ServerOption{grpc.Creds(creds)}, nil
	}
	return nil, nil
}

func startFXMetricsServer(metricsPort string, srv *fxServer) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", observability.DefaultMetrics.Handler())
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
	server := &http.Server{
		Addr:              ":" + metricsPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("[FX-SERVICE] Management metrics server listening on :%s/metrics", metricsPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[FX-SERVICE] Metrics server error: %v", err)
		}
	}()
	return server
}

func runFXServer(ctx context.Context) error {
	cfg := parseFXConfig()
	log.Printf("[FX-SERVICE] Starting Global FX Engine (gRPC :%s, B2B HTTP :%s, Metrics :%s, spread=%dbps)...",
		cfg.grpcPort, cfg.httpPort, cfg.metricsPort, cfg.spreadBps)

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var mongoClient *mongo.Client
	var fxDB *mongo.Database
	if os.Getenv("TEST_MOCK_DB") == "true" {
		log.Println("[FX-SERVICE] TEST_MOCK_DB=true — running with in-memory rate & quote book")
	} else {
		mCtx, mCancel := context.WithTimeout(subCtx, 10*time.Second)
		client, err := db.ConnectWithRetry(mCtx, cfg.mongoURI, 2)
		mCancel()
		if err != nil {
			log.Printf("[FX-SERVICE] MongoDB connection warning (%v) — operating in resilient in-memory mode", err)
		} else {
			mongoClient = client
			fxDB = client.Database("fx_db")
			defer func() { _ = mongoClient.Disconnect(context.Background()) }()
			idxCtx, idxCancel := context.WithTimeout(subCtx, 10*time.Second)
			if idxErr := db.EnsureFXIndexes(idxCtx, fxDB); idxErr != nil {
				log.Printf("[FX-SERVICE] FX index warning: %v", idxErr)
			}
			idxCancel()
		}
	}

	shutdownTracer, err := observability.InitTracer(fxServiceName)
	if err == nil && shutdownTracer != nil {
		defer func() { _ = shutdownTracer(context.Background()) }()
	}

	provider := NewCompositeRateProviderFromEnv()
	engine := NewFXEngine(fxDB, provider, cfg.spreadBps, cfg.feeBps)
	engine.StartBackgroundSync(subCtx, cfg.syncInterval)
	defer engine.Stop()

	logger := observability.LoggerFromEnvironment(fxServiceName, cfg.environment, "global-fx", os.Stdout)
	srv := &fxServer{
		engine: engine,
		logger: logger,
	}

	// 1. Setup gRPC Server (:50055)
	lis, err := net.Listen("tcp", ":"+cfg.grpcPort)
	if err != nil {
		return fmt.Errorf("failed to listen on gRPC port %s: %w", cfg.grpcPort, err)
	}
	serverOpts, err := getFXServerOptions()
	if err != nil {
		return err
	}
	serverOpts = append(serverOpts, grpc.ChainUnaryInterceptor(
		observability.UnaryServerTraceInterceptor(fxServiceName),
		observability.DefaultMetrics.UnaryServerMetricsInterceptor(fxServiceName),
	))
	grpcServer := grpc.NewServer(serverOpts...)
	fxv1.RegisterFXServiceServer(grpcServer, srv)

	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("fx.v1.FXService", grpc_health_v1.HealthCheckResponse_SERVING)

	go func() {
		log.Printf("[FX-SERVICE] gRPC server listening on :%s", cfg.grpcPort)
		if err := grpcServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			log.Printf("[FX-SERVICE] gRPC server stopped: %v", err)
		}
	}()

	// 2. Setup B2B 3rd-Party HTTP REST Server (:8086)
	httpHandler := buildFXHTTPHandler(srv, cfg.partnerKey)
	httpServer := &http.Server{
		Addr:              ":" + cfg.httpPort,
		Handler:           httpHandler,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("[FX-SERVICE] B2B & 3rd-Party REST API listening on :%s", cfg.httpPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[FX-SERVICE] HTTP server stopped: %v", err)
		}
	}()

	// 3. Setup Prometheus Metrics Server (:9096)
	metricsServer := startFXMetricsServer(cfg.metricsPort, srv)

	<-subCtx.Done()
	log.Println("[FX-SERVICE] Context cancelled, initiating graceful shutdown...")

	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	healthServer.SetServingStatus("fx.v1.FXService", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	grpcServer.GracefulStop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	_ = metricsServer.Shutdown(shutdownCtx)

	log.Println("[FX-SERVICE] Graceful shutdown completed cleanly.")
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runFXServer(ctx); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
}
