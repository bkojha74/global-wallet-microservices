# Global Wallet Microservices — Release v1.6.0
## Pillar 5: Developer Experience, Tooling & Auditing

**Release Version**: `v1.6.0`  
**Previous Version**: `v1.5.0`  
**Release Date**: October 2026  
**Status**: General Availability (GA)  

---

## 1. Executive Summary

**Release v1.6.0** delivers **Pillar 5 (Developer Experience, Tooling & Auditing)** of the Enterprise Next-Level Improvement Roadmap for the Global Multi-Currency Digital Wallet and Ledger Microservices Platform.

Following the core banking multi-currency expansion in v1.5.0, v1.6.0 equips operators, Site Reliability Engineers (SREs), partner developers, and regulatory auditors with enterprise-grade developer tooling, zero-dependency API exploration, diagnostic CLI capabilities, and cryptographic mathematical attestation.

### Key Milestones Delivered in v1.6.0:
1. **DEV-01: OpenAPI 3.0 & Embedded Swagger UI** (`v1.6.0-rc.1`)
   - Interactive, zero-dependency Swagger UI embedded directly inside the `api-gateway` binary via Go 1.16+ `//go:embed`.
   - Comprehensive OpenAPI 3.0.3 specification documenting all 24+ platform REST endpoints across Auth, Wallets, FX, Limits, External Settlement Rails, Outbound Webhooks, Merkle Audit, and Cluster Operations.
   - Live testing console accessible at `/swagger/` and `/docs`, with raw machine-readable JSON at `/api-docs/openapi.json`.

2. **DEV-02: Unified Operator CLI Utility (`walletctl`)** (`v1.6.0-rc.2`)
   - Standalone, cross-platform CLI tool built in Go (`cmd/walletctl`) for operators and developers.
   - Subcommands for cluster health diagnostics (`walletctl health`), HMAC/Keycloak token generation (`walletctl token`), zero-downtime failover drills (`walletctl failover`), cryptographic ledger chain verification (`walletctl ledger verify`), and partner webhook probing (`walletctl webhook ping`).
   - Remediation of standard library vulnerability `GO-2026-6809` via Go toolchain bump to `1.26.10`.

3. **DEV-03: Merkle Root Ledger Checkpoints & Mathematical Proofs** (`v1.6.0-rc.3`)
   - Cryptographic binary Merkle Tree package (`pkg/merkle`) utilizing SHA-256 for batch verification over append-only ledger journals.
   - Background checkpoint worker in `ledger-service` periodically capturing immutable checkpoints in MongoDB (`ledger_merkle_roots`).
   - $O(\log N)$ logarithmic inclusion proof generator allowing external auditors to verify individual transaction inclusion against an epoch root without scanning entire historical ledger journals.
   - Public auditor bypass endpoints at `/api/v1/ledger/merkle/checkpoints`, `/api/v1/ledger/merkle/latest`, and `/api/v1/ledger/merkle/proof`, integrated with `walletctl ledger merkle verify`.

---

## 2. Detailed Deliverables & Architectural Changes

### 2.1. DEV-01: OpenAPI 3.0 & Embedded Swagger UI
- **Zero External Dependencies**: Embedded UI assets (`swagger.go`) serve HTML, CSS, and JS directly from binary memory with automatic MIME type resolution and cache control.
- **Security & Authorization**: Integrated OpenAPI 3.0 `BearerAuth` JWT authorization modal for executing authenticated requests against secured endpoints.
- **API Catalog Coverage**:
  - `POST /api/v1/auth/token`, `POST /api/v1/auth/refresh`, `POST /api/v1/auth/logout`
  - `POST /api/v1/wallets`, `GET /api/v1/wallets/{id}/balance`, `POST /api/v1/wallets/transfer`, `POST /api/v1/wallets/exchange`
  - `POST /api/v1/wallets/kyc`, `GET /api/v1/wallets/{id}/limits`
  - `POST /api/v1/wallets/deposit`, `POST /api/v1/wallets/withdraw`
  - `POST /api/v1/webhooks/subscriptions`, `GET /api/v1/webhooks/subscriptions`, `POST /api/v1/webhooks/test`
  - `GET /api/v1/ledger/merkle/checkpoints`, `GET /api/v1/ledger/merkle/latest`, `GET /api/v1/ledger/merkle/proof`
  - `GET /api/v1/cluster/status`, `POST /api/v1/cluster/failover`, `/healthz`, `/readyz`

