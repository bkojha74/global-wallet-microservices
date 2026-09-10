package ledgerv1

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestRecordTransactionRequestProtoJSONRoundTrip(t *testing.T) {
	original := &RecordTransactionRequest{
		IdempotencyKey:      "tx-001",
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              25,
		Currency:            "USD",
		Region:              "us-east-1",
	}

	payload, err := protojson.Marshal(original)
	if err != nil {
		t.Fatalf("marshal protobuf JSON: %v", err)
	}

	var decoded RecordTransactionRequest
	if err := protojson.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal protobuf JSON: %v", err)
	}
	if !proto.Equal(original, &decoded) {
		t.Fatalf("round trip changed message: original=%v decoded=%v", original, &decoded)
	}
}

func TestLedgerEntryUsesJSONFieldNames(t *testing.T) {
	payload, err := protojson.Marshal(&LedgerEntry{TransactionId: "tx-001", SourceWalletId: "alice"})
	if err != nil {
		t.Fatalf("marshal protobuf JSON: %v", err)
	}

	jsonPayload := string(payload)
	for _, field := range []string{"transactionId", "sourceWalletId"} {
		if !strings.Contains(jsonPayload, `"`+field+`"`) {
			t.Fatalf("expected protobuf JSON field %q in %s", field, jsonPayload)
		}
	}
}
