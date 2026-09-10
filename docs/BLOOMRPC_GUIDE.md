# Testing Wallet gRPC with BloomRPC

BloomRPC can call the Wallet Service directly. This bypasses the HTTP API gateway, so BloomRPC sends a protobuf request and receives a protobuf response over gRPC.

Use this guide with the local hybrid setup:

- MongoDB runs in Docker.
- Ledger, primary wallet, standby wallet, and gateway run with `go run`.

## 1. Start the services

Start MongoDB first:

```powershell
docker compose -f docker-compose.mongodb.yml up -d
```

Then start the application services in separate PowerShell windows as described in the [Local Development Guide](LOCAL_DEVELOPMENT.md#docker-mongodb-with-native-go-services):

1. Ledger service on `127.0.0.1:50052`.
2. Primary wallet service on `127.0.0.1:50051`.
3. Standby wallet service on `127.0.0.1:50053`.
4. API gateway on `127.0.0.1:8080`.

The wallet services must be running before using BloomRPC. Each wallet service also needs to connect to the ledger service at `127.0.0.1:50052`.

Check the gateway only as a general health check:

```powershell
Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:8080/api/v1/cluster/status" | ConvertTo-Json -Depth 5
```

## 2. Install and open BloomRPC

Install BloomRPC from its official GitHub releases page or your organization-approved software source. Open BloomRPC after installation.

BloomRPC is a gRPC client. It does not use the REST gateway URL on port `8080` for these calls.

## 3. Import the wallet protobuf

In BloomRPC:

1. Add or open the file `proto\wallet\wallet.proto`.
2. If BloomRPC asks for imports, add the repository `proto` directory as an import path.
3. Select the service `wallet.v1.WalletService`.
4. Select the method `TransferFunds`.

The method is:

```text
/wallet.v1.WalletService/TransferFunds
```

No generated `.pb.go` file is needed by BloomRPC. BloomRPC reads the `.proto` contract directly.

## 4. Configure the local gRPC connection

Create a BloomRPC request for the primary wallet:

```text
Host: 127.0.0.1:50051
TLS/SSL: Disabled
``` 

The application uses insecure local gRPC transport, so do not enable TLS for this local test.

To call the standby wallet directly, use a second request or change the host to:

```text
127.0.0.1:50053
```

Calling port `50051` or `50053` directly does not change the gateway route. It sends the request straight to the selected wallet service.

## 5. Prepare test wallets

Create wallets through the REST gateway so the HTTP-to-protobuf path is also initialized:

```powershell
$body = @{ wallet_id = "bloom-alice"; currency = "USD"; initial_balance = 1000 } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/wallets" -ContentType "application/json" -Body $body | ConvertTo-Json

$body = @{ wallet_id = "bloom-bob"; currency = "USD"; initial_balance = 100 } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/wallets" -ContentType "application/json" -Body $body | ConvertTo-Json
```

You may also select `CreateWallet` in BloomRPC and send this protobuf request:

```json
{
  "wallet_id": "bloom-alice",
  "currency": "USD",
  "initial_balance": 1000
}
```

## 6. Send a transfer from BloomRPC

In the `TransferFunds` request body, use the nested protobuf shape:

```json
{
  "idempotency_key": "bloomrpc-tx-001",
  "source_wallet_id": "bloom-alice",
  "destination_wallet_id": "bloom-bob",
  "amount": {
    "currency": "USD",
    "units": 250
  }
}
```

BloomRPC versions differ in how they map protobuf JSON names. For this local BloomRPC setup, use the original `snake_case` names from `wallet.proto`. Some protobuf JSON clients also accept the camelCase aliases, but this BloomRPC request is arriving with the three top-level string fields empty when camelCase is used.

- `idempotency_key`
- `source_wallet_id`
- `destination_wallet_id`
- `amount.currency`
- `amount.units`

Click **Run** or **Invoke** in BloomRPC.

A successful response should look similar to:

```json
{
  "transaction_id": "<generated-transaction-id>",
  "status": "SUCCESS",
  "handled_by_region": "us-east-1-primary"
}
```

When calling the standby host directly, `handled_by_region` should identify the standby region, for example `eu-west-1-standby`.

## 7. Important idempotency rule

Every new transfer must use a new `idempotency_key`.

Do not reuse:

```text
bloomrpc-tx-001
```

for another transfer. Reusing it intentionally returns:

```text
status: REJECTED_DUPLICATE
```

To retry the same transfer safely, reuse the key. To create a new transfer, change the key:

```json
{
  "idempotency_key": "bloomrpc-tx-002",
  "source_wallet_id": "bloom-alice",
  "destination_wallet_id": "bloom-bob",
  "amount": {
    "currency": "USD",
    "units": 50
  }
}
```

## 8. Verify the response and data

### Verify through REST

Read the balances through the gateway:

```powershell
Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:8080/api/v1/wallets?id=bloom-alice" | ConvertTo-Json -Depth 5
Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:8080/api/v1/wallets?id=bloom-bob" | ConvertTo-Json -Depth 5
Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:8080/api/v1/ledger?wallet_id=bloom-alice" | ConvertTo-Json -Depth 5
```

After the 250 USD transfer, expected balances are:

```text
bloom-alice: 750 USD
bloom-bob:   350 USD
```

### Verify service logs

The wallet service should log the protobuf request and transaction steps, including:

```text
step=wallet_proto_request_received
step=mongo_transaction_started
step=source_wallet_debited
step=destination_wallet_credited
step=ledger_grpc_request
step=ledger_grpc_response
step=wallet_proto_response_created
```

The ledger service should log:

```text
proto request received
step=idempotency_check
step=ledger_document_persisted
```

When using BloomRPC directly, the API gateway will not log `http_json_received` or `http_json_to_wallet_proto_request`, because BloomRPC does not pass through HTTP. The wallet and ledger protobuf logs are still produced.

### Verify through MongoDB

Using `mongosh` or MongoDB Compass, inspect the `banking_db` database:

```javascript
use banking_db

db.wallets.find({ _id: { $in: ["bloom-alice", "bloom-bob"] } })
db.idempotency_records.findOne({ _id: "bloomrpc-tx-001" })
db.ledger_entries.findOne({ idempotency_key: "bloomrpc-tx-001" })
```

You should find:

- Updated wallet balances.
- One idempotency record mapped to the transaction ID.
- One ledger entry for the transfer.

## 9. Test primary and standby directly

To test the primary wallet directly, set BloomRPC host to:

```text
127.0.0.1:50051
```

To test the standby wallet directly, set BloomRPC host to:

```text
127.0.0.1:50053
```

Use a new idempotency key for the standby test:

```json
{
  "idempotency_key": "bloomrpc-standby-tx-001",
  "source_wallet_id": "bloom-bob",
  "destination_wallet_id": "bloom-alice",
  "amount": {
    "currency": "USD",
    "units": 25
  }
}
```

Because both wallet services share MongoDB, the standby can see wallets and idempotency records created through the primary.

## 10. Troubleshooting

### Connection refused

Check that the selected wallet service is running:

```powershell
Test-NetConnection 127.0.0.1 -Port 50051
Test-NetConnection 127.0.0.1 -Port 50053
```

### Unimplemented or service not found

Make sure BloomRPC imported `proto\wallet\wallet.proto` and selected `wallet.v1.WalletService`, not the REST gateway port `8080`.

### Invalid argument

Check that:

- `idempotency_key` is present.
- `source_wallet_id` and `destination_wallet_id` are present and different.
- `amount.units` is greater than zero.
- Source and destination wallet IDs are different.
- Both wallets use the requested currency.

If the wallet console logs `source= destination=` or the response says that the source and destination wallets are identical, the protobuf request arrived with empty wallet IDs. Replace the entire BloomRPC request body with this exact shape and invoke it again:

```json
{
  "idempotency_key": "bloomrpc-new-001",
  "source_wallet_id": "bipin",
  "destination_wallet_id": "ruby",
  "amount": {
    "currency": "USD",
    "units": 50
  }
}
```

Restart the native wallet process after pulling a code update. The service now returns an explicit `InvalidArgument` error when required transfer fields are empty.

### Duplicate response

Use a new idempotency key for a new transaction. Do not delete idempotency records to work around this behavior.
