package walletv1

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestTransferFundsRequestProtoJSONRoundTrip(t *testing.T) {
	original := &TransferFundsRequest{
		IdempotencyKey:      "tx-001",
		SourceWalletId:      "alice",
		DestinationWalletId: "bob",
		Amount:              &Money{Currency: "USD", Units: 25},
	}

	payload, err := protojson.Marshal(original)
	if err != nil {
		t.Fatalf("marshal protobuf JSON: %v", err)
	}

	var decoded TransferFundsRequest
	if err := protojson.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal protobuf JSON: %v", err)
	}
	if !proto.Equal(original, &decoded) {
		t.Fatalf("round trip changed message: original=%v decoded=%v", original, &decoded)
	}
}

func TestTransferFundsResponseStatusNames(t *testing.T) {
	response := &TransferFundsResponse{Status: TransferFundsResponse_REJECTED_DUPLICATE}
	if response.Status.String() != "REJECTED_DUPLICATE" {
		t.Fatalf("unexpected status name: %s", response.Status.String())
	}

	payload, err := protojson.Marshal(response)
	if err != nil {
		t.Fatalf("marshal protobuf JSON: %v", err)
	}
	if string(payload) != `{"status":"REJECTED_DUPLICATE"}` {
		t.Fatalf("unexpected protobuf JSON: %s", payload)
	}
}
