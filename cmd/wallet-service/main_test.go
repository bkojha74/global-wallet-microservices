package main

import (
	"context"
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
	if status.Convert(err).Message() != "idempotency_key, source_wallet_id, destination_wallet_id, amount.currency, and positive amount.units are required" {
		t.Fatalf("unexpected validation error: %v", err)
	}
}
