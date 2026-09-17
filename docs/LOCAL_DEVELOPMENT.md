# Local Development on Windows

This guide covers running the Global Wallet Microservices project on Windows with either Docker MongoDB or native MongoDB. It includes:

- Docker MongoDB deployment as a single-node replica set.
- Optional native MongoDB Community Server deployment as a single-node replica set.
- Starting the ledger, wallet, and API gateway processes locally.
- Testing the HTTP API and following transaction logs.
- Connecting to MongoDB with MongoDB Compass.
- Running and organizing Go unit tests.

Docker Desktop is the recommended local MongoDB option because it does not require installing the MongoDB server binary.

For Docker-based deployment, MongoDB is deliberately separate from the application services. Start `docker-compose.mongodb.yml` first, then start `docker-compose.yml`. This allows other projects to join the `wallet_shared_net` network without starting another MongoDB container.

## 1. Prerequisites

Required for the Docker-based workflow:

- Go 1.23 or later. Your Go 1.27.1 is compatible with this module.
- MongoDB Shell (`mongosh`).
- Protocol Buffers compiler (`protoc`).
- Go protobuf plugins: `protoc-gen-go` and `protoc-gen-go-grpc`.
- `curl.exe` (included with current Windows versions).
- MongoDB Compass, if you want a graphical database client.

Required only for the native MongoDB workflow:

- MongoDB Community Server, including the `mongod` executable.

`mongod` is not required when MongoDB is running through Docker Compose. Your current output confirms that `mongosh` is installed, but `mongod` is not installed or not on `PATH`, so use the Docker option below.

Check the tools from PowerShell:

```powershell
go version
mongosh --version
protoc --version
protoc-gen-go --version
protoc-gen-go-grpc --version
curl.exe --version
```

Expected tool versions from the current verified environment are:

```text
Go:              1.27.1
MongoDB Shell:   2.5.10
protoc:          33.2
protoc-gen-go:   v1.36.11
protoc-gen-go-grpc: 1.6.0
curl:            8.21.0
```

These versions are compatible with the project. The Docker MongoDB image supplies the `mongod` server separately.

The Go protobuf plugins can be installed with:

```powershell
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
```

Make sure `$env:GOBIN` or `$env:GOPATH\bin` is on `PATH`. Open a new PowerShell window after changing `PATH`.

## 2. Open the repository

Use the repository root for all commands:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
```

Download Go dependencies:

```powershell
go mod download
go mod tidy
```

### Generate protobuf files when needed

The Docker build generates protobuf files automatically. For a native build, generate them manually if `proto\wallet\*.pb.go` or `proto\ledger\*.pb.go` is missing:

```powershell
protoc --go_out=. --go_opt=paths=source_relative `
  --go-grpc_out=. --go-grpc_opt=paths=source_relative `
  proto\wallet\wallet.proto proto\ledger\ledger.proto
```

Verify that generated files exist:

```powershell
Get-ChildItem proto\wallet,proto\ledger -Filter "*.go"
```

## 3. Deploy MongoDB locally

### Docker option

If Docker Desktop is available, use the dedicated database Compose file from the repository root:

```powershell
docker network inspect wallet_shared_net *> $null
if ($LASTEXITCODE -ne 0) { docker network create wallet_shared_net }
docker volume inspect global-wallet-microservices_mongo_data *> $null
if ($LASTEXITCODE -ne 0) { docker volume create global-wallet-microservices_mongo_data }
docker compose -f docker-compose.mongodb.yml up -d
docker compose -f docker-compose.mongodb.yml ps
```

Then start the wallet application in a separate command:

```powershell
docker compose -f docker-compose.yml up --build -d
```

The application Compose file uses the existing external network `wallet_shared_net` and does not own MongoDB. Stop the application without affecting MongoDB with:

```powershell
docker compose -f docker-compose.yml down
```

Stop MongoDB separately with:

```powershell
docker compose -f docker-compose.mongodb.yml down
```

The MongoDB container is named `wallet_mongodb`, the Docker hostname is `mongodb`, and the persistent volume is `wallet_mongo_data`.

For another Docker project to use this MongoDB instance, attach its services to this external network:

```yaml
networks:
  wallet_shared_net:
    external: true
    name: wallet_shared_net
