package main

import (
	"context"
	"encoding/base64"
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

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	"wallet-system/pkg/tlsutil"
	ledgerv1 "wallet-system/proto/ledger"
)

type server struct {
	ledgerv1.UnimplementedLedgerServiceServer
	mongoClient *mongo.Client
	region      string
	logger      observability.Logger
	// Phase 5 (GAP-10): outbox for ledger AUDIT events.
	outbox *observability.MongoOutbox // nil when outbox is disabled
}

func (s *server) emit(ctx context.Context, eventType, level, message string, attributes map[string]any) {
	s.emitFull(ctx, eventType, level, message, 0, nil, attributes)
}

func (s *server) emitTerminal(ctx context.Context, eventType, level, message string, durationMS int64, success bool, attributes map[string]any) {
	s.emitFull(ctx, eventType, level, message, durationMS, &success, attributes)
}

func (s *server) emitFull(ctx context.Context, eventType, level, message string, durationMS int64, success *bool, attributes map[string]any) {
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
		DurationMS:     durationMS,
		Success:        success,
		Attributes:     observability.RedactAttributes(attributes),
	})
}

type LedgerDocument struct {
	ID                  primitive.ObjectID `bson:"_id,omitempty"`
	IdempotencyKey      string             `bson:"idempotency_key"`
	SourceWalletID      string             `bson:"source_wallet_id"`
	DestinationWalletID string             `bson:"destination_wallet_id"`
	Amount              int64              `bson:"amount"`
	Currency            string             `bson:"currency"`
	Region              string             `bson:"region"`
	Timestamp           time.Time          `bson:"timestamp"`
}

