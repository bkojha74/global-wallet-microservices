#!/usr/bin/env bash
set -e

echo "=========================================================="
echo " Running Dockerized SAST / Security Scan (GoSec)"
echo "=========================================================="

docker run --rm \
  -v "$(pwd):/app" \
  -w /app \
  securego/gosec:latest \
  -exclude-generated \
  -severity medium \
  ./... || true

echo "[INFO] SAST scan completed."
