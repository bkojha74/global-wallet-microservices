# Stage 1: Protobuf Generator & Go Multi-Binary Builder
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git protobuf protobuf-dev build-base

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2 && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

WORKDIR /app

# Deterministic dependency caching (GAP-OPS-02)
COPY go.mod go.sum ./
RUN go mod download

COPY proto ./proto

# Compile protocol buffers into Go interfaces & structs
RUN protoc --go_out=. --go_opt=paths=source_relative \
           --go-grpc_out=. --go-grpc_opt=paths=source_relative \
           proto/wallet/wallet.proto \
           proto/ledger/ledger.proto \
           proto/auth/auth.proto

COPY pkg ./pkg
COPY cmd ./cmd

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/wallet-service ./cmd/wallet-service
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/ledger-service ./cmd/ledger-service
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/api-gateway ./cmd/api-gateway
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/logging-service ./cmd/logging-service
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/auth-service ./cmd/auth-service

# Stage 2: Wallet Service Minimal Runtime (GAP-OPS-01)
FROM alpine:3.20 AS wallet-service
RUN apk add --no-cache ca-certificates curl && \
    addgroup -g 10001 -S appgroup && \
    adduser -u 10001 -S appuser -G appgroup
WORKDIR /app
RUN mkdir -p /app/data/logging && chown -R appuser:appgroup /app
COPY --from=builder /bin/wallet-service /app/wallet-service
USER appuser
EXPOSE 50051 50053 9094 9093
ENTRYPOINT ["/app/wallet-service"]

# Stage 3: Ledger Service Minimal Runtime (GAP-OPS-01)
FROM alpine:3.20 AS ledger-service
RUN apk add --no-cache ca-certificates curl && \
    addgroup -g 10001 -S appgroup && \
    adduser -u 10001 -S appuser -G appgroup
WORKDIR /app
RUN mkdir -p /app/data/logging && chown -R appuser:appgroup /app
COPY --from=builder /bin/ledger-service /app/ledger-service
USER appuser
EXPOSE 50052 9092
ENTRYPOINT ["/app/ledger-service"]

# Stage 4: API Gateway Minimal Runtime (GAP-OPS-01)
FROM alpine:3.20 AS api-gateway
RUN apk add --no-cache ca-certificates curl && \
    addgroup -g 10001 -S appgroup && \
    adduser -u 10001 -S appuser -G appgroup
WORKDIR /app
RUN mkdir -p /app/data/logging && chown -R appuser:appgroup /app
COPY --from=builder /bin/api-gateway /app/api-gateway
USER appuser
EXPOSE 8080 8081
ENTRYPOINT ["/app/api-gateway"]

# Stage 5: Logging Service Minimal Runtime (GAP-OPS-01)
FROM alpine:3.20 AS logging-service
RUN apk add --no-cache ca-certificates curl && \
    addgroup -g 10001 -S appgroup && \
    adduser -u 10001 -S appuser -G appgroup
WORKDIR /app
RUN mkdir -p /app/data/logging && chown -R appuser:appgroup /app
COPY --from=builder /bin/logging-service /app/logging-service
USER appuser
EXPOSE 8090 9090
ENTRYPOINT ["/app/logging-service"]

# Stage 6: Auth Service Minimal Runtime
# Isolated identity service — holds the JWT signing key, never co-located with business logic.
FROM alpine:3.20 AS auth-service
RUN apk add --no-cache ca-certificates curl && \
    addgroup -g 10001 -S appgroup && \
    adduser -u 10001 -S appuser -G appgroup
WORKDIR /app
RUN mkdir -p /app/data/logging /app/certs && chown -R appuser:appgroup /app
COPY --from=builder /bin/auth-service /app/auth-service
USER appuser
EXPOSE 50054 9095
ENTRYPOINT ["/app/auth-service"]