```

Use `mongodb://mongodb:27017/?replicaSet=rs0&directConnection=true` from containers on that network. Use `mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true` from Windows applications such as Compass.

## Docker MongoDB with native Go services

Use this workflow when only MongoDB should run in Docker. Do not start the application Compose file; it would also start Docker versions of the ledger, wallet, and gateway services.

### 1. Start MongoDB and RabbitMQ

From the repository root:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
docker network inspect wallet_shared_net *> $null
if ($LASTEXITCODE -ne 0) { docker network create wallet_shared_net }
docker volume inspect global-wallet-microservices_mongo_data *> $null
if ($LASTEXITCODE -ne 0) { docker volume create global-wallet-microservices_mongo_data }
docker compose -f docker-compose.mongodb.yml up -d
docker compose -f docker-compose.rabbitmq.yml up -d
docker compose -f docker-compose.mongodb.yml ps
docker compose -f docker-compose.rabbitmq.yml ps
```

Wait until both `wallet_mongodb` and `wallet_rabbitmq` show `healthy`. Verify the replica set:

```powershell
mongosh.exe --host 127.0.0.1:27017 --quiet --eval "rs.status().members.map(m => ({name: m.name, stateStr: m.stateStr, health: m.health}))"
```

Expected result includes `stateStr: 'PRIMARY'` and `health: 1`.

### 2. Start the ledger service

Open a new PowerShell window and leave it running:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:PORT = "50052"
$env:MONGO_URI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
$env:REGION_NAME = "global-core"
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run .\cmd\ledger-service
```

Wait for:

```text
[LEDGER-SERVICE] gRPC listening on :50052
```

### 3. Start the primary wallet service

Open another PowerShell window:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:PORT = "50051"
$env:MONGO_URI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
$env:LEDGER_SERVICE_ADDR = "127.0.0.1:50052"
$env:REGION_NAME = "us-east-1-primary"
$env:IS_ACTIVE = "true"
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run .\cmd\wallet-service
```

Wait for:

```text
[WALLET-SERVICE] Listening for gRPC requests on :50051
```

### 4. Start the standby wallet service

Open another PowerShell window:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:PORT = "50053"
$env:MONGO_URI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
$env:LEDGER_SERVICE_ADDR = "127.0.0.1:50052"
$env:REGION_NAME = "eu-west-1-standby"
$env:IS_ACTIVE = "false"
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run .\cmd\wallet-service
```

Wait for:

```text
[WALLET-SERVICE] Listening for gRPC requests on :50053
```

### 5. Start the API gateway

Open a fourth PowerShell window:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:HTTP_PORT = "8080"
$env:PRIMARY_WALLET_ADDR = "127.0.0.1:50051"
$env:STANDBY_WALLET_ADDR = "127.0.0.1:50053"
$env:LEDGER_ADDR = "127.0.0.1:50052"
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run .\cmd\api-gateway
```

Wait for:

```text
[API-GATEWAY] HTTP REST Gateway listening on :8080
```

### 6. Start the logging service

Open a fifth PowerShell window:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:MONGO_URI = "mongodb://127.0.0.1:27017"
$env:RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run .\cmd\logging-service
```

Wait for:

```text
[LOGGING-SERVICE] Starting RabbitMQ consumer...
```

### 7. Verify the native application

The gateway is now available at `http://127.0.0.1:8080`:

```powershell
Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:8080/api/v1/cluster/status" | ConvertTo-Json -Depth 5
```

The response should show `PRIMARY` as the current route, with the primary wallet reported as `ACTIVE` and standby reported as `STANDBY`.

Run the API examples in this guide to create wallets and transfer funds. The native services connect to Docker MongoDB through `127.0.0.1:27017`; they do not use the Docker hostname `mongodb`.

### 7. Stop the hybrid setup

Press `Ctrl+C` in each Go service window. Stop only MongoDB with:

```powershell
docker compose -f docker-compose.mongodb.yml down
```

Do not run `docker compose -f docker-compose.yml up` during this workflow, because that starts duplicate application containers on ports `50051`, `50052`, `50053`, and `8080`.

