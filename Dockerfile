# Stage 1: Protobuf Generator & Go Multi-Binary Builder
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git protobuf protobuf-dev build-base

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2 && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

WORKDIR /app

COPY go.mod ./
COPY proto ./proto

# Compile protocol buffers into Go interfaces & structs
RUN protoc --go_out=. --go_opt=paths=source_relative \
           --go-grpc_out=. --go-grpc_opt=paths=source_relative \
           proto/wallet/wallet.proto \
           proto/ledger/ledger.proto

COPY pkg ./pkg
COPY cmd ./cmd

RUN go mod tidy

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/wallet-service ./cmd/wallet-service
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/ledger-service ./cmd/ledger-service
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/api-gateway ./cmd/api-gateway

# Stage 2: Wallet Service Minimal Runtime
FROM alpine:3.20 AS wallet-service
RUN apk add --no-cache ca-certificates curl
WORKDIR /app
COPY --from=builder /bin/wallet-service /app/wallet-service
EXPOSE 50051 50053
ENTRYPOINT ["/app/wallet-service"]

# Stage 3: Ledger Service Minimal Runtime
FROM alpine:3.20 AS ledger-service
RUN apk add --no-cache ca-certificates curl
WORKDIR /app
COPY --from=builder /bin/ledger-service /app/ledger-service
EXPOSE 50052
ENTRYPOINT ["/app/ledger-service"]

# Stage 4: API Gateway Minimal Runtime
FROM alpine:3.20 AS api-gateway
RUN apk add --no-cache ca-certificates curl
WORKDIR /app
COPY --from=builder /bin/api-gateway /app/api-gateway
EXPOSE 8080
ENTRYPOINT ["/app/api-gateway"]
