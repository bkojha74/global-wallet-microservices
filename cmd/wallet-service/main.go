package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"wallet-system/pkg/db"
	"wallet-system/pkg/observability"
	ledgerv1 "wallet-system/proto/ledger"
	walletv1 "wallet-system/proto/wallet"
)

type server struct {
	walletv1.UnimplementedWalletServiceServer
	mongoClient  *mongo.Client
	ledgerClient ledgerv1.LedgerServiceClient
	region       string
	isActive     bool
	logger       observability.Logger
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

type WalletModel struct {
	ID        string `bson:"_id"`
	Currency  string `bson:"currency"`
	Balance   int64  `bson:"balance"`
	UpdatedAt string `bson:"updated_at"`
}

type IdempotencyRecord struct {
	ID            string    `bson:"_id"` // Idempotency key
	TransactionID string    `bson:"transaction_id"`
	CreatedAt     time.Time `bson:"created_at"`
}

func (s *server) HealthCheck(ctx context.Context, req *walletv1.HealthRequest) (*walletv1.HealthResponse, error) {
	statusStr := "STANDBY"
	if s.isActive {
		statusStr = "ACTIVE"
	}
	return &walletv1.HealthResponse{
		Status:   statusStr,
		Region:   s.region,
		IsActive: s.isActive,
	}, nil
}

func (s *server) CreateWallet(ctx context.Context, req *walletv1.CreateWalletRequest) (*walletv1.CreateWalletResponse, error) {
	correlation := observability.FromIncomingContext(ctx)
	ctx = observability.WithCorrelation(ctx, correlation)
	s.emit(ctx, "wallet.create.request_received", "INFO", "Create wallet request received", map[string]any{"wallet_id": req.WalletId})
	log.Printf("[WALLET] CreateWallet proto received: wallet_id=%s currency=%s initial_balance=%d region=%s", req.WalletId, req.Currency, req.InitialBalance, s.region)
	col := s.mongoClient.Database("banking_db").Collection("wallets")

	model := WalletModel{
		ID:        req.WalletId,
		Currency:  req.Currency,
		Balance:   req.InitialBalance,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	opts := options.Update().SetUpsert(true)
	_, err := col.UpdateOne(ctx, bson.M{"_id": req.WalletId}, bson.M{"$set": model}, opts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create wallet: %v", err)
	}

	log.Printf("[WALLET] Wallet initialized: ID=%s, Currency=%s, Balance=%d (Region: %s)",
		req.WalletId, req.Currency, req.InitialBalance, s.region)

	return &walletv1.CreateWalletResponse{
		Success:         true,
		Message:         fmt.Sprintf("Wallet %s created successfully", req.WalletId),
		HandledByRegion: s.region,
	}, nil
}

func (s *server) GetBalance(ctx context.Context, req *walletv1.GetBalanceRequest) (*walletv1.GetBalanceResponse, error) {
	correlation := observability.FromIncomingContext(ctx)
	ctx = observability.WithCorrelation(ctx, correlation)
	s.emit(ctx, "wallet.balance.request_received", "INFO", "Get balance request received", map[string]any{"wallet_id": req.WalletId})
	log.Printf("[WALLET] GetBalance proto received: wallet_id=%s region=%s", req.WalletId, s.region)
	col := s.mongoClient.Database("banking_db").Collection("wallets")

	var wallet WalletModel
	err := col.FindOne(ctx, bson.M{"_id": req.WalletId}).Decode(&wallet)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, status.Errorf(codes.NotFound, "wallet %s not found", req.WalletId)
		}
		return nil, status.Errorf(codes.Internal, "db error: %v", err)
	}

	return &walletv1.GetBalanceResponse{
		WalletId: req.WalletId,
		Balances: []*walletv1.Money{
			{Currency: wallet.Currency, Units: wallet.Balance},
		},
		HandledByRegion: s.region,
	}, nil
}

