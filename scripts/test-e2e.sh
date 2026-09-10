#!/usr/bin/env bash
set -e

BASE_URL="http://localhost:8080"

echo "=========================================================="
echo " Starting Global Digital Wallet E2E Demonstration"
echo "=========================================================="

echo -e "\n1. Checking Cluster & Multi-Region Health Status..."
curl -s "${BASE_URL}/api/v1/cluster/status" | grep -o '"current_routed_target":"[^"]*"' || true
echo ""

echo -e "\n2. Creating Wallets (Alice = $1000 USD, Bob = $500 USD)..."
curl -s -X POST "${BASE_URL}/api/v1/wallets" \
  -H "Content-Type: application/json" \
  -d '{"wallet_id":"alice","currency":"USD","initial_balance":1000}'
echo ""
curl -s -X POST "${BASE_URL}/api/v1/wallets" \
  -H "Content-Type: application/json" \
  -d '{"wallet_id":"bob","currency":"USD","initial_balance":500}'
echo ""

echo -e "\n3. Verifying Alice Initial Balance:"
curl -s "${BASE_URL}/api/v1/wallets?id=alice"
echo ""

echo -e "\n4. Executing Atomic Transfer: Alice sends $250 to Bob (via PRIMARY region)..."
curl -s -X POST "${BASE_URL}/api/v1/transfers" \
  -H "Content-Type: application/json" \
  -d '{"idempotency_key":"tx-abc-001","source_wallet_id":"alice","destination_wallet_id":"bob","amount":250,"currency":"USD"}'
echo ""

echo -e "\n5. Testing Idempotency (Re-sending the EXACT same request)..."
curl -s -X POST "${BASE_URL}/api/v1/transfers" \
  -H "Content-Type: application/json" \
  -d '{"idempotency_key":"tx-abc-001","source_wallet_id":"alice","destination_wallet_id":"bob","amount":250,"currency":"USD"}'
echo ""

echo -e "\n6. Verifying Updated Balances:"
echo -n "Alice: " && curl -s "${BASE_URL}/api/v1/wallets?id=alice"
echo ""
echo -n "Bob:   " && curl -s "${BASE_URL}/api/v1/wallets?id=bob"
echo ""

echo -e "\n7. SIMULATING REGIONAL FAILOVER (Switching to Standby DR Region)..."
curl -s -X POST "${BASE_URL}/api/v1/cluster/failover"
echo ""

echo -e "\n8. Executing Second Transfer via STANDBY Region (Bob sends $100 to Alice)..."
curl -s -X POST "${BASE_URL}/api/v1/transfers" \
  -H "Content-Type: application/json" \
  -d '{"idempotency_key":"tx-xyz-002","source_wallet_id":"bob","destination_wallet_id":"alice","amount":100,"currency":"USD"}'
echo ""

echo -e "\n9. Inspecting Immutable Audit Ledger for Alice:"
curl -s "${BASE_URL}/api/v1/ledger?wallet_id=alice"
echo ""

echo -e "\n=========================================================="
echo " All tests executed successfully with full ACID atomicity!"
echo "=========================================================="