### Native Windows option

### 3.1 Create a local data directory

Run PowerShell as a user who can write to the selected directory:

```powershell
New-Item -ItemType Directory -Force -Path "C:\data\mongodb\db" | Out-Null
New-Item -ItemType Directory -Force -Path "C:\data\mongodb\log" | Out-Null
```

### 3.2 Create a replica-set configuration

Transactions require a replica set. A standalone MongoDB process is not sufficient for `TransferFunds`.

Create `C:\data\mongodb\mongod-local.cfg` with this content:

```yaml
storage:
  dbPath: C:\data\mongodb\db
systemLog:
  destination: file
  path: C:\data\mongodb\log\mongod.log
  logAppend: true
net:
  bindIp: 127.0.0.1
  port: 27017
replication:
  replSetName: rs0
```

If MongoDB is already running as a Windows service on port `27017`, stop it before starting this instance:

```powershell
Get-Service MongoDB -ErrorAction SilentlyContinue
Stop-Service MongoDB -ErrorAction SilentlyContinue
```

Start MongoDB in its own PowerShell window and leave that window running:

```powershell
mongod.exe --config "C:\data\mongodb\mongod-local.cfg"
```

### 3.3 Initialize the replica set

In a second PowerShell window, run:

```powershell
mongosh.exe --host 127.0.0.1:27017 --eval 'rs.initiate({_id: "rs0", members: [{_id: 0, host: "127.0.0.1:27017"}]})'
```

Check that the node is primary:

```powershell
mongosh.exe --host 127.0.0.1:27017 --eval 'rs.status().members | map(m => ({name: m.name, stateStr: m.stateStr, health: m.health}))'
```

Expected output includes a member with:

```text
stateStr: "PRIMARY"
health: 1
```

If the replica set was already initialized, `rs.initiate` may report that it is already configured. That is fine; use `rs.status()` to verify the state.

The local application URI is:

```text
mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true
```

Do not use the Compose hostname `mongodb` when running the Go services directly on Windows.

## 4. Start the services locally

Use a separate PowerShell window for each process. Start MongoDB first, then the ledger, wallet services, and gateway.

### 4.1 Ledger service

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:PORT = "50052"
$env:MONGO_URI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
$env:REGION_NAME = "global-core"
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run .\cmd\ledger-service
```

Wait for a log similar to:

```text
[LEDGER-SERVICE] gRPC listening on :50052
```

### 4.2 Primary wallet service

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:PORT = "50051"
$env:MONGO_URI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
$env:LEDGER_SERVICE_ADDR = "127.0.0.1:50052"
$env:REGION_NAME = "us-east-1-primary"
$env:IS_ACTIVE = "true"
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run .\cmd\wallet-service
```

Wait for:

```text
[WALLET-SERVICE] Listening for gRPC requests on :50051
```

### 4.3 Standby wallet service

Run the same service in another PowerShell window with a different port and standby settings:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:PORT = "50053"
$env:MONGO_URI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
$env:LEDGER_SERVICE_ADDR = "127.0.0.1:50052"
$env:REGION_NAME = "eu-west-1-standby"
$env:IS_ACTIVE = "false"
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run .\cmd\wallet-service
```

### 4.4 API gateway

Run the gateway in a fourth PowerShell window:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:HTTP_PORT = "8080"
$env:PRIMARY_WALLET_ADDR = "127.0.0.1:50051"
$env:STANDBY_WALLET_ADDR = "127.0.0.1:50053"
$env:LEDGER_ADDR = "127.0.0.1:50052"
$env:LOGGING_RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
$env:JWT_SECRET = "dev-secret-key-change-in-production"
# Optional: enable mTLS across all gRPC services (requires running `go run scripts/generate_certs.go` first)
# $env:GRPC_TLS_ENABLED = "true"
go run .\cmd\api-gateway
```

Wait for:

```text
[API-GATEWAY] HTTP REST Gateway listening on :8080
```

### 4.5 Logging service

Run the logging service in a fifth PowerShell window:

```powershell
Set-Location "C:\workarea\personal\After-equifax\global-wallet-microservices"
$env:MONGO_URI = "mongodb://127.0.0.1:27017"
$env:RABBITMQ_URL = "amqp://guest:guest@localhost:5672/"
go run .\cmd\logging-service
```