func (s *server) RecordTransaction(ctx context.Context, req *ledgerv1.RecordTransactionRequest) (*ledgerv1.RecordTransactionResponse, error) {
	startTime := time.Now()
	correlation := observability.FromIncomingContext(ctx)
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = req.IdempotencyKey
	}
	ctx = observability.WithCorrelation(ctx, correlation)
	s.emit(ctx, "ledger.record.request_received", observability.LevelInfo, "Record transaction request received", map[string]any{"source_wallet_id": req.SourceWalletId, "destination_wallet_id": req.DestinationWalletId})
	log.Printf("[LEDGER] proto request received: trace_id=%s source=%s destination=%s amount=%d currency=%s region=%s", req.IdempotencyKey, req.SourceWalletId, req.DestinationWalletId, req.Amount, req.Currency, req.Region)
	if req.IdempotencyKey == "" {
		log.Printf("[LEDGER] trace_id=%s step=validation_failed reason=missing_idempotency_key", req.IdempotencyKey)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, "ledger.record.failed", observability.LevelError, "Validation failed", durationMS, false, map[string]any{"reason": "missing_idempotency_key"})
		return nil, status.Errorf(codes.InvalidArgument, "idempotency_key is required")
	}
	if err := db.ValidateAmount(req.Amount, req.Currency); err != nil {
		log.Printf("[LEDGER] trace_id=%s step=validation_failed reason=%v", req.IdempotencyKey, err)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, "ledger.record.failed", observability.LevelError, "Validation failed", durationMS, false, map[string]any{"reason": err.Error()})
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	col := s.mongoClient.Database("banking_db").Collection("ledger_entries")

	// Idempotency check: if entry exists, return it
	var existing LedgerDocument
	log.Printf("[LEDGER] trace_id=%s step=idempotency_check", req.IdempotencyKey)
	err := col.FindOne(ctx, bson.M{"idempotency_key": req.IdempotencyKey}).Decode(&existing)
	if err == nil {
		log.Printf("[LEDGER] Duplicate transaction detected for key: %s, returning existing ID: %s", req.IdempotencyKey, existing.ID.Hex())
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, "ledger.transaction.duplicate", observability.LevelInfo, "Duplicate ledger transaction detected", durationMS, true, map[string]any{"transaction_id": existing.ID.Hex()})
		return &ledgerv1.RecordTransactionResponse{
			TransactionId: existing.ID.Hex(),
			Success:       true,
		}, nil
	}

	var docID primitive.ObjectID
	if req.TransactionId != "" {
		if parsed, err := primitive.ObjectIDFromHex(req.TransactionId); err == nil {
			docID = parsed
		} else {
			docID = primitive.NewObjectID()
		}
	} else {
		docID = primitive.NewObjectID()
	}

	doc := LedgerDocument{
		ID:                  docID,
		IdempotencyKey:      req.IdempotencyKey,
		SourceWalletID:      req.SourceWalletId,
		DestinationWalletID: req.DestinationWalletId,
		Amount:              req.Amount,
		Currency:            req.Currency,
		Region:              s.region,
		Timestamp:           time.Now().UTC(),
	}

	_, err = col.InsertOne(ctx, doc)
	durationMS := time.Since(startTime).Milliseconds()
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			log.Printf("[LEDGER] Race detected: Duplicate transaction key on insert: %s", req.IdempotencyKey)
			var dupDoc LedgerDocument
			if findErr := col.FindOne(ctx, bson.M{"idempotency_key": req.IdempotencyKey}).Decode(&dupDoc); findErr == nil {
				s.emitTerminal(ctx, "ledger.transaction.duplicate", observability.LevelInfo, "Duplicate ledger transaction detected", durationMS, true, map[string]any{"transaction_id": dupDoc.ID.Hex()})
				return &ledgerv1.RecordTransactionResponse{
					TransactionId: dupDoc.ID.Hex(),
					Success:       true,
				}, nil
			}
		}
		log.Printf("[LEDGER] Error persisting audit record: %v", err)
		s.emitTerminal(ctx, "ledger.record.failed", observability.LevelError, "Failed to persist ledger record", durationMS, false, map[string]any{"error": err.Error()})
		return &ledgerv1.RecordTransactionResponse{
			Success:      false,
			ErrorMessage: err.Error(),
		}, nil
	}
	log.Printf("[LEDGER] trace_id=%s step=ledger_document_persisted transaction_id=%s", req.IdempotencyKey, doc.ID.Hex())
	correlation.TransactionID = doc.ID.Hex()
	ctx = observability.WithCorrelation(ctx, correlation)

	// Step 8: ledger.transaction.persisted (AUDIT)
	auditEvt := observability.Event{
		SchemaVersion:  1,
		EventID:        observability.NewAssociationID(),
		OccurredAt:     time.Now().UTC(),
		Service:        "ledger-service",
		Environment:    environmentName(),
		Region:         s.region,
		Level:          observability.LevelAudit,
		EventType:      "ledger.transaction.persisted",
		Message:        "Ledger transaction persisted",
		AssociationID:  correlation.AssociationID,
		TransactionID:  doc.ID.Hex(),
		IdempotencyKey: correlation.IdempotencyKey,
		DurationMS:     durationMS,
		Attributes: map[string]any{
			"transaction_id": doc.ID.Hex(),
			"source":         req.SourceWalletId,
			"dest":           req.DestinationWalletId,
			"amount":         req.Amount,
			"currency":       req.Currency,
		},
	}
	s.emitTerminal(ctx, auditEvt.EventType, auditEvt.Level, auditEvt.Message, durationMS, true, auditEvt.Attributes)
	if s.outbox != nil {
		if err := s.outbox.Append(ctx, auditEvt); err != nil {
			log.Printf("[LEDGER] outbox append (transaction.persisted) failed: %v", err)
			return nil, status.Errorf(codes.Internal, "failed to append to outbox: %v", err)
		}
	}

	log.Printf("[LEDGER] Audit entry recorded: TX=%s | %s -> %s (%d %s) [Region: %s]",
		doc.ID.Hex(), req.SourceWalletId, req.DestinationWalletId, req.Amount, req.Currency, s.region)

	return &ledgerv1.RecordTransactionResponse{
		TransactionId: doc.ID.Hex(),
		Success:       true,
	}, nil
}