### 2.2. DEV-02: Unified CLI Utility (`walletctl`)
- **`walletctl health`**:
  - Probes API Gateway, Wallet Primary, Wallet Standby, FX Engine, Ledger Service, and Logging Service.
  - Outputs colorized ANSI status tables with round-trip latencies, component states, and active routing targets.
- **`walletctl token`**:
  - Generates signed JWT bearer tokens instantly with customizable roles (`USER`, `ADMIN`), user IDs, and scopes (`wallet:read`, `wallet:write`, `wallet:transfer`, `cluster:admin`).
- **`walletctl failover`**:
  - Inspects active gateway traffic routing and triggers seamless switches between Primary and Standby wallet instances with safety confirmation flags (`--yes`).
- **`walletctl ledger verify`**:
  - Validates cryptographic SHA-256 hash chains across sequential journal entries to detect database tampering.
- **`walletctl webhook ping`**:
  - Sends signed HMAC-SHA256 ping events (`X-Wallet-Signature`, `X-Wallet-Timestamp`) to partner webhook URLs to verify connectivity and signature validation.

### 2.3. DEV-03: Merkle Root Ledger Checkpoints
- **Binary Merkle Tree Engine (`pkg/merkle/tree.go`)**:
  - Complete, balanced binary Merkle tree with deterministic leaf ordering and SHA-256 pair hashing.
  - Proof generator producing compact inclusion proof steps (`left` or `right` sibling hashes).
  - Standalone verification function `VerifyProof(root, target, proof)` with zero external dependencies.
- **Ledger Service Checkpoint Manager (`cmd/ledger-service/merkle.go`)**:
  - Manages periodic and on-demand checkpoint creation over transaction sequence windows.
  - Persists `{ checkpoint_id, start_sequence, end_sequence, merkle_root, transaction_count, created_at }` in MongoDB collection `ledger_merkle_roots`.
  - Background worker running periodically (configurable interval) with graceful context termination.
- **Auditor Verification via CLI**:
  - `walletctl ledger merkle verify --checkpoint-id <id>` downloads checkpoint and inclusion proof, recomputes root hash from leaf hash up the audit path, and verifies match.

---

## 3. Security, Quality & Performance Verification

All quality gates passed with zero warnings and strict compliance:
- **Unit & Concurrency Tests**: `go test -race ./...` — 100% passed across all microservices and packages with **0 data races detected**.
- **Static Analysis & Linting**: `golangci-lint` and `go vet ./...` — **0 issues**.
- **Security Scans**:
  - `govulncheck ./...` — Resolved `GO-2026-6809` by updating Go version in `go.mod` to `1.26.10`.
  - `gosec -quiet ./...` — **0 vulnerabilities**.
- **Container Build & Publishing**:
  - All 6 microservice images built and pushed to Docker Hub under `bkojha74/*:latest` and tagged release versions.

---

## 4. Upgrading from v1.5.0 to v1.6.0

### Docker Compose
```bash
docker compose pull
docker compose up -d
```

### Kubernetes (via Helm or Manifests)
```bash
kubectl rollout restart deployment/api-gateway -n banking-system
kubectl rollout restart deployment/ledger-service -n banking-system
kubectl rollout restart deployment/wallet-primary -n banking-system
kubectl rollout restart deployment/wallet-standby -n banking-system
```

### Building the `walletctl` CLI
```bash
go build -o walletctl.exe ./cmd/walletctl
./walletctl.exe --help
```

---

## 5. Contributors
- **Author & Maintainer**: Bipin Kumar Ojha ([@bkojha74](https://github.com/bkojha74))
