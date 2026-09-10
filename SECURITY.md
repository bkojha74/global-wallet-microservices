# Security Policy

## Scope

This repository is a demonstration and learning project. The default Docker Compose and local Go instructions are development configurations, not production deployment configurations.

## Production boundaries

Before deploying any part of this project to production:

- Enable MongoDB authentication and TLS.
- Store MongoDB credentials and other secrets in a secret manager or orchestrator secret, never in Git or plain Compose files.
- Use a managed or multi-member MongoDB replica set with backups, monitoring, restore tests, and access controls.
- Replace insecure gRPC transport with authenticated TLS or a service-mesh security layer.
- Restrict MongoDB network access to the services that require it; do not expose port `27017` publicly.
- Replace hard-coded image tags such as `latest` with immutable, scanned image digests.
- Add container users, resource limits, health/readiness probes, network policies, and vulnerability scanning.
- Add authentication and authorization at the API gateway and service boundaries.
- Validate request size, wallet identifiers, currencies, amounts, and idempotency behavior according to the business requirements.
- Review transaction and ledger consistency under retries, timeouts, concurrent requests, and service failure.
- Centralize structured logs and metrics, and redact credentials, tokens, and sensitive financial data.
- Do not use the local one-member MongoDB setup as a high-availability or disaster-recovery configuration.

## Reporting a vulnerability

Do not open a public issue for a suspected credential or security vulnerability. Contact the repository owner privately through the GitHub security contact configured for the repository.

If a secret is ever committed accidentally, revoke or rotate it immediately. Removing it from the latest commit is not sufficient because Git history may still contain it.
