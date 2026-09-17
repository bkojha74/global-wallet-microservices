package main

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ledgerv1 "wallet-system/proto/ledger"
)

func TestRecordTransactionRejectsInvalidPayload(t *testing.T) {
	tests := []struct {
		name string
		req  *ledgerv1.RecordTransactionRequest
	}{
		{
			name: "missing idempotency key",
			req:  &ledgerv1.RecordTransactionRequest{Amount: 10, Currency: "USD"},
		},
		{
			name: "non-positive amount",
			req: &ledgerv1.RecordTransactionRequest{
				IdempotencyKey: "key-1",
				Amount:         0,
				Currency:       "USD",
			},
		},
		{
			name: "negative amount",
			req: &ledgerv1.RecordTransactionRequest{
				IdempotencyKey: "key-2",
				Amount:         -100,
				Currency:       "USD",
			},
		},
		{
			name: "unsupported currency",
			req: &ledgerv1.RecordTransactionRequest{
				IdempotencyKey: "key-3",
				Amount:         100,
				Currency:       "FAKE",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &server{}
			response, err := service.RecordTransaction(context.Background(), test.req)

			if response != nil {
				t.Fatalf("expected no response, got %+v", response)
			}
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestGetLedgerEntriesRejectsMissingWalletID(t *testing.T) {
	service := &server{}
	resp, err := service.GetLedgerEntries(context.Background(), &ledgerv1.GetLedgerRequest{WalletId: ""})
	if resp != nil {
		t.Fatalf("expected nil response, got %+v", resp)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

