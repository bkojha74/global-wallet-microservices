package main

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	walletv1 "wallet-system/proto/wallet"
)

func TestTransferFundsRejectsIdenticalWalletsWithoutDatabase(t *testing.T) {
	service := &server{region: "test-region"}
	response, err := service.TransferFunds(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey:      "duplicate-wallet-test",
		SourceWalletId:      "alice",
		DestinationWalletId: "alice",
		Amount:              &walletv1.Money{Currency: "USD", Units: 10},
	})

	if err != nil {
		t.Fatalf("expected application response, got error: %v", err)
	}
	if response.Status != walletv1.TransferFundsResponse_INTERNAL_ERROR {
		t.Fatalf("expected INTERNAL_ERROR, got %s", response.Status.String())
	}
	if response.ErrorMessage != "source and destination wallets cannot be identical" {
		t.Fatalf("unexpected error message: %q", response.ErrorMessage)
	}
	if response.HandledByRegion != "test-region" {
		t.Fatalf("expected test region, got %q", response.HandledByRegion)
	}
}

func TestTransferFundsRejectsMissingWalletIDs(t *testing.T) {
	service := &server{region: "test-region"}
	response, err := service.TransferFunds(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey: "invalid-transfer-test",
		Amount:         &walletv1.Money{Currency: "USD", Units: 50},
	})

	if response != nil {
		t.Fatalf("expected no response, got %+v", response)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "required") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestTransferFundsRejectsInvalidCurrency(t *testing.T) {
	service := &server{region: "test-region"}
	response, err := service.TransferFunds(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey:      "invalid-curr-test",
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              &walletv1.Money{Currency: "FAKECURR", Units: 50},
	})

	if response != nil {
		t.Fatalf("expected no response, got %+v", response)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "invalid or unsupported currency") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestTransferFundsRejectsNonPositiveAmount(t *testing.T) {
	service := &server{region: "test-region"}
	response, err := service.TransferFunds(context.Background(), &walletv1.TransferFundsRequest{
		IdempotencyKey:      "zero-amount-test",
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              &walletv1.Money{Currency: "USD", Units: 0},
	})

	if response != nil {
		t.Fatalf("expected no response, got %+v", response)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestCreateWalletValidation(t *testing.T) {
	service := &server{region: "test-region"}

	// Invalid currency
	_, err := service.CreateWallet(context.Background(), &walletv1.CreateWalletRequest{
		WalletId:       "user-1",
		Currency:       "XYZ",
		InitialBalance: 100,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for bad currency, got %v", err)
	}

	// Negative balance
	_, err = service.CreateWallet(context.Background(), &walletv1.CreateWalletRequest{
		WalletId:       "user-2",
		Currency:       "USD",
		InitialBalance: -50,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for negative balance, got %v", err)
	}

	// Missing wallet ID
	_, err = service.CreateWallet(context.Background(), &walletv1.CreateWalletRequest{
		WalletId:       "",
		Currency:       "USD",
		InitialBalance: 100,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty wallet ID, got %v", err)
	}
}

func TestGetBalanceRejectsMissingWalletID(t *testing.T) {
	service := &server{region: "test-region"}
	_, err := service.GetBalance(context.Background(), &walletv1.GetBalanceRequest{WalletId: ""})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty wallet ID, got %v", err)
	}
}
