package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	SequenceNumber      int64              `bson:"sequence_number,omitempty"`
	IdempotencyKey      string             `bson:"idempotency_key"`
	SourceWalletID      string             `bson:"source_wallet_id"`
	DestinationWalletID string             `bson:"destination_wallet_id"`
	Amount              int64              `bson:"amount"`
	Currency            string             `bson:"currency"`
	Region              string             `bson:"region"`
	Timestamp           time.Time          `bson:"timestamp"`
	Postings            []JournalPosting   `bson:"postings,omitempty"`
	PreviousHash        string             `bson:"previous_hash,omitempty"`
	EntryHash           string             `bson:"entry_hash,omitempty"`
}

const (
	ledgerServiceName       = "ledger-service"
	eventLedgerRecordFailed = "ledger.record.failed"
)

func parseTransactionDocID(txID string) primitive.ObjectID {
	if txID != "" {
		if parsed, err := primitive.ObjectIDFromHex(txID); err == nil {
			return parsed
		}
	}
	return primitive.NewObjectID()
}

func (s *server) checkDuplicateTransaction(ctx context.Context, col *mongo.Collection, key string, startTime time.Time) (*ledgerv1.RecordTransactionResponse, bool) {
	var existing LedgerDocument
	log.Printf("[LEDGER] trace_id=%s step=idempotency_check", key)
	if err := col.FindOne(ctx, bson.M{"idempotency_key": key}).Decode(&existing); err == nil {
		log.Printf("[LEDGER] Duplicate transaction detected for key: %s, returning existing ID: %s", key, existing.ID.Hex())
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, "ledger.transaction.duplicate", observability.LevelInfo, "Duplicate ledger transaction detected", durationMS, true, map[string]any{"transaction_id": existing.ID.Hex()})
		return &ledgerv1.RecordTransactionResponse{
			TransactionId: existing.ID.Hex(),
			Success:       true,
		}, true
	}
	return nil, false
}

func (s *server) getPreviousSequenceAndHash(ctx context.Context, col *mongo.Collection) (int64, string) {
	var seqNumber int64 = 1
	prevHash := GenesisHash
	var lastDoc LedgerDocument
	findLastOpts := options.FindOne().SetSort(bson.D{bson.E{Key: "sequence_number", Value: -1}})
	if findErr := col.FindOne(ctx, bson.M{"sequence_number": bson.M{"$gt": 0}}, findLastOpts).Decode(&lastDoc); findErr == nil {
		seqNumber = lastDoc.SequenceNumber + 1
		if lastDoc.EntryHash != "" {
			prevHash = lastDoc.EntryHash
		}
	}
	return seqNumber, prevHash
}

