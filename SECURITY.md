# Security Policy

## Scope

This repository is a demonstration and learning project. The default Docker Compose and local Go instructions are development configurations, not production deployment configurations.

## Implemented Security & Resilience Controls (Phases 1, 2 & 3)

The platform maintains the following zero-trust security and resilience controls:
- **Authentication (AuthN)**: Cryptographic HMAC-SHA256 JWT validation at the API Gateway with claims verification (`sub`, `roles`, `scopes`, `exp`).
- **Authorization & IDOR Protection (AuthZ)**: Strict subject verification (`sub == wallet_id`) preventing cross-wallet access; administrative RBAC for cluster failovers.
- **Inter-Service Mutual TLS (mTLS)**: Bidirectional TLS certificate validation (`tls.RequireAndVerifyClientCert`) for gRPC inter-service communication across `api-gateway`, `wallet-service`, and `ledger-service`.
- **Gateway Defense-in-Depth**: Token-bucket rate limiting (60 rps, 100 burst), 1MB payload limits (`http.MaxBytesReader`), standard security headers (HSTS, CSP, X-Frame-Options: DENY, X-Content-Type-Options: nosniff), and Slowloris mitigation (`ReadHeaderTimeout: 3s`).
- **Data & Financial Integrity**: Strict ISO-4217 currency scale enforcement, atomic duplicate wallet creation protection (409 Conflict), unique idempotency indexes, and a 30-day TTL lifecycle.
- **Standby Write Fencing & Consensus (GAP-HA-01 & GAP-HA-02)**: Distributed `FailoverCoordinator` synchronization in MongoDB, with strict write fencing rejecting balance alterations on standby nodes (`codes.FailedPrecondition`).
- **Graceful Process Lifecycle (GAP-REL-01)**: Interception of `SIGTERM`/`SIGINT` with HTTP request draining (15s) and `grpcServer.GracefulStop()` ensuring in-flight financial transactions commit cleanly before shutdown.

## Remaining Production Boundaries (Phase 4)

Before final cloud-native production deployment:
- Enable MongoDB enterprise authentication (`SCRAM-SHA-256`) and database TLS encryption.
- Inject secrets via Kubernetes Secrets, HashiCorp Vault, or AWS Secrets Manager (template available in `.env.example`).
- Deploy a multi-node MongoDB replica set across independent Availability Zones (replacing the single-node local replica set).
- Add non-root container users, resource limits, Kubernetes NetworkPolicies, and Ingress with TLS cert-manager.


## Reporting a vulnerability

Do not open a public issue for a suspected credential or security vulnerability. Contact the repository owner privately through the GitHub security contact configured for the repository.

If a secret is ever committed accidentally, revoke or rotate it immediately. Removing it from the latest commit is not sufficient because Git history may still contain it.
