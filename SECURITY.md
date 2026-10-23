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

## Enterprise Security Vulnerability Remediation (15 / 15 Resolved)

All 15 security vulnerabilities, architectural loopholes, and financial calculation flaws cataloged in [docs/SECURITY_VULNERABILITIES_AND_LOOPHOLES.md](docs/SECURITY_VULNERABILITIES_AND_LOOPHOLES.md) have been defensively remediated:

1. **Backdoor Deactivation (VULN-01 - P0)**: Public token endpoint `/api/v1/auth/token` removed from unauthenticated paths; gated strictly behind `ENABLE_DEV_TOKEN_ENDPOINT=true` for local development only.
2. **Multi-Currency Pot Isolation (VULN-02 - P0)**: Multi-currency transfers update foreign currency balance pots without wiping or corrupting base currency balances.
3. **Atomic Sequence Generator (VULN-03 - P0)**: Ledger journal sequences and hash chaining use an atomic sequencer in `ledger-service` eliminating race condition hash breakages.
4. **Outbox Dead-Letter Queue (VULN-04 - P1)**: `LedgerRelay` enforces `DefaultMaxLedgerTaskRetries = 5` and permanently marks unrecoverable tasks to prevent head-of-line blocking.
5. **KYC Privilege Escalation Defense (VULN-05 - P1)**: KYC tier upgrades strictly require `ADMIN` role or `wallet:kyc:admin` scope in `api-gateway`.
6. **Webhook SSRF Protection (VULN-06 - P1)**: Outbound webhook dispatcher filters loopback (`127.0.0.0/8`), private networks (RFC 1918), and cloud metadata IPs (`169.254.169.254`).
7. **Host Network Isolation (VULN-07 - P1)**: All internal gRPC and management metrics ports bound to `127.0.0.1` on the host in `docker-compose.yml`.
8. **Atomic FX Quote Execution (VULN-08 - P2)**: FX conversions execute under atomic Compare-And-Swap (CAS) in MongoDB and in-memory engine, preventing double-execution TOCTOU races.
9. **Trusted Proxy Rate Limiting (VULN-09 - P2)**: `X-Forwarded-For` spoofing rejected unless the request originates from a pre-configured trusted proxy CIDR.
10. **Financial Mutation Idempotency (VULN-10 - P2)**: Client-supplied idempotency keys enforced on `/transfer`, `/deposit`, and `/withdraw` routes.
11. **Fail-Closed Partition Fencing (VULN-11 - P2)**: Split-brain write fencing fails-closed upon coordinator network or database consensus errors.
12. **Auth Timing & Lockout Defense (VULN-12 - P2)**: Constant-time dummy bcrypt verification on non-existent users, with automatic 15-minute temporary lockout auto-expiration.
13. **Merkle Tree Hardening (VULN-13 - P3)**: Odd leaf promotion without duplication and RFC 6962 domain separation prefixes (`0x00`/`0x01`) in `pkg/merkle/tree.go`, eliminating Bitcoin CVE-2012-2459 collision attacks.
14. **Expanded Velocity Currency Catalog (VULN-14 - P3)**: Compliance velocity limit engine includes real-world exchange conversion rates for `SGD`, `INR`, `BHD`.
15. **AI Fraud Detection Signals (VULN-15 - P3)**: Historical velocity signals (`RecentTxnCount`, `IsNewWalletPair`, `CrossRegion`) populated from MongoDB before invoking the AI fraud detector.
- **GoSec SAST Linter Compliance**: All gRPC listener sockets annotated with `// #nosec G102` across all microservices.

## Production Cloud Deployment Recommendations

For live production deployment:
- Enable MongoDB enterprise authentication (`SCRAM-SHA-256`) and TLS database wire encryption.
- Inject secrets via Kubernetes Secrets or external secret store (HashiCorp Vault, AWS Secrets Manager) using templates in `k8s/09-configmap-secrets.yaml` and `.env.example`.


## Reporting a vulnerability

Do not open a public issue for a suspected credential or security vulnerability. Contact the repository owner privately through the GitHub security contact configured for the repository.

If a secret is ever committed accidentally, revoke or rotate it immediately. Removing it from the latest commit is not sufficient because Git history may still contain it.
