# Testing Wallet & Ledger gRPC with BloomRPC

BloomRPC allows calling the **Wallet Service** and **Ledger Service** directly over native gRPC. This tests the internal data plane directly without passing through the HTTP API Gateway.

---

## 1. Prerequisites & Target Endpoints

Ensure the local services are running in your terminal:
- **MongoDB**: `127.0.0.1:27017` (Docker replica set)
- **Ledger Service**: `127.0.0.1:50052` (gRPC)
- **Primary Wallet**: `127.0.0.1:50051` (gRPC — Active Node)
- **Standby Wallet**: `127.0.0.1:50053` (gRPC — Hot DR Standby)

```text
Host Connections in BloomRPC:
├── Primary Wallet: 127.0.0.1:50051 (TLS: Disabled)
├── Standby Wallet: 127.0.0.1:50053 (TLS: Disabled)
└── Ledger Service: 127.0.0.1:50052 (TLS: Disabled)
```

> [!NOTE]
> All payloads below are also saved as machine-readable presets in [postman/BloomRPC_Test_Presets.json](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/postman/BloomRPC_Test_Presets.json).

---

## 2. Importing Protobuf Contracts into BloomRPC

1. Open BloomRPC.
2. Click the **`+`** (Import Protos) icon in the top-left sidebar.
3. Import the following files from the repository:
   - `proto/wallet/wallet.proto`
   - `proto/ledger/ledger.proto`
4. If BloomRPC asks for an **Import Path**, set it to the `proto` folder of this workspace:
   `c:\workarea\personal\After-equifax\global-wallet-microservices\proto`
5. You will see two service trees:
   - `wallet.v1.WalletService` (`CreateWallet`, `GetBalance`, `TransferFunds`, `HealthCheck`)
   - `ledger.v1.LedgerService` (`RecordTransaction`, `GetLedgerEntries`)

---

## 3. Step-by-Step Test Scenarios

### Scenario 1: Verify Active vs Standby Status

#### Test 1A: Primary Node Health
- **Address**: `127.0.0.1:50051`
- **Method**: `wallet.v1.WalletService/HealthCheck`
- **Request Body**: `{}`
- **Expected Response**:
  ```json
  {
    "status": "ACTIVE",
    "region": "us-east-1",
    "is_active": true
  }
  ```

#### Test 1B: Standby Node Health
- **Address**: `127.0.0.1:50053`
- **Method**: `wallet.v1.WalletService/HealthCheck`
- **Request Body**: `{}`
- **Expected Response**:
  ```json
  {
    "status": "STANDBY",
    "region": "eu-west-1",
    "is_active": false
  }
  ```

---

### Scenario 2: Create Wallets (Seed Accounts)

#### Test 2A: Create Alice's Wallet on Primary
- **Address**: `127.0.0.1:50051`
- **Method**: `wallet.v1.WalletService/CreateWallet`
- **Request Body**:
  ```json
  {
    "wallet_id": "bloom-alice",
    "currency": "USD",
    "initial_balance": 5000
  }
  ```
- **Expected Response**:
  ```json
  {
    "success": true,
    "message": "Wallet bloom-alice created successfully",
    "handled_by_region": "us-east-1"
  }
  ```

#### Test 2B: Create Bob's Wallet on Primary
- **Address**: `127.0.0.1:50051`
- **Method**: `wallet.v1.WalletService/CreateWallet`
- **Request Body**:
  ```json
  {
    "wallet_id": "bloom-bob",
    "currency": "USD",
    "initial_balance": 2000
  }
  ```
- **Expected Response**:
  ```json
  {
    "success": true,
    "message": "Wallet bloom-bob created successfully",
    "handled_by_region": "us-east-1"
  }
  ```

#### Test 2C: Standby Write Fencing Verification (GAP-HA-02)
Attempt to create a wallet directly on the **standby** instance:
- **Address**: `127.0.0.1:50053`
- **Method**: `wallet.v1.WalletService/CreateWallet`
- **Request Body**:
  ```json
  {
    "wallet_id": "fenced-wallet",
    "currency": "USD",
    "initial_balance": 100
  }
  ```