Wait for:

```text
[LOGGING-SERVICE] Starting RabbitMQ consumer...
```

The gateway selects the primary wallet service initially. The failover endpoint toggles between ports `50051` and `50053` in memory.

## 5. Test the local HTTP API

With **Phase 2 (Zero-Trust Security & Identity)** active, the API Gateway enforces cryptographic JWT Bearer authentication, Insecure Direct Object Reference (IDOR) verification, rate limiting, and administrative RBAC.

Unauthenticated calls or calls acting on another user's wallet without an `admin` role will return `401 Unauthorized` or `403 Forbidden`.

### 5.0 Mint JWT Authentication Tokens

The gateway provides a local development minting endpoint `POST /api/v1/auth/token`. Mint tokens for `alice` (standard user), `bob` (standard user), and `admin` (operations administrator):

```powershell
$AliceToken = (Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/auth/token?sub=alice&role=user").token
$BobToken   = (Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/auth/token?sub=bob&role=user").token
$AdminToken = (Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/auth/token?sub=admin&role=admin").token

$AliceHeaders = @{ Authorization = "Bearer $AliceToken" }
$BobHeaders   = @{ Authorization = "Bearer $BobToken" }
$AdminHeaders = @{ Authorization = "Bearer $AdminToken" }
```

### 5.1 Check service health & status (Public)

```powershell
curl.exe http://127.0.0.1:8080/healthz
curl.exe http://127.0.0.1:8080/readyz
curl.exe http://127.0.0.1:8080/api/v1/cluster/status
```

### 5.2 Create wallets

Create initial wallets for Alice and Bob. Note: If a wallet ID already exists, the API returns `409 Conflict`:

```powershell
$body = @{ wallet_id = "alice"; currency = "USD"; initial_balance = 1000 } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/wallets" -Headers $AliceHeaders -ContentType "application/json" -Body $body | ConvertTo-Json

$body = @{ wallet_id = "bob"; currency = "USD"; initial_balance = 500 } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/wallets" -Headers $BobHeaders -ContentType "application/json" -Body $body | ConvertTo-Json
```

### 5.3 Transfer funds & test idempotency

Alice transfers $250 USD to Bob:

```powershell
$body = @{ idempotency_key = "local-tx-001"; source_wallet_id = "alice"; destination_wallet_id = "bob"; amount = 250; currency = "USD" } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/transfers" -Headers $AliceHeaders -ContentType "application/json" -Body $body | ConvertTo-Json
```

Repeat the exact command with the same `idempotency_key`. The second response returns the cached transaction with status `REJECTED_DUPLICATE` and will not debit Alice again.

### 5.4 Test IDOR security (Insecure Direct Object Reference)

Attempting to read Bob's balance using Alice's token is blocked:

```powershell
# Expected: 403 Forbidden (claims.Subject != wallet_id)
curl.exe -i -H "Authorization: Bearer $AliceToken" "http://127.0.0.1:8080/api/v1/wallets?id=bob"
```

Authorized balance and ledger inquiries:

```powershell
curl.exe -s -H "Authorization: Bearer $AliceToken" "http://127.0.0.1:8080/api/v1/wallets?id=alice"
curl.exe -s -H "Authorization: Bearer $BobToken" "http://127.0.0.1:8080/api/v1/wallets?id=bob"
curl.exe -s -H "Authorization: Bearer $AliceToken" "http://127.0.0.1:8080/api/v1/ledger?wallet_id=alice"
```

### 5.5 Test failover (Requires Admin RBAC)

Attempting failover without an admin token returns `401 Unauthorized` or `403 Forbidden`. Execute failover using `$AdminHeaders`:

```powershell
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/cluster/failover" -Headers $AdminHeaders | ConvertTo-Json
curl.exe http://127.0.0.1:8080/api/v1/cluster/status
```

### 5.6 Automated Testing via Postman or Bruno

A complete, 24-test automated suite is provided in the repository under [postman/](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/postman):
- `postman/Global_Wallet_Microservices.postman_collection.json`
- `postman/Global_Wallet_Local.postman_environment.json`

