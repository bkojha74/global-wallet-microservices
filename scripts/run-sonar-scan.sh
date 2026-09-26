#!/usr/bin/env bash
set -e

SONAR_HOST="${SONAR_HOST_URL:-http://sonarqube:9000}"
TOKEN="${1:-$SONAR_TOKEN}"

echo "=========================================================="
echo " Running Dockerized SonarQube Scanner"
echo " Target: ${SONAR_HOST}"
echo "=========================================================="

if [ -z "$TOKEN" ]; then
  echo "[WARNING] No SONAR_TOKEN provided via argument or environment variable."
  echo "SonarQube requires an authentication token to submit an analysis."
  echo ""
  echo "How to get a token:"
  echo "  1. Open http://localhost:9000 in your browser"
  echo "  2. Sign in with 'admin' / 'admin' (update your password if prompted)"
  echo "  3. Go to User Profile -> Security (http://localhost:9000/account/security)"
  echo "  4. Generate a Token (e.g. 'local-scan-token')"
  echo "  5. Re-run: ./scripts/run-sonar-scan.sh <YOUR_TOKEN>"
  echo ""
fi

echo "Generating Go test coverage profile (coverage.out)..."
go test "-coverprofile=coverage.out" ./... || true

docker run --rm \
  --network wallet_shared_net \
  -v "$(pwd):/usr/src" \
  -e SONAR_HOST_URL="${SONAR_HOST}" \
  -e SONAR_TOKEN="${TOKEN}" \
  sonarsource/sonar-scanner-cli:latest

echo ""
echo "[SUCCESS] SonarQube scan complete!"
echo "View results at: http://localhost:9000/dashboard?id=global-wallet-microservices"