- **Expected Error** (`9 FAILED_PRECONDITION`):
  ```text
  code: 9
  message: "instance is standby replica; write operations rejected (region: eu-west-1)"
  ```
  *(Confirms write fencing successfully blocks rogue mutations on standby!)*

---

### Scenario 3: Fund Transfers & Idempotency

#### Test 3A: Execute Transfer on Primary
- **Address**: `127.0.0.1:50051`
- **Method**: `wallet.v1.WalletService/TransferFunds`
- **Request Body**:
  ```json
  {
    "idempotency_key": "bloom-tx-001",
    "source_wallet_id": "bloom-alice",
    "destination_wallet_id": "bloom-bob",
    "amount": {
      "currency": "USD",
      "units": 250
    }
  }
  ```
- **Expected Response**:
  ```json
  {
    "transaction_id": "<hex-string>",
    "status": "SUCCESS",
    "error_message": "",
    "handled_by_region": "us-east-1"
  }
  ```

#### Test 3B: Idempotent Replay (Same Key)
Without changing anything, click **Invoke** again with the same `idempotency_key: "bloom-tx-001"`:
- **Expected Response**:
  ```json
  {
    "transaction_id": "<exact-same-hex-string>",
    "status": "REJECTED_DUPLICATE",
    "error_message": "Transaction already processed",
    "handled_by_region": "us-east-1"
  }
  ```
  *(Confirms duplicate replay is safely trapped without deducting money again!)*

#### Test 3C: Insufficient Funds Rejection
- **Address**: `127.0.0.1:50051`
- **Method**: `wallet.v1.WalletService/TransferFunds`
- **Request Body**:
  ```json
  {
    "idempotency_key": "bloom-overdraft-test",
    "source_wallet_id": "bloom-alice",
    "destination_wallet_id": "bloom-bob",
    "amount": {
      "currency": "USD",
      "units": 999999
    }
  }
  ```
- **Expected Response**:
  ```json
  {
    "status": "FAILED_INSUFFICIENT_FUNDS",
    "error_message": "insufficient funds in source wallet bloom-alice"
  }
  ```

#### Test 3D: Standby Transfer Write Fencing (GAP-HA-02)
Attempt a transfer on the **standby** node:
- **Address**: `127.0.0.1:50053`
- **Method**: `wallet.v1.WalletService/TransferFunds`
- **Request Body**:
  ```json
  {
    "idempotency_key": "bloom-standby-fence-test",
    "source_wallet_id": "bloom-alice",
    "destination_wallet_id": "bloom-bob",
    "amount": {
      "currency": "USD",
      "units": 10
    }
  }
  ```
- **Expected Error** (`9 FAILED_PRECONDITION`):
  ```text
  code: 9
  message: "instance is standby replica; write operations rejected (region: eu-west-1)"
  ```

---

### Scenario 4: Read Balances (Permitted on Standby)

Unlike mutations which are fenced on standby, read queries are **fully permitted**:

#### Test 4A: Check Alice on Standby (`127.0.0.1:50053`)
- **Address**: `127.0.0.1:50053`
- **Method**: `wallet.v1.WalletService/GetBalance`
- **Request Body**:
  ```json
  {
    "wallet_id": "bloom-alice"
  }
  ```
- **Expected Response**:
  ```json
  {
    "wallet_id": "bloom-alice",
    "balances": [
      {
        "currency": "USD",
        "units": 4750
      }
    ],
    "handled_by_region": "eu-west-1"
  }
  ```
  *(Started with $5000, transferred $250 -> Balance is strictly $4750).*

#### Test 4B: Check Bob on Primary (`127.0.0.1:50051`)
- **Address**: `127.0.0.1:50051`
- **Method**: `wallet.v1.WalletService/GetBalance`
- **Request Body**:
  ```json
  {
    "wallet_id": "bloom-bob"
  }
  ```
- **Expected Response**:
  ```json
  {
    "wallet_id": "bloom-bob",
    "balances": [
      {
        "currency": "USD",
        "units": 2250
      }
    ],
    "handled_by_region": "us-east-1"
  }
  ```
  *(Started with $2000, received $250 -> Balance is strictly $2250).*

---

### Scenario 5: Direct Ledger Inspection