// TransferFunds executes an ACID multi-document MongoDB transaction
func (s *server) TransferFunds(ctx context.Context, req *walletv1.TransferFundsRequest) (*walletv1.TransferFundsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "transfer request is required")
	}
	correlation := observability.FromIncomingContext(ctx)
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = req.IdempotencyKey
	}
	ctx = observability.WithCorrelation(ctx, correlation)
	s.emit(ctx, "wallet.transfer.request_received", "INFO", "Transfer request received", map[string]any{"source_wallet_id": req.SourceWalletId, "destination_wallet_id": req.DestinationWalletId})
	traceID := req.IdempotencyKey
	log.Printf("[WALLET-TX] trace_id=%s step=wallet_proto_request_received source=%s destination=%s amount=%d currency=%s region=%s", traceID, req.SourceWalletId, req.DestinationWalletId, req.Amount.GetUnits(), req.Amount.GetCurrency(), s.region)
	if req.IdempotencyKey == "" || req.SourceWalletId == "" || req.DestinationWalletId == "" || req.Amount == nil || req.Amount.Currency == "" || req.Amount.Units <= 0 {
		log.Printf("[WALLET-TX] trace_id=%s step=validation_failed reason=invalid_transfer_request", traceID)
		return nil, status.Error(codes.InvalidArgument, "idempotency_key, source_wallet_id, destination_wallet_id, amount.currency, and positive amount.units are required")
	}
	if req.SourceWalletId == req.DestinationWalletId {
		log.Printf("[WALLET-TX] trace_id=%s step=validation_failed reason=identical_wallets", traceID)
		return &walletv1.TransferFundsResponse{
			Status:          walletv1.TransferFundsResponse_INTERNAL_ERROR,
			ErrorMessage:    "source and destination wallets cannot be identical",
			HandledByRegion: s.region,
		}, nil
	}

	session, err := s.mongoClient.StartSession()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to start mongo session: %v", err)
	}
	defer session.EndSession(ctx)

	txnOpts := options.Transaction().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Snapshot())

	var finalTxnID string
	var txnStatus walletv1.TransferFundsResponse_Status = walletv1.TransferFundsResponse_SUCCESS
	var txnErrMsg string

	_, err = session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		log.Printf("[WALLET-TX] trace_id=%s step=mongo_transaction_started", traceID)
		walletsCol := s.mongoClient.Database("banking_db").Collection("wallets")
		idempCol := s.mongoClient.Database("banking_db").Collection("idempotency_records")

		// 1. Idempotency verification
		var existingIdemp IdempotencyRecord
		log.Printf("[WALLET-TX] trace_id=%s step=idempotency_check", traceID)
		err := idempCol.FindOne(sessCtx, bson.M{"_id": req.IdempotencyKey}).Decode(&existingIdemp)
		if err == nil {
			log.Printf("[WALLET-TX] Idempotent request detected. Returning existing TX: %s", existingIdemp.TransactionID)
			finalTxnID = existingIdemp.TransactionID
			txnStatus = walletv1.TransferFundsResponse_REJECTED_DUPLICATE
			txnErrMsg = "Transaction already processed"
			s.emit(ctx, "wallet.transfer.duplicate", "INFO", "Duplicate transfer detected", map[string]any{"transaction_id": finalTxnID})
			return nil, nil
		}

		// 2. Atomic debit from source wallet (guarantees sufficient balance)
		filterSource := bson.M{
			"_id":      req.SourceWalletId,
			"currency": req.Amount.Currency,
			"balance":  bson.M{"$gte": req.Amount.Units},
		}
		updateSource := bson.M{
			"$inc": bson.M{"balance": -req.Amount.Units},
			"$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)},
		}
		resSource, err := walletsCol.UpdateOne(sessCtx, filterSource, updateSource)
		if err != nil {
			return nil, err
		}
		if resSource.ModifiedCount == 0 {
			txnStatus = walletv1.TransferFundsResponse_FAILED_INSUFFICIENT_FUNDS
			txnErrMsg = "Insufficient funds or source wallet not found"
			return nil, fmt.Errorf("insufficient funds")
		}
		log.Printf("[WALLET-TX] trace_id=%s step=source_wallet_debited modified_count=%d", traceID, resSource.ModifiedCount)

		// 3. Atomic credit to destination wallet
		filterDest := bson.M{
			"_id":      req.DestinationWalletId,
			"currency": req.Amount.Currency,
		}
		updateDest := bson.M{
			"$inc": bson.M{"balance": req.Amount.Units},
			"$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)},
		}
		resDest, err := walletsCol.UpdateOne(sessCtx, filterDest, updateDest)
		if err != nil {
			return nil, err
		}
		if resDest.ModifiedCount == 0 {
			txnStatus = walletv1.TransferFundsResponse_INTERNAL_ERROR
			txnErrMsg = "Destination wallet not found or currency mismatch"
			return nil, fmt.Errorf("destination wallet not found")
		}
		log.Printf("[WALLET-TX] trace_id=%s step=destination_wallet_credited modified_count=%d", traceID, resDest.ModifiedCount)

		// 4. Inter-service gRPC call to Ledger Service for immutable audit record
		log.Printf("[WALLET-TX] trace_id=%s step=ledger_grpc_request", traceID)
		ledgerCtx := observability.WithOutgoingMetadata(sessCtx, correlation)
		ledgerResp, err := s.ledgerClient.RecordTransaction(ledgerCtx, &ledgerv1.RecordTransactionRequest{
			IdempotencyKey:      req.IdempotencyKey,
			SourceWalletId:      req.SourceWalletId,
			DestinationWalletId: req.DestinationWalletId,
			Amount:              req.Amount.Units,
			Currency:            req.Amount.Currency,
			Region:              s.region,
		})
		if err != nil || ledgerResp == nil || !ledgerResp.Success {
			log.Printf("[WALLET-TX] trace_id=%s step=ledger_grpc_response success=%t error=%v", traceID, ledgerResp != nil && ledgerResp.Success, err)
			return nil, fmt.Errorf("failed to record ledger transaction: %v", err)
		}
		finalTxnID = ledgerResp.TransactionId
		correlation.TransactionID = finalTxnID
		ctx = observability.WithCorrelation(ctx, correlation)
		log.Printf("[WALLET-TX] trace_id=%s step=ledger_grpc_response transaction_id=%s", traceID, finalTxnID)

		// 5. Store Idempotency Key mapping
		_, err = idempCol.InsertOne(sessCtx, IdempotencyRecord{
			ID:            req.IdempotencyKey,
			TransactionID: finalTxnID,
			CreatedAt:     time.Now().UTC(),
		})
		if err != nil {
			return nil, err
		}
		log.Printf("[WALLET-TX] trace_id=%s step=idempotency_record_stored transaction_id=%s", traceID, finalTxnID)

		return nil, nil
	}, txnOpts)

	if err != nil {
		log.Printf("[WALLET-TX] trace_id=%s step=mongo_transaction_failed error=%v", traceID, err)
		if txnStatus == walletv1.TransferFundsResponse_SUCCESS {
			txnStatus = walletv1.TransferFundsResponse_INTERNAL_ERROR
			txnErrMsg = err.Error()
		}
		return &walletv1.TransferFundsResponse{
			TransactionId:   finalTxnID,
			Status:          txnStatus,
			ErrorMessage:    txnErrMsg,
			HandledByRegion: s.region,
		}, nil
	}

	log.Printf("[WALLET-TX] trace_id=%s step=wallet_proto_response_created status=%s transaction_id=%s", traceID, txnStatus.String(), finalTxnID)
	s.emit(ctx, "wallet.transfer.completed", "INFO", "Transfer response created", map[string]any{"status": txnStatus.String(), "transaction_id": finalTxnID})
	log.Printf("[WALLET-TX] SUCCESS: TX=%s | %s -> %s (%d %s) [Region: %s]",
		finalTxnID, req.SourceWalletId, req.DestinationWalletId, req.Amount.Units, req.Amount.Currency, s.region)

	return &walletv1.TransferFundsResponse{
		TransactionId:   finalTxnID,
		Status:          txnStatus,
		HandledByRegion: s.region,
	}, nil
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "50051"
	}
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://mongodb:27017/?replicaSet=rs0&directConnection=true"
	}
	ledgerAddr := os.Getenv("LEDGER_SERVICE_ADDR")
	if ledgerAddr == "" {
		ledgerAddr = "ledger-service:50052"
	}
	region := os.Getenv("REGION_NAME")
	if region == "" {
		region = "us-east-1"
	}
	isActive, _ := strconv.ParseBool(os.Getenv("IS_ACTIVE"))
	environment := os.Getenv("ENVIRONMENT")
	if environment == "" {
		environment = "local"
	}

	log.Printf("[WALLET-SERVICE] Starting in Region: %s (Active: %t) on port %s...", region, isActive, port)

	ctx := context.Background()
	client, err := db.ConnectWithRetry(ctx, mongoURI, 15)
	if err != nil {
		log.Fatalf("Could not connect to MongoDB: %v", err)
	}
	defer client.Disconnect(ctx)

	// Dial Ledger Service via gRPC
	log.Printf("[WALLET-SERVICE] Connecting to Ledger Service at %s...", ledgerAddr)
	conn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect to ledger service: %v", err)
	}
	defer conn.Close()
	ledgerClient := ledgerv1.NewLedgerServiceClient(conn)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	srv := &server{
		mongoClient:  client,
		ledgerClient: ledgerClient,
		region:       region,
		isActive:     isActive,
		logger:       observability.NewStructuredLogger("wallet-service", environment, region, os.Stdout),
	}
	walletv1.RegisterWalletServiceServer(grpcServer, srv)

	log.Printf("[WALLET-SERVICE] Listening for gRPC requests on :%s", port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