**To run in Postman or Bruno**:
1. Open Postman or Bruno.
2. In Bruno: Click **Import Collection** -> Select **Postman Collection** -> choose `postman/Global_Wallet_Microservices.postman_collection.json`. Then import the environment file.
3. In Postman: Click **Import** -> Select both files -> Select the `Global Wallet (Local Environment)` environment.
4. Run the full collection to automatically verify:
   - System Health (`/healthz`, `/readyz`, `/api/v1/cluster/status`)
   - Authentication & Token Minting (`/api/v1/auth/token`)
   - Security Protections (Missing Auth 401, Invalid Token 401, IDOR 403, Rate Limiting 429)
   - Wallet Management (Alice $1000, Bob $500, Duplicate 409 Conflict)
   - Transfers & Idempotency (Atomic transfer, duplicate key replay, insufficient funds)
   - Disaster Recovery Failover (Admin-only failover, route verification, reset)

## 6. Trace one transaction in logs

Use the transfer idempotency key as the trace ID. For `local-tx-001`, look for these steps across the three service windows:

```text
http_json_decoded
http_json_to_wallet_proto_request
wallet_grpc_request
wallet_proto_request_received
mongo_transaction_started
idempotency_check
source_wallet_debited
destination_wallet_credited
ledger_grpc_request
proto request received
ledger_document_persisted
ledger_grpc_response
idempotency_record_stored
wallet_proto_response_created
proto_to_http_json_response
```

The gateway also logs protobuf payloads using `protojson`, which lets you compare the incoming HTTP shape with the generated protobuf shape.

## 7. Connect with MongoDB Compass

1. Open MongoDB Compass.
2. Select **New Connection**.
3. Enter this connection string:

   ```text
   mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true
   ```

4. Select **Connect**.
5. Open the `banking_db` database.
6. Inspect these collections:

   - `wallets`: current wallet balances.
   - `idempotency_records`: transfer key to transaction ID mappings.
   - `ledger_entries`: immutable audit records.

You can also connect with the shorter host/port fields:

```text
Host: 127.0.0.1
Port: 27017
Authentication: None
```

For this local setup, authentication is not enabled. Do not expose this unauthenticated MongoDB listener beyond localhost.

Useful Compass filters:

```javascript
// wallets
{ _id: "alice" }

// one idempotency record
{ _id: "local-tx-001" }

// ledger entries involving Alice
{ $or: [{ source_wallet_id: "alice" }, { destination_wallet_id: "alice" }] }
```

## 8. Stop the local environment

Stop each Go process with `Ctrl+C` in its PowerShell window. Stop MongoDB with `Ctrl+C` in the MongoDB window.

To remove local MongoDB data and start from an empty database later:

```powershell
Remove-Item -Recurse -Force "C:\data\mongodb\db"
New-Item -ItemType Directory -Force -Path "C:\data\mongodb\db" | Out-Null
```

After deleting the data directory, start `mongod` again and run `rs.initiate(...)` again.

## 9. Run Go unit tests

Run all package tests from the repository root:

```powershell
go test ./...
```

Run tests with verbose output:

```powershell
go test ./... -v
```

Run only one package:

```powershell
go test ./pkg/db -v
go test ./cmd/api-gateway -v
```

Run one named test:

```powershell
go test ./cmd/api-gateway -run TestName -v
```

Measure coverage:

```powershell
go test ./... -cover
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out
go tool cover -html=coverage.out -o coverage.html
Start-Process .\coverage.html
```

Race-check tests when the package has tests that can run without external services:

```powershell
go test -race ./...
```

On Windows, the race detector requires CGO and a C compiler such as GCC. If you see `go: -race requires cgo` or `C compiler "gcc" not found`, install a GCC toolchain such as MinGW-w64, add it to `PATH`, and rerun with:

```powershell
$env:CGO_ENABLED = "1"
go test -race ./...
```

The repository includes unit tests in `cmd/api-gateway/main_test.go`, `cmd/wallet-service/main_test.go`, `cmd/ledger-service/main_test.go`, `pkg/db/mongo_test.go`, `proto/ledger/ledger_test.go`, and `proto/wallet/wallet_test.go`. These tests cover gateway JSON/protobuf conversion, service validation, MongoDB retry configuration, and protobuf JSON contracts without requiring MongoDB. MongoDB transaction persistence remains an integration-test concern.