func (s *server) GetLedgerEntries(ctx context.Context, req *ledgerv1.GetLedgerRequest) (*ledgerv1.GetLedgerResponse, error) {
	if req == nil || strings.TrimSpace(req.WalletId) == "" {
		return nil, status.Errorf(codes.InvalidArgument, "wallet_id is required")
	}

	col := s.mongoClient.Database("banking_db").Collection("ledger_entries")

	filter := bson.M{
		"$or": []bson.M{
			{"source_wallet_id": req.WalletId},
			{"destination_wallet_id": req.WalletId},
		},
	}

	limit := int64(req.Limit)
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	var offset int64 = 0
	if req.PageToken != "" {
		if decoded, err := base64.StdEncoding.DecodeString(req.PageToken); err == nil {
			if parsedOffset, err := strconv.ParseInt(string(decoded), 10, 64); err == nil && parsedOffset >= 0 {
				offset = parsedOffset
			}
		}
	}

	totalCount, _ := col.CountDocuments(ctx, filter)

	findOpts := options.Find().
		SetSort(bson.D{{Key: "timestamp", Value: -1}}).
		SetSkip(offset).
		SetLimit(limit)

	cursor, err := col.Find(ctx, filter, findOpts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to query ledger: %v", err)
	}
	defer cursor.Close(ctx)

	var entries []*ledgerv1.LedgerEntry
	for cursor.Next(ctx) {
		var doc LedgerDocument
		if err := cursor.Decode(&doc); err != nil {
			continue
		}
		entries = append(entries, &ledgerv1.LedgerEntry{
			TransactionId:       doc.ID.Hex(),
			IdempotencyKey:      doc.IdempotencyKey,
			SourceWalletId:      doc.SourceWalletID,
			DestinationWalletId: doc.DestinationWalletID,
			Amount:              doc.Amount,
			Currency:            doc.Currency,
			Timestamp:           doc.Timestamp.Format(time.RFC3339),
			Region:              doc.Region,
		})
	}

	var nextPageToken string
	if offset+int64(len(entries)) < totalCount {
		nextPageToken = base64.StdEncoding.EncodeToString([]byte(strconv.FormatInt(offset+int64(len(entries)), 10)))
	}

	return &ledgerv1.GetLedgerResponse{
		Entries:       entries,
		NextPageToken: nextPageToken,
		TotalCount:    totalCount,
	}, nil
}

func environmentName() string {
	if environment := os.Getenv("ENVIRONMENT"); environment != "" {
		return environment
	}
	return "local"
}

