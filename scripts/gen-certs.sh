#!/usr/bin/env bash
# scripts/gen-certs.sh
# Phase 5: Generate self-signed TLS certificates for RabbitMQ mutual TLS.
#
# Usage:
#   chmod +x scripts/gen-certs.sh
#   ./scripts/gen-certs.sh
#
# Output (in config/rabbitmq/tls/):
#   ca.key       CA private key
#   ca.crt       CA certificate
#   server.key   RabbitMQ server private key
#   server.crt   RabbitMQ server certificate (signed by CA)
#   client.key   Go SDK client private key
#   client.crt   Go SDK client certificate (signed by CA)
#
# Set these environment variables to enable TLS in the logging SDK:
#   LOGGING_RABBITMQ_TLS_CERT=config/rabbitmq/tls/client.crt
#   LOGGING_RABBITMQ_TLS_KEY=config/rabbitmq/tls/client.key
#   LOGGING_RABBITMQ_TLS_CA=config/rabbitmq/tls/ca.crt
#
# ⚠️  These certs are for LOCAL DEVELOPMENT ONLY.
#     Use a proper PKI (cert-manager, Vault, AWS ACM, etc.) in production.

set -euo pipefail

TLS_DIR="config/rabbitmq/tls"
mkdir -p "$TLS_DIR"
echo "Generating self-signed certs in $TLS_DIR ..."

# 1. CA
openssl genrsa -out "$TLS_DIR/ca.key" 4096
openssl req -x509 -new -nodes \
    -key "$TLS_DIR/ca.key" \
    -sha256 -days 3650 \
    -subj "/C=US/ST=Dev/L=Local/O=WalletSystem/CN=wallet-ca" \
    -out "$TLS_DIR/ca.crt"

# 2. Server cert (RabbitMQ)
openssl genrsa -out "$TLS_DIR/server.key" 2048
openssl req -new \
    -key "$TLS_DIR/server.key" \
    -subj "/C=US/ST=Dev/L=Local/O=WalletSystem/CN=wallet_rabbitmq" \
    -out "$TLS_DIR/server.csr"
openssl x509 -req \
    -in "$TLS_DIR/server.csr" \
    -CA "$TLS_DIR/ca.crt" \
    -CAkey "$TLS_DIR/ca.key" \
    -CAcreateserial \
    -out "$TLS_DIR/server.crt" \
    -days 825 -sha256 \
    -extfile <(printf "subjectAltName=DNS:wallet_rabbitmq,DNS:localhost,IP:127.0.0.1")
rm "$TLS_DIR/server.csr"

# 3. Client cert (Go SDK / services)
openssl genrsa -out "$TLS_DIR/client.key" 2048
openssl req -new \
    -key "$TLS_DIR/client.key" \
    -subj "/C=US/ST=Dev/L=Local/O=WalletSystem/CN=wallet-client" \
    -out "$TLS_DIR/client.csr"
openssl x509 -req \
    -in "$TLS_DIR/client.csr" \
    -CA "$TLS_DIR/ca.crt" \
    -CAkey "$TLS_DIR/ca.key" \
    -CAcreateserial \
    -out "$TLS_DIR/client.crt" \
    -days 825 -sha256
rm "$TLS_DIR/client.csr"

echo ""
echo "✅  Certificates generated:"
ls -1 "$TLS_DIR/"
echo ""
echo "To enable TLS, set:"
echo "  LOGGING_RABBITMQ_TLS_CERT=$TLS_DIR/client.crt"
echo "  LOGGING_RABBITMQ_TLS_KEY=$TLS_DIR/client.key"
echo "  LOGGING_RABBITMQ_TLS_CA=$TLS_DIR/ca.crt"
echo ""
echo "And in docker-compose.rabbitmq.yml, uncomment the 5671 port"
echo "and the ssl_options in config/rabbitmq/rabbitmq.conf."