## 10. Recommended unit-test layers

### Gateway handler tests

Use `httptest.NewRequest` and `httptest.NewRecorder` to test HTTP behavior. The gateway currently stores generated gRPC client interfaces, so a fake `walletv1.WalletServiceClient` can return controlled protobuf responses and errors.

Test cases should include:

- Valid wallet JSON becomes the expected protobuf request.
- Invalid JSON returns HTTP 400.
- Wallet gRPC errors return the expected HTTP error.
- Transfer responses are converted to the expected JSON fields.
- Repeated transfer requests preserve the idempotency key.

### Wallet service tests

The current wallet service is tightly coupled to MongoDB through `*mongo.Client`, so transaction behavior is better tested with an integration test against the local replica set. Unit-test any extracted validation or conversion helpers without MongoDB.

Important cases:

- Source and destination wallets cannot be identical.
- Insufficient funds do not produce a successful transfer.
- A missing destination fails the transfer.
- A duplicate idempotency key does not apply the transfer again.
- A successful transfer creates a ledger record and an idempotency record.

### Ledger service tests

Use a MongoDB-backed integration test for persistence and duplicate detection. Test the pure protobuf validation paths as unit tests where possible:

- Empty idempotency key is rejected.
- Non-positive amount is rejected.
- A valid request returns a transaction ID.
- A repeated idempotency key returns the existing transaction ID.

## 11. Unit test versus integration test

A unit test should be fast, isolated, deterministic, and not require MongoDB or running services. An integration test may require the local MongoDB replica set and should be clearly named or placed in a separate test package.

A practical workflow is:

```powershell
# Fast feedback while editing
go test ./... -count=1

# Full local package check
go test ./... -race -count=1

# Manual service and MongoDB integration check
# Start MongoDB and all services, then run the curl commands above.
```

Keep test data isolated with unique wallet IDs and idempotency keys, or reset the local database between runs.

## Local active-standby failover test

The gateway failover and MongoDB replica set solve different problems:

- The gateway switches traffic between the primary wallet on `50051` and standby wallet on `50053`.
- The current MongoDB `rs0` has one member. It supports transactions, but it cannot provide database failover. MongoDB failover requires multiple replica-set members.

Both wallet services use the same MongoDB database, so the standby can read data created through the primary.

Check the initial route:

```powershell
$status = Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:8080/api/v1/cluster/status"
$status | ConvertTo-Json -Depth 5
```

The initial `current_routed_target` should be `PRIMARY`. Toggle to standby (requires Admin token):

```powershell
$AdminToken = (Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/auth/token?sub=admin&role=admin").token
$AdminHeaders = @{ Authorization = "Bearer $AdminToken" }

Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/cluster/failover" -Headers $AdminHeaders | ConvertTo-Json
$status = Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:8080/api/v1/cluster/status"
$status | ConvertTo-Json -Depth 5
```

The route should now be `STANDBY`. Create or transfer a wallet and verify the response contains `routed_gateway: STANDBY` and the standby region.

To simulate primary application failure while traffic is on standby:

```powershell
$AliceToken = (Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/auth/token?sub=alice&role=user").token
$AliceHeaders = @{ Authorization = "Bearer $AliceToken" }

docker stop wallet_primary_active
Invoke-RestMethod -Method Get -Uri "http://127.0.0.1:8080/api/v1/wallets?id=alice" -Headers $AliceHeaders | ConvertTo-Json
docker start wallet_primary_active
```

The balance request should still succeed through standby. Restore the gateway route when finished:

```powershell
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:8080/api/v1/cluster/failover" -Headers $AdminHeaders | ConvertTo-Json
```

Verify the single MongoDB member with:

```powershell
mongosh.exe --host 127.0.0.1:27017 --quiet --eval "rs.status().members.map(m => ({name: m.name, stateStr: m.stateStr, health: m.health}))"
```

Expected result is `PRIMARY` with `health: 1`. This verifies replica-set initialization, not automatic MongoDB failover.
