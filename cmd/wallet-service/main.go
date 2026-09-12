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
	s.emit(ctx, "wallet.create.request_received", observability.LevelInfo, "Create wallet request received", map[string]any{"wallet_id": req.WalletId})
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
	s.emit(ctx, "wallet.balance.request_received", observability.LevelInfo, "Get balance request received", map[string]any{"wallet_id": req.WalletId})
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
	startTime := time.Now()
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "transfer request is required")
	}
	correlation := observability.FromIncomingContext(ctx)
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = req.IdempotencyKey
	}
	ctx = observability.WithCorrelation(ctx, correlation)

	// Step 3: wallet.transfer.request_received (INFO)
	s.emit(ctx, "wallet.transfer.request_received", observability.LevelInfo, "Transfer request received", map[string]any{
		"source_wallet": req.SourceWalletId,
		"dest_wallet":   req.DestinationWalletId,
		"amount":        req.Amount.GetUnits(),
		"currency":      req.Amount.GetCurrency(),
	})
	traceID := req.IdempotencyKey
	log.Printf("[WALLET-TX] trace_id=%s step=wallet_proto_request_received source=%s destination=%s amount=%d currency=%s region=%s", traceID, req.SourceWalletId, req.DestinationWalletId, req.Amount.GetUnits(), req.Amount.GetCurrency(), s.region)
	if req.IdempotencyKey == "" || req.SourceWalletId == "" || req.DestinationWalletId == "" || req.Amount == nil || req.Amount.Currency == "" || req.Amount.Units <= 0 {
		log.Printf("[WALLET-TX] trace_id=%s step=validation_failed reason=invalid_transfer_request", traceID)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, "wallet.transfer.failed", observability.LevelError, "Validation failed", durationMS, false, map[string]any{
			"error_code":    "invalid_transfer_request",
			"error_message": "idempotency_key, source_wallet_id, destination_wallet_id, amount.currency, and positive amount.units are required",
			"step":          "validation",
		})
		return nil, status.Error(codes.InvalidArgument, "idempotency_key, source_wallet_id, destination_wallet_id, amount.currency, and positive amount.units are required")
	}
	if req.SourceWalletId == req.DestinationWalletId {
		log.Printf("[WALLET-TX] trace_id=%s step=validation_failed reason=identical_wallets", traceID)
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, "wallet.transfer.failed", observability.LevelError, "Identical wallets", durationMS, false, map[string]any{
			"error_code":    "identical_wallets",
			"error_message": "source and destination wallets cannot be identical",
			"step":          "validation",
		})
		return &walletv1.TransferFundsResponse{
			Status:          walletv1.TransferFundsResponse_INTERNAL_ERROR,
			ErrorMessage:    "source and destination wallets cannot be identical",
			HandledByRegion: s.region,
		}, nil
	}

	session, err := s.mongoClient.StartSession()
	if err != nil {
		durationMS := time.Since(startTime).Milliseconds()
		s.emitTerminal(ctx, "wallet.transfer.failed", observability.LevelError, "Failed to start mongo session", durationMS, false, map[string]any{
			"error_code":    "mongo_session_error",
			"error_message": err.Error(),
			"step":          "start_session",
		})
		return nil, status.Errorf(codes.Internal, "failed to start mongo session: %v", err)
	}
	defer session.EndSession(ctx)

	txnOpts := options.Transaction().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Snapshot())

	var finalTxnID string
	var txnStatus walletv1.TransferFundsResponse_Status = walletv1.TransferFundsResponse_SUCCESS
	var txnErrMsg string
	var sourceDebited bool

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
			// Step 4: idempotency check (duplicate found)
			s.emit(ctx, "wallet.transfer.idempotency_checked", observability.LevelDebug, "Duplicate transfer detected", map[string]any{
				"idempotency_key": req.IdempotencyKey,
				"is_replay":       true,
				"transaction_id":  finalTxnID,
			})
			return nil, nil
		}

		// Step 4: wallet.transfer.idempotency_checked (DEBUG) - new transaction
		s.emit(ctx, "wallet.transfer.idempotency_checked", observability.LevelDebug, "Idempotency check passed", map[string]any{
			"idempotency_key": req.IdempotencyKey,
			"is_replay":       false,
		})

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
		sourceDebited = true
		log.Printf("[WALLET-TX] trace_id=%s step=source_wallet_debited modified_count=%d", traceID, resSource.ModifiedCount)

		// Step 5: wallet.transfer.source_debited (AUDIT)
		s.emit(ctx, "wallet.transfer.source_debited", observability.LevelAudit, "Source wallet debited", map[string]any{
			"source_wallet": req.SourceWalletId,
			"debit_amount":  req.Amount.Units,
			"currency":      req.Amount.Currency,
		})

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

		// Step 6: wallet.transfer.destination_credited (AUDIT)
		s.emit(ctx, "wallet.transfer.destination_credited", observability.LevelAudit, "Destination wallet credited", map[string]any{
			"dest_wallet":   req.DestinationWalletId,
			"credit_amount": req.Amount.Units,
			"currency":      req.Amount.Currency,
		})

		// 4. Inter-service gRPC call to Ledger Service for immutable audit record
		log.Printf("[WALLET-TX] trace_id=%s step=ledger_grpc_request", traceID)
		ledgerCtx := observability.WithOutgoingMetadata(sessCtx, correlation)

		// Step 7: wallet.transfer.ledger_request_sent (DEBUG)
		s.emit(ctx, "wallet.transfer.ledger_request_sent", observability.LevelDebug, "Ledger transaction record request sent", map[string]any{
			"source_wallet": req.SourceWalletId,
			"dest_wallet":   req.DestinationWalletId,
			"amount":        req.Amount.Units,
			"currency":      req.Amount.Currency,
		})

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

		// Step 9: wallet.transfer.ledger_response_received (DEBUG)
		s.emit(ctx, "wallet.transfer.ledger_response_received", observability.LevelDebug, "Ledger response received", map[string]any{
			"transaction_id": finalTxnID,
			"ledger_status":  "SUCCESS",
		})

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

		// Step 10: wallet.transfer.idempotency_record_stored (DEBUG)
		s.emit(ctx, "wallet.transfer.idempotency_record_stored", observability.LevelDebug, "Idempotency record stored", map[string]any{
			"idempotency_key": req.IdempotencyKey,
			"transaction_id":  finalTxnID,
		})

		return nil, nil
	}, txnOpts)

	durationMS := time.Since(startTime).Milliseconds()
	if err != nil {
		log.Printf("[WALLET-TX] trace_id=%s step=mongo_transaction_failed error=%v", traceID, err)
		if txnStatus == walletv1.TransferFundsResponse_SUCCESS {
			txnStatus = walletv1.TransferFundsResponse_INTERNAL_ERROR
			txnErrMsg = err.Error()
		}

		if sourceDebited {
			s.emit(ctx, "wallet.transfer.rollback_completed", observability.LevelWarn, "Transaction aborted, balances rolled back", map[string]any{
				"source_wallet": req.SourceWalletId,
				"dest_wallet":   req.DestinationWalletId,
				"amount":        req.Amount.Units,
			})
		}

		s.emitTerminal(ctx, "wallet.transfer.failed", observability.LevelError, "Transfer transaction failed", durationMS, false, map[string]any{
			"error_code":    txnStatus.String(),
			"error_message": txnErrMsg,
			"step":          "mongo_transaction",
		})

		return &walletv1.TransferFundsResponse{
			TransactionId:   finalTxnID,
			Status:          txnStatus,
			ErrorMessage:    txnErrMsg,
			HandledByRegion: s.region,
		}, nil
	}

	// Step 11: wallet.transfer.completed (AUDIT)
	log.Printf("[WALLET-TX] trace_id=%s step=wallet_proto_response_created status=%s transaction_id=%s", traceID, txnStatus.String(), finalTxnID)
	isSuccess := txnStatus == walletv1.TransferFundsResponse_SUCCESS
	s.emitTerminal(ctx, "wallet.transfer.completed", observability.LevelAudit, "Transfer completed successfully", durationMS, isSuccess, map[string]any{
		"status":         txnStatus.String(),
		"transaction_id": finalTxnID,
		"source_wallet":  req.SourceWalletId,
		"dest_wallet":    req.DestinationWalletId,
		"amount":         req.Amount.Units,
		"currency":       req.Amount.Currency,
	})
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
	mongoURI := db.DefaultMongoURI()
	ledgerAddr := os.Getenv("LEDGER_SERVICE_ADDR")
	if ledgerAddr == "" {
		if _, err := net.LookupHost("ledger-service"); err == nil {
			ledgerAddr = "ledger-service:50052"
		} else {
			ledgerAddr = "127.0.0.1:50052"
		}
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
		logger:       observability.LoggerFromEnvironment("wallet-service", environment, region, os.Stdout),
	}
	walletv1.RegisterWalletServiceServer(grpcServer, srv)

	log.Printf("[WALLET-SERVICE] Listening for gRPC requests on :%s", port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
