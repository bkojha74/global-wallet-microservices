# Global Multi-Currency Digital Wallet & Ledger Service

A production-grade, microservice-based digital wallet and double-entry ledger platform implemented in Go.

> New to this project or gRPC? Start with the [Beginner Guide](docs/BEGINNER_GUIDE.md) for the architecture, request flow, transfer example, and local setup.

For a native Windows setup without Docker, including MongoDB replica-set deployment, MongoDB Compass, service startup, API checks, and Go unit testing, see the [Local Development Guide](docs/LOCAL_DEVELOPMENT.md).

To run only MongoDB in Docker and start the ledger, wallet, and gateway services with `go run`, follow the [Docker MongoDB with native Go services](docs/LOCAL_DEVELOPMENT.md#docker-mongodb-with-native-go-services) procedure.

To call `WalletService.TransferFunds` directly with BloomRPC, see the [BloomRPC gRPC Testing Guide](docs/BLOOMRPC_GUIDE.md).

This repository includes a GitHub Actions workflow for formatting, unit tests, and race tests. See [SECURITY.md](SECURITY.md) for the boundary between this demonstration setup and a production deployment.

## Architecture Highlights
- **Inter-Service Communication**: Binary gRPC (Protobuf v3) between API Gateway, Wallet Service, and Ledger Service.
- **Persistence & Atomicity**: MongoDB Multi-Document Transactions with `writeconcern.Majority()` and `readconcern.Snapshot()`.
- **Multi-Region Active-Standby**:
  - `wallet-primary`: Active Primary instance (`us-east-1`).
  - `wallet-standby`: Standby Hot Disaster Recovery replica (`eu-west-1`).
  - `api-gateway`: Client-facing REST router with instant failover capability (`/api/v1/cluster/failover`).
- **Containerization & Orchestration**:
  - `docker-compose.mongodb.yml` for a reusable MongoDB Replica Set (`rs0`).
  - `docker-compose.yml` for the wallet application services only.
  - Complete `k8s/` manifests for Kubernetes deployment.

---

## Quickstart (Docker Compose)

### 1. Start the shared MongoDB stack

MongoDB is intentionally deployed separately so other projects can use the same database container and Docker network:

```bash
docker network create wallet_shared_net 2>/dev/null || true
docker volume create global-wallet-microservices_mongo_data >/dev/null 2>&1 || true
docker compose -f docker-compose.mongodb.yml up -d
```

The MongoDB stack creates:

- Container `wallet_mongodb` using `mongo:7.0`.
- Replica set `rs0`, required by MongoDB transactions.
- Persistent volume `wallet_mongo_data`.
- Shared Docker network `wallet_shared_net`.

Verify that MongoDB is healthy before starting the application:

```bash
docker compose -f docker-compose.mongodb.yml ps
docker compose -f docker-compose.mongodb.yml logs mongo-init
```

### 2. Start the wallet application

```bash
docker compose -f docker-compose.yml up --build -d
```

The application Compose file connects to the existing `wallet_shared_net`; it does not create or remove MongoDB containers or data.

### 3. Run the End-to-End Automated Test
```bash
./scripts/test-e2e.sh
```
Or via Makefile:
```bash
make test
```

### 4. Interactive Manual Verification with `curl`

* **Create Alice's Wallet ($1,000 USD):**
  ```bash
  curl -X POST http://localhost:8080/api/v1/wallets \
    -H "Content-Type: application/json" \
    -d '{"wallet_id":"alice","currency":"USD","initial_balance":1000}'
  ```

* **Create Bob's Wallet ($500 USD):**
  ```bash
  curl -X POST http://localhost:8080/api/v1/wallets \
    -H "Content-Type: application/json" \
    -d '{"wallet_id":"bob","currency":"USD","initial_balance":500}'
  ```

* **Execute Atomic Transfer ($250 from Alice to Bob):**
  ```bash
  curl -X POST http://localhost:8080/api/v1/transfers \
    -H "Content-Type: application/json" \
    -d '{
      "idempotency_key": "tx-001",
      "source_wallet_id": "alice",
      "destination_wallet_id": "bob",
      "amount": 250,
      "currency": "USD"
    }'
  ```

* **Trigger Regional Failover to Standby:**
  ```bash
  curl -X POST http://localhost:8080/api/v1/cluster/failover
  ```

* **Query Immutable Audit Ledger:**
  ```bash
  curl "http://localhost:8080/api/v1/ledger?wallet_id=alice"
  ```

### Stop the stacks

Stop application services without touching MongoDB data:

```bash
docker compose -f docker-compose.yml down
```

Stop MongoDB separately:

```bash
docker compose -f docker-compose.mongodb.yml down
```

The MongoDB volume is retained by default. Remove it only when intentionally resetting data:

```bash
docker volume rm global-wallet-microservices_mongo_data
```

The volume is external, so `docker compose down -v` does not remove it.

### Reuse MongoDB from another project

Attach another Compose service to the existing network and use the MongoDB service name:

```yaml
services:
  another-service:
    image: your-image
    environment:
      MONGO_URI: mongodb://mongodb:27017/?replicaSet=rs0&directConnection=true
    networks:
      - wallet_shared_net

networks:
  wallet_shared_net:
    external: true
    name: wallet_shared_net
```

Start the MongoDB stack first so the external network exists. In production, use authentication, TLS, restricted network access, separate databases/users per project, backups, and a managed or multi-node MongoDB deployment instead of sharing an unauthenticated development database.

---

## Kubernetes Deployment (Minikube / Kind)

```bash
# Build the Docker images locally:
docker build --target wallet-service -t wallet-system/wallet-service:latest .
docker build --target ledger-service -t wallet-system/ledger-service:latest .
docker build --target api-gateway -t wallet-system/api-gateway:latest .

# Apply manifests:
kubectl apply -f k8s/
```
