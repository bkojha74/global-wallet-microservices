package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	ledgerv1 "wallet-system/proto/ledger"
)

type server struct {
	ledgerv1.UnimplementedLedgerServiceServer
	mongoClient *mongo.Client
	region      string
	logger      observability.Logger
}

func (s *server) emit(ctx context.Context, eventType, level, message string, attributes map[string]any) {
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
		Attributes:     attributes,
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
	correlation := observability.FromIncomingContext(ctx)
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = req.IdempotencyKey
	}
	ctx = observability.WithCorrelation(ctx, correlation)
	s.emit(ctx, "ledger.record.request_received", "INFO", "Record transaction request received", map[string]any{"source_wallet_id": req.SourceWalletId, "destination_wallet_id": req.DestinationWalletId})
	log.Printf("[LEDGER] proto request received: trace_id=%s source=%s destination=%s amount=%d currency=%s region=%s", req.IdempotencyKey, req.SourceWalletId, req.DestinationWalletId, req.Amount, req.Currency, req.Region)
	if req.IdempotencyKey == "" || req.Amount <= 0 {
		log.Printf("[LEDGER] trace_id=%s step=validation_failed", req.IdempotencyKey)
		return nil, status.Errorf(codes.InvalidArgument, "invalid ledger transaction payload")
	}

	col := s.mongoClient.Database("banking_db").Collection("ledger_entries")

	// Idempotency check: if entry exists, return it
	var existing LedgerDocument
	log.Printf("[LEDGER] trace_id=%s step=idempotency_check", req.IdempotencyKey)
	err := col.FindOne(ctx, bson.M{"idempotency_key": req.IdempotencyKey}).Decode(&existing)
	if err == nil {
		log.Printf("[LEDGER] Duplicate transaction detected for key: %s, returning existing ID: %s", req.IdempotencyKey, existing.ID.Hex())
		return &ledgerv1.RecordTransactionResponse{
			TransactionId: existing.ID.Hex(),
			Success:       true,
		}, nil
	}

	doc := LedgerDocument{
		ID:                  primitive.NewObjectID(),
		IdempotencyKey:      req.IdempotencyKey,
		SourceWalletID:      req.SourceWalletId,
		DestinationWalletID: req.DestinationWalletId,
		Amount:              req.Amount,
		Currency:            req.Currency,
		Region:              s.region,
		Timestamp:           time.Now().UTC(),
	}

	_, err = col.InsertOne(ctx, doc)
	if err != nil {
		log.Printf("[LEDGER] Error persisting audit record: %v", err)
		return &ledgerv1.RecordTransactionResponse{
			Success:      false,
			ErrorMessage: err.Error(),
		}, nil
	}
	log.Printf("[LEDGER] trace_id=%s step=ledger_document_persisted transaction_id=%s", req.IdempotencyKey, doc.ID.Hex())
	correlation.TransactionID = doc.ID.Hex()
	ctx = observability.WithCorrelation(ctx, correlation)
	s.emit(ctx, "ledger.transaction.persisted", "INFO", "Ledger transaction persisted", map[string]any{"transaction_id": doc.ID.Hex()})

	log.Printf("[LEDGER] Audit entry recorded: TX=%s | %s -> %s (%d %s) [Region: %s]",
		doc.ID.Hex(), req.SourceWalletId, req.DestinationWalletId, req.Amount, req.Currency, s.region)

	return &ledgerv1.RecordTransactionResponse{
		TransactionId: doc.ID.Hex(),
		Success:       true,
	}, nil
}

func (s *server) GetLedgerEntries(ctx context.Context, req *ledgerv1.GetLedgerRequest) (*ledgerv1.GetLedgerResponse, error) {
	col := s.mongoClient.Database("banking_db").Collection("ledger_entries")

	filter := bson.M{
		"$or": []bson.M{
			{"source_wallet_id": req.WalletId},
			{"destination_wallet_id": req.WalletId},
		},
	}

	cursor, err := col.Find(ctx, filter)
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

	return &ledgerv1.GetLedgerResponse{Entries: entries}, nil
}

func environmentName() string {
	if environment := os.Getenv("ENVIRONMENT"); environment != "" {
		return environment
	}
	return "local"
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "50052"
	}
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://mongodb:27017/?replicaSet=rs0&directConnection=true"
	}
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

	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	srv := &server{
		mongoClient: client,
		region:      region,
		logger:      observability.NewStructuredLogger("ledger-service", environmentName(), region, os.Stdout),
	}
	ledgerv1.RegisterLedgerServiceServer(grpcServer, srv)

	log.Printf("[LEDGER-SERVICE] gRPC listening on :%s", port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