Inspect immutable double-entry audit records committed by the Transactional Outbox:
- **Address**: `127.0.0.1:50052`
- **Method**: `ledger.v1.LedgerService/GetLedgerEntries`
- **Request Body**:
  ```json
  {
    "wallet_id": "bloom-alice",
    "limit": 10
  }
  ```
- **Expected Response**:
  ```json
  {
    "entries": [
      {
        "transaction_id": "<hex-string>",
        "idempotency_key": "bloom-tx-001",
        "source_wallet_id": "bloom-alice",
        "destination_wallet_id": "bloom-bob",
        "amount": 250,
        "currency": "USD",
        "timestamp": "<iso-timestamp>",
        "region": "us-east-1"
      }
    ],
    "total_count": 1
  }
  ```

---

### Scenario 6: Distributed Tracing Context Injection (GAP-OBS-01)

BloomRPC allows injecting gRPC metadata headers:
1. In BloomRPC, click the **Metadata** tab below the request editor.
2. Add a W3C traceparent key-value:
   - **Key**: `traceparent`
   - **Value**: `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`
3. Execute any RPC (e.g. `GetBalance`).
4. In the `wallet-service` console output, you will see the OpenTelemetry interceptor extract and link the trace context seamlessly.

---

### Scenario 7: Wallet Account Status & Operational Fencing (GAP-FIN-05)

Test operational account controls and transaction fencing directly via BloomRPC and management HTTP controls:

#### Step 7A: Freeze Alice via Management HTTP Server
In your terminal or Postman, freeze `bloom-alice`:
```bash
curl -X POST http://127.0.0.1:9094/admin/wallet/status \
  -H "Content-Type: application/json" \
  -d '{"wallet_id": "bloom-alice", "status": "FROZEN", "reason": "Operational compliance hold"}'
```
Response: `{"success": true, "wallet_id": "bloom-alice", "status": "FROZEN", "message": "Wallet bloom-alice status updated to FROZEN"}`

#### Step 7B: Attempt Fund Transfer from Frozen Account in BloomRPC
- **Address**: `127.0.0.1:50051`
- **Method**: `wallet.v1.WalletService/TransferFunds`
- **Request Body**:
  ```json
  {
    "idempotency_key": "bloom-tx-frozen-source-001",
    "source_wallet_id": "bloom-alice",
    "destination_wallet_id": "bloom-bob",
    "amount": {
      "currency": "USD",
      "units": 50
    }
  }
  ```
- **Expected Response**:
  ```json
  {
    "status": "INTERNAL_ERROR",
    "error_message": "source wallet bloom-alice is FROZEN; transfers prohibited",
    "handled_by_region": "us-east-1"
  }
  ```
  *(The transfer is prohibited by transactional account fencing; no balances are modified).*

#### Step 7C: Unfreeze Alice via Management HTTP Server
```bash
curl -X POST http://127.0.0.1:9094/admin/wallet/status \
  -H "Content-Type: application/json" \
  -d '{"wallet_id": "bloom-alice", "status": "ACTIVE", "reason": "Hold cleared"}'
```

#### Step 7D: Re-attempt Fund Transfer in BloomRPC
Execute `TransferFunds` with key `"bloom-tx-unfrozen-001"`:
- **Expected Response**:
  ```json
  {
    "transaction_id": "<hex-string>",
    "status": "SUCCESS",
    "handled_by_region": "us-east-1"
  }
  ```

---

### Scenario 8: Cryptographic SHA-256 Audit Verification (GAP-FIN-02)

After running transfers in BloomRPC, verify that all ledger entries conform to GAAP/IFRS balanced double-entry accounting ($\sum \text{Debits} == \sum \text{Credits}$) and that the SHA-256 hash chain is intact:

#### Step 8A: Verify Audit Chain on Ledger Management Server
Execute online verification via HTTP:
```bash
curl -s http://127.0.0.1:9092/audit/verify?wallet_id=bloom-alice
```
- **Expected Response**:
  ```json
  {
    "chain_valid": true,
    "double_entry_balanced": true,
    "entries_verified": 2,
    "genesis_hash": "0000000000000000000000000000000000000000000000000000000000000000",
    "latest_hash": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
    "wallet_id": "bloom-alice"
  }
  ```
  *(Guarantees zero balance tampering and cryptographic chain continuity back to genesis).*

