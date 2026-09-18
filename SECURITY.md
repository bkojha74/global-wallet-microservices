# Security Policy

## Scope

This repository is a demonstration and learning project. The default Docker Compose and local Go instructions are development configurations, not production deployment configurations.

## Implemented Security & Resilience Controls (Phases 1, 2, 3 & 4)

The platform maintains the following zero-trust security, compliance, and resilience controls:
- **Authentication (AuthN)**: Cryptographic HMAC-SHA256 JWT validation at the API Gateway with claims verification (`sub`, `roles`, `scopes`, `exp`).
- **Authorization & IDOR Protection (AuthZ)**: Strict subject verification (`sub == wallet_id`) preventing cross-wallet access; administrative RBAC for cluster failovers.
- **Inter-Service Mutual TLS (mTLS)**: Bidirectional TLS certificate validation (`tls.RequireAndVerifyClientCert`) for gRPC inter-service communication across `api-gateway`, `wallet-service`, and `ledger-service`.
- **Gateway Defense-in-Depth**: Token-bucket rate limiting (60 rps, 100 burst), 1MB payload limits (`http.MaxBytesReader`), standard security headers (HSTS, CSP, X-Frame-Options: DENY, X-Content-Type-Options: nosniff), and Slowloris mitigation (`ReadHeaderTimeout: 3s`).
- **Data & Financial Integrity**: Strict ISO-4217 currency scale enforcement, atomic duplicate wallet creation protection (409 Conflict), unique idempotency indexes, and a 30-day TTL lifecycle.
- **True Double-Entry & Audit Immutability (GAP-FIN-02)**: Balanced journal postings ($\sum \text{Debits} == \sum \text{Credits}$) and SHA-256 cryptographic audit chaining back to `GenesisHash`, with online tamper verification via `/audit/verify`.
- **Account Operational Fencing (GAP-FIN-05)**: Explicit operational states (`ACTIVE`, `FROZEN`, `CLOSED`) with atomic transaction fencing blocking movements to/from frozen accounts, and administrative management endpoint `/admin/wallet/status`.
- **Container Hardening (GAP-OPS-01 & GAP-OPS-02)**: Multi-stage Docker build running under unprivileged `appuser:appgroup` (UID 10001, GID 10001) with deterministic dependency caching (`COPY go.mod go.sum` -> `RUN go mod download`).
- **Production Kubernetes Architecture (GAP-OPS-03 & GAP-HA-03)**: Multi-node HA MongoDB StatefulSet (`rs0`), unprivileged Pod securityContexts (`runAsNonRoot: true`), CPU/memory requests and limits, liveness/readiness probes, HPA, PDB, and TLS-terminated Ingress routing.
- **Standby Write Fencing & Consensus (GAP-HA-01 & GAP-HA-02)**: Distributed `FailoverCoordinator` synchronization in MongoDB, with strict write fencing rejecting balance alterations on standby nodes (`codes.FailedPrecondition`).
- **Graceful Process Lifecycle (GAP-REL-01)**: Interception of `SIGTERM`/`SIGINT` with HTTP request draining (15s) and `grpcServer.GracefulStop()` ensuring in-flight financial transactions commit cleanly before shutdown.

## Production Cloud Deployment Recommendations

For live production deployment:
- Enable MongoDB enterprise authentication (`SCRAM-SHA-256`) and TLS database wire encryption.
- Inject secrets via Kubernetes Secrets or external secret store (HashiCorp Vault, AWS Secrets Manager) using templates in `k8s/09-configmap-secrets.yaml` and `.env.example`.


## Reporting a vulnerability

Do not open a public issue for a suspected credential or security vulnerability. Contact the repository owner privately through the GitHub security contact configured for the repository.

If a secret is ever committed accidentally, revoke or rotate it immediately. Removing it from the latest commit is not sufficient because Git history may still contain it.
