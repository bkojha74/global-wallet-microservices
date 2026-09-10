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
			req:  &ledgerv1.RecordTransactionRequest{Amount: 10},
		},
		{
			name: "non-positive amount",
			req: &ledgerv1.RecordTransactionRequest{
				IdempotencyKey: "key-1",
				Amount:         0,
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