func getLedgerServerOptions() ([]grpc.ServerOption, error) {
	if os.Getenv("GRPC_TLS_ENABLED") == "true" {
		certFile := os.Getenv("GRPC_SERVER_CERT")
		keyFile := os.Getenv("GRPC_SERVER_KEY")
		caFile := os.Getenv("GRPC_CA_CERT")
		creds, err := tlsutil.NewServerTransportCredentials(certFile, keyFile, caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load ledger server mTLS credentials: %w", err)
		}
		log.Println("[SECURITY] Ledger gRPC server configured with mTLS transport credentials")
		return []grpc.ServerOption{grpc.Creds(creds)}, nil
	}
	return nil, nil
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "50052"
	}
	mongoURI := db.DefaultMongoURI()
	region := os.Getenv("REGION_NAME")
	if region == "" {
		region = "us-east-1"
	}

	log.Printf("[LEDGER-SERVICE] Initializing on port %s in region %s...", port, region)

	ctx := context.Background()
	client, err := db.ConnectWithRetry(ctx, mongoURI, 15)
	if err != nil {
		log.Fatalf("Could not connect to MongoDB: %v", err)
	}
	defer client.Disconnect(ctx)

	// Phase 1: Ensure MongoDB indexes
	idxCtx, idxCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := db.EnsureLedgerIndexes(idxCtx, client.Database("banking_db")); err != nil {
		log.Printf("[LEDGER-SERVICE] Ledger database index creation warning: %v", err)
	}
	idxCancel()

	// Phase 3 (GAP-OBS-01): OpenTelemetry W3C distributed tracing
	shutdownTracer, err := observability.InitTracer("ledger-service")
	if err == nil && shutdownTracer != nil {
		defer func() { _ = shutdownTracer(context.Background()) }()
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	serverOpts, err := getLedgerServerOptions()
	if err != nil {
		log.Fatalf("Failed to configure server mTLS credentials: %v", err)
	}
	serverOpts = append(serverOpts, grpc.ChainUnaryInterceptor(
		observability.UnaryServerTraceInterceptor("ledger-service"),
		observability.DefaultMetrics.UnaryServerMetricsInterceptor("ledger-service"),
	))
	grpcServer := grpc.NewServer(serverOpts...)

	// Phase 3 (GAP-REL-03): Register standard gRPC Health Check service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("ledger.v1.LedgerService", grpc_health_v1.HealthCheckResponse_SERVING)

	// Phase 5 (GAP-10): initialise transactional outbox when enabled.
	var ledgerOutbox *observability.MongoOutbox
	var outboxPublisher *observability.RabbitPublisher
	if observability.OutboxEnabled() {
		rabbitURL := os.Getenv("LOGGING_RABBITMQ_URL")
		if rabbitURL == "" {
			log.Println("[LEDGER-SERVICE] LOGGING_OUTBOX_ENABLED=true but LOGGING_RABBITMQ_URL is not set — outbox disabled")
		} else {
			log.Println("[LEDGER-SERVICE] Transactional outbox ENABLED")
			ledgerOutbox = observability.NewMongoOutbox(client.Database("banking_db"), "ledger_outbox")
			idxCtx, idxCancel := context.WithTimeout(ctx, 10*time.Second)
			if err := ledgerOutbox.EnsureIndexes(idxCtx); err != nil {
				log.Printf("[LEDGER-SERVICE] outbox index creation warning: %v", err)
			}
			idxCancel()
			tlsCfg, _ := observability.TLSConfigFromEnv()
			outboxPublisher = observability.NewRabbitPublisherWithTLS(rabbitURL, "", tlsCfg)
			relay := observability.NewOutboxRelay(client.Database("banking_db"), "ledger_outbox", outboxPublisher)
			relay.Start(ctx)
		}
	}

	srv := &server{
		mongoClient: client,
		region:      region,
		logger:      observability.LoggerFromEnvironment("ledger-service", environmentName(), region, os.Stdout),
		outbox:      ledgerOutbox,
	}
	ledgerv1.RegisterLedgerServiceServer(grpcServer, srv)

	// Phase 3 (GAP-OBS-02): Dedicated management HTTP server for Prometheus metrics and health
	metricsPort := os.Getenv("METRICS_PORT")
	if metricsPort == "" {
		metricsPort = "9092"
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", observability.DefaultMetrics.Handler())
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"UP","region":%q}`+"\n", region)
	})
	metricsServer := &http.Server{
		Addr:    ":" + metricsPort,
		Handler: metricsMux,
	}
	go func() {
		log.Printf("[LEDGER-SERVICE] Management metrics server listening on :%s/metrics", metricsPort)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[LEDGER-SERVICE] Metrics server error: %v", err)
		}
	}()

	go func() {
		log.Printf("[LEDGER-SERVICE] gRPC listening on :%s", port)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("Failed to serve: %v", err)
		}
	}()

	// Phase 3 (GAP-REL-01): Graceful Shutdown on SIGINT / SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	sig := <-sigChan
	log.Printf("[LEDGER-SERVICE] Received signal %v, initiating graceful shutdown...", sig)

	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	healthServer.SetServingStatus("ledger.v1.LedgerService", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	grpcServer.GracefulStop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[LEDGER-SERVICE] Metrics server shutdown error: %v", err)
	}
	if outboxPublisher != nil {
		outboxPublisher.Close()
	}
	client.Disconnect(shutdownCtx)
	log.Println("[LEDGER-SERVICE] Graceful shutdown completed cleanly.")
}