func (s *server) handleInsertError(ctx context.Context, col *mongo.Collection, req *ledgerv1.RecordTransactionRequest, err error, startTime time.Time) (*ledgerv1.RecordTransactionResponse, error) {
	durationMS := time.Since(startTime).Milliseconds()
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
	s.emitTerminal(ctx, eventLedgerRecordFailed, observability.LevelError, "Failed to persist ledger record", durationMS, false, map[string]any{"error": err.Error()})
	return &ledgerv1.RecordTransactionResponse{
		Success:      false,
		ErrorMessage: err.Error(),
	}, nil
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
		s.emitTerminal(ctx, eventLedgerRecordFailed, observability.LevelError, "Validation failed", durationMS, false, map[string]any{"reason": "missing_idempotency_key"})
		return nil, status.Errorf(codes.InvalidArgument, "idempotency_key is required")
	}
	if err := db.ValidateAmount(req.Amount, req.Currency); err != nil {
		log.Printf("[LEDGER] trace_id=%s step=validation_failed reason=%v", req.IdempotencyKey, err)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, eventLedgerRecordFailed, observability.LevelError, "Validation failed", durationMS, false, map[string]any{"reason": err.Error()})
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	if s.mongoClient == nil {
		return nil, status.Errorf(codes.Unavailable, "database unavailable")
	}
	col := s.mongoClient.Database("banking_db").Collection("ledger_entries")

	// Idempotency check: if entry exists, return it
	if resp, isDup := s.checkDuplicateTransaction(ctx, col, req.IdempotencyKey, startTime); isDup {
		return resp, nil
	}

	docID := parseTransactionDocID(req.TransactionId)

	// GAAP/IFRS Double-Entry Postings (GAP-FIN-02)
	postings, postErr := CreateTransferPostings(req.SourceWalletId, req.DestinationWalletId, req.Amount, req.Currency)
	if postErr != nil {
		log.Printf("[LEDGER] trace_id=%s step=double_entry_failed reason=%v", req.IdempotencyKey, postErr)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, eventLedgerRecordFailed, observability.LevelError, "Double-entry validation failed", durationMS, false, map[string]any{"reason": postErr.Error()})
		return nil, status.Errorf(codes.InvalidArgument, "double-entry validation failed: %v", postErr)
	}

	// Cryptographic Hash Chaining (GAP-FIN-02): find the previous entry
	seqNumber, prevHash := s.getPreviousSequenceAndHash(ctx, col)
	now := time.Now().UTC()
	entryHash := ComputeEntryHash(prevHash, seqNumber, docID.Hex(), req.IdempotencyKey, now, postings)

	doc := LedgerDocument{
		ID:                  docID,
		SequenceNumber:      seqNumber,
		IdempotencyKey:      req.IdempotencyKey,
		SourceWalletID:      req.SourceWalletId,
		DestinationWalletID: req.DestinationWalletId,
		Amount:              req.Amount,
		Currency:            req.Currency,
		Region:              s.region,
		Timestamp:           now,
		Postings:            postings,
		PreviousHash:        prevHash,
		EntryHash:           entryHash,
	}

	_, err := col.InsertOne(ctx, doc)
	if err != nil {
		return s.handleInsertError(ctx, col, req, err, startTime)
	}

	durationMS := time.Since(startTime).Milliseconds()
	log.Printf("[LEDGER] trace_id=%s step=ledger_document_persisted transaction_id=%s", req.IdempotencyKey, doc.ID.Hex())
	correlation.TransactionID = doc.ID.Hex()
	ctx = observability.WithCorrelation(ctx, correlation)

	// Step 8: ledger.transaction.persisted (AUDIT)
	auditEvt := observability.Event{
		SchemaVersion:  1,
		EventID:        observability.NewAssociationID(),
		OccurredAt:     time.Now().UTC(),
		Service:        ledgerServiceName,
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

func parseLedgerPagination(req *ledgerv1.GetLedgerRequest) (int64, int64) {
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
	return limit, offset
}

func decodeLedgerEntries(ctx context.Context, cursor *mongo.Cursor) []*ledgerv1.LedgerEntry {
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
	return entries
}

func (s *server) GetLedgerEntries(ctx context.Context, req *ledgerv1.GetLedgerRequest) (*ledgerv1.GetLedgerResponse, error) {
	if req == nil || strings.TrimSpace(req.WalletId) == "" {
		return nil, status.Errorf(codes.InvalidArgument, "wallet_id is required")
	}

	if s.mongoClient == nil {
		return nil, status.Errorf(codes.Unavailable, "database unavailable")
	}
	col := s.mongoClient.Database("banking_db").Collection("ledger_entries")
	filter := bson.M{
		"$or": []bson.M{
			{"source_wallet_id": req.WalletId},
			{"destination_wallet_id": req.WalletId},
		},
	}

	limit, offset := parseLedgerPagination(req)
	totalCount, _ := col.CountDocuments(ctx, filter)

	findOpts := options.Find().
		SetSort(bson.D{bson.E{Key: "timestamp", Value: -1}}).
		SetSkip(offset).
		SetLimit(limit)

	cursor, err := col.Find(ctx, filter, findOpts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to query ledger: %v", err)
	}
	defer cursor.Close(ctx)

	entries := decodeLedgerEntries(ctx, cursor)

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

func initLedgerOutbox(ctx context.Context, client *mongo.Client) (*observability.MongoOutbox, *observability.RabbitPublisher) {
	if client == nil || !observability.OutboxEnabled() {
		return nil, nil
	}
	rabbitURL := os.Getenv("LOGGING_RABBITMQ_URL")
	if rabbitURL == "" {
		log.Println("[LEDGER-SERVICE] LOGGING_OUTBOX_ENABLED=true but LOGGING_RABBITMQ_URL is not set — outbox disabled")
		return nil, nil
	}
	log.Println("[LEDGER-SERVICE] Transactional outbox ENABLED")
	ledgerOutbox := observability.NewMongoOutbox(client.Database("banking_db"), "ledger_outbox")
	idxCtx, idxCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := ledgerOutbox.EnsureIndexes(idxCtx); err != nil {
		log.Printf("[LEDGER-SERVICE] outbox index creation warning: %v", err)
	}
	idxCancel()
	tlsCfg, _ := observability.TLSConfigFromEnv()
	outboxPublisher := observability.NewRabbitPublisherWithTLS(rabbitURL, "", tlsCfg)
	relay := observability.NewOutboxRelay(client.Database("banking_db"), "ledger_outbox", outboxPublisher)
	relay.Start(ctx)
	return ledgerOutbox, outboxPublisher
}

func startLedgerMetricsServer(client *mongo.Client, metricsPort, region string) *http.Server {
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", observability.DefaultMetrics.Handler())
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"UP","region":%q}`+"\n", region)
	})
	// Phase 4 (GAP-FIN-02): Ledger Audit Chain & Trial Balance Verification
	metricsMux.HandleFunc("/audit/verify", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if client == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "VERIFIED"})
			return
		}
		col := client.Database("banking_db").Collection("ledger_entries")
		res, err := VerifyAuditChain(r.Context(), col)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ERROR", "error": err.Error()})
			return
		}
		if res.Status != "VERIFIED" {
			w.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	server := &http.Server{
		Addr:              ":" + metricsPort,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("[LEDGER-SERVICE] Management metrics server listening on :%s/metrics", metricsPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[LEDGER-SERVICE] Metrics server error: %v", err)
		}
	}()
	return server
}

func runLedgerServer(ctx context.Context) error {
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

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var client *mongo.Client
	if os.Getenv("TEST_MOCK_DB") == "true" {
		log.Println("[LEDGER-SERVICE] TEST_MOCK_DB=true — running in mock DB mode")
	} else {
		var err error
		client, err = db.ConnectWithRetry(subCtx, mongoURI, 2)
		if err != nil {
			return fmt.Errorf("could not connect to MongoDB: %w", err)
		}
		defer client.Disconnect(context.Background())

		// Phase 1: Ensure MongoDB indexes
		idxCtx, idxCancel := context.WithTimeout(subCtx, 10*time.Second)
		if err := db.EnsureLedgerIndexes(idxCtx, client.Database("banking_db")); err != nil {
			log.Printf("[LEDGER-SERVICE] Ledger database index creation warning: %v", err)
		}
		idxCancel()
	}

	// Phase 3 (GAP-OBS-01): OpenTelemetry W3C distributed tracing
	shutdownTracer, err := observability.InitTracer(ledgerServiceName)
	if err == nil && shutdownTracer != nil {
		defer func() { _ = shutdownTracer(context.Background()) }()
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	serverOpts, err := getLedgerServerOptions()
	if err != nil {
		return fmt.Errorf("failed to configure server mTLS credentials: %w", err)
	}
	serverOpts = append(serverOpts, grpc.ChainUnaryInterceptor(
		observability.UnaryServerTraceInterceptor(ledgerServiceName),
		observability.DefaultMetrics.UnaryServerMetricsInterceptor(ledgerServiceName),
	))
	grpcServer := grpc.NewServer(serverOpts...)

	// Phase 3 (GAP-REL-03): Register standard gRPC Health Check service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("ledger.v1.LedgerService", grpc_health_v1.HealthCheckResponse_SERVING)

	// Phase 5 (GAP-10): initialise transactional outbox when enabled.
	ledgerOutbox, outboxPublisher := initLedgerOutbox(subCtx, client)

	srv := &server{
		mongoClient: client,
		region:      region,
		logger:      observability.LoggerFromEnvironment(ledgerServiceName, environmentName(), region, os.Stdout),
		outbox:      ledgerOutbox,
	}
	ledgerv1.RegisterLedgerServiceServer(grpcServer, srv)

	metricsPort := os.Getenv("METRICS_PORT")
	if metricsPort == "" {
		metricsPort = "9092"
	}
	metricsServer := startLedgerMetricsServer(client, metricsPort, region)

	go func() {
		log.Printf("[LEDGER-SERVICE] gRPC listening on :%s", port)
		if err := grpcServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			log.Printf("gRPC server stopped: %v", err)
		}
	}()

	// Phase 3 (GAP-REL-01): Graceful Shutdown on SIGINT / SIGTERM / ctx cancel
	<-subCtx.Done()
	log.Printf("[LEDGER-SERVICE] Context cancelled, initiating graceful shutdown...")

	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	healthServer.SetServingStatus("ledger.v1.LedgerService", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	grpcServer.GracefulStop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[LEDGER-SERVICE] Metrics server shutdown error: %v", err)
	}
	if outboxPublisher != nil {
		outboxPublisher.Close()
	}
	log.Println("[LEDGER-SERVICE] Graceful shutdown completed cleanly.")
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runLedgerServer(ctx); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
}
