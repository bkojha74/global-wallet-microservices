package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"google.golang.org/grpc/metadata"
)

const (
	AssociationIDHeader  = "X-Association-ID"
	TransactionIDHeader  = "X-Transaction-ID"
	IdempotencyKeyHeader = "X-Idempotency-Key"

	associationMetadataKey = "x-association-id"
	transactionMetadataKey = "x-transaction-id"
	idempotencyMetadataKey = "x-idempotency-key"
)

type Correlation struct {
	AssociationID  string
	TransactionID  string
	IdempotencyKey string
}

type contextKey struct{}

func NewAssociationID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return hex.EncodeToString(value[:])
	}
	return hex.EncodeToString(value[:])
}

func FromHTTPRequest(r *http.Request) Correlation {
	correlation := Correlation{
		AssociationID:  r.Header.Get(AssociationIDHeader),
		TransactionID:  r.Header.Get(TransactionIDHeader),
		IdempotencyKey: r.Header.Get(IdempotencyKeyHeader),
	}
	if correlation.AssociationID == "" {
		correlation.AssociationID = NewAssociationID()
	}
	return correlation
}

func WithCorrelation(ctx context.Context, correlation Correlation) context.Context {
	return context.WithValue(ctx, contextKey{}, correlation)
}

func FromContext(ctx context.Context) Correlation {
	correlation, _ := ctx.Value(contextKey{}).(Correlation)
	return correlation
}

func FromIncomingContext(ctx context.Context) Correlation {
	correlation := FromContext(ctx)
	metadata, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return correlation
	}
	if correlation.AssociationID == "" {
		correlation.AssociationID = firstMetadataValue(metadata, associationMetadataKey)
	}
	if correlation.TransactionID == "" {
		correlation.TransactionID = firstMetadataValue(metadata, transactionMetadataKey)
	}
	if correlation.IdempotencyKey == "" {
		correlation.IdempotencyKey = firstMetadataValue(metadata, idempotencyMetadataKey)
	}
	if correlation.AssociationID == "" {
		correlation.AssociationID = NewAssociationID()
	}
	return correlation
}

func WithOutgoingMetadata(ctx context.Context, correlation Correlation) context.Context {
	metadataPairs := []string{associationMetadataKey, correlation.AssociationID}
	if correlation.TransactionID != "" {
		metadataPairs = append(metadataPairs, transactionMetadataKey, correlation.TransactionID)
	}
	if correlation.IdempotencyKey != "" {
		metadataPairs = append(metadataPairs, idempotencyMetadataKey, correlation.IdempotencyKey)
	}
	return metadata.AppendToOutgoingContext(ctx, metadataPairs...)
}

func firstMetadataValue(values metadata.MD, key string) string {
	items := values.Get(key)
	if len(items) == 0 {
		return ""
	}
	return items[0]
}
