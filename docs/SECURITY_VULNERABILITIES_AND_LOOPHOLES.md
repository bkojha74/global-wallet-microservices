# Security Vulnerabilities, Architectural Loopholes & Prioritized Remediation Guide

This document catalogs and prioritizes all **15 security vulnerabilities, architectural loopholes, and financial calculation flaws** identified in the **Global Multi-Currency Digital Wallet & Ledger Service** codebase.

Each entry includes the exact file path, vulnerable code block, impact analysis, and definitive defensive remediation to serve as an authoritative engineering reference.

---

## Prioritization Summary (P0 to P3)

The vulnerabilities are ordered strictly by operational and financial risk:
- **P0 - Critical (Immediate Threat)**: Direct loss of funds, total auth bypass, or irreversible audit chain corruption.
- **P1 - High (High Exploitability / Service Disruption)**: Privilege escalation, queue deadlocks, unauthenticated pod access, or network pivoting.
- **P2 - Medium (Concurrency & Resilience Risks)**: Race conditions, client idempotency failures, split-brain risk, or denial-of-service.
- **P3 - Low / Hardening (Cryptographic & Precision Hygiene)**: Hash tree edge cases, currency conversion math skews, and signal deficiencies.

```
┌────────────────────────────────────────────────────────────────────────────────────────┐
│                              PRIORITIZATION MATRIX                                     │
├──────┬──────────────┬──────────────────┬───────────────────────────────────────────────┤
│ Tier │ Priority     │ Impact           │ Issues                                        │
├──────┼──────────────┼──────────────────┼───────────────────────────────────────────────┤
│ P0   │ Critical     │ Immediate Danger │ #1 Auth Bypass Backdoor (/api/v1/auth/token)  │
│      │              │                  │ #2 Transfer Foreign Currency Balance Wipe    │
│      │              │                  │ #3 Non-Atomic Ledger Sequence Hash Breakage   │
├──────┼──────────────┼──────────────────┼───────────────────────────────────────────────┤
│ P1   │ High         │ Major Risk       │ #4 Outbox Relay Head-of-Line Infinite Retry   │
│      │              │                  │ #5 Self-KYC Tier Upgrade to TIER_3            │
│      │              │                  │ #6 Webhook Server-Side Request Forgery (SSRF) │
│      │              │                  │ #7 Unprotected Host-Exposed gRPC Ingress      │
├──────┼──────────────┼──────────────────┼───────────────────────────────────────────────┤
│ P2   │ Medium       │ Moderate Risk    │ #8 Fixed-Rate FX Quote TOCTOU Race Condition  │
│      │              │                  │ #9 Client IP Spoofing Rate Limit Bypass       │
│      │              │                  │ #10 Volatile Deposit/Withdraw Idempotency Key │
│      │              │                  │ #11 Partition Split-Brain Write Fencing Gap   │
│      │              │                  │ #12 Timing User Enumeration & Permanent Lock  │
├──────┼──────────────┼──────────────────┼───────────────────────────────────────────────┤
│ P3   │ Low          │ Hardening / Math │ #13 Merkle Tree Duplicate Leaf Collision Bug  │
│      │              │                  │ #14 Non-Standard Currency Velocity Skew       │
│      │              │                  │ #15 Unpopulated AI Fraud Detection Signals    │
└──────┴──────────────┴──────────────────┴───────────────────────────────────────────────┘
```

---

## Detailed Prioritized Catalog

---

### Priority 0: Critical Severity (P0)

---

#### 1. Public Token Minting Backdoor in API Gateway (`/api/v1/auth/token`)
- **Severity**: **CRITICAL (P0)**
- **Category**: Broken Authentication & Authorization (CWE-306, OWASP A01:2021)
- **Target File**: [cmd/api-gateway/main.go:1634-1665](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/main.go#L1634-L1665)
- **Middleware Whitelist**: [cmd/api-gateway/main.go:2057](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/main.go#L2057)

##### Vulnerability Details
While the platform provides [handleLogin](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/main.go#L1474) which authenticates credentials against the `auth-service`, the development mock route `/api/v1/auth/token` remained registered and explicitly whitelisted in `publicPaths`:

```go
func (g *Gateway) handleAuthToken(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost && r.Method != http.MethodGet {
        http.Error(w, errMethodNotAllowed, http.StatusMethodNotAllowed)
        return
    }

    sub, role, scope := parseAuthTokenParams(r)
    claims, roles, scopes := buildAuthTokenClaims(sub, role, scope)

    secret := os.Getenv("JWT_SECRET")
    if secret == "" {
        secret = defaultJWTSecret
    }

    token, err := auth.GenerateToken(claims, secret)
    ...
}
```

##### Impact
Any anonymous user can forge valid tokens for any identity with administrative privileges:
```bash
curl -X POST http://localhost:8080/api/v1/auth/token \
  -H "Content-Type: application/json" \
  -d '{"subject": "attacker", "roles": ["ADMIN"], "scopes": ["cluster:admin", "ledger:audit", "wallet:transfer"]}'
```
This grants access to drain wallets, view audit logs, trigger failovers, or freeze accounts.

##### Remediation
1. Remove `"/api/v1/auth/token": true` from `publicPaths` in `wrapGatewayMiddleware`.
2. Remove the route `mux.HandleFunc("/api/v1/auth/token", gw.handleAuthToken)` or conditionally compile it only under unit tests (`#if test`).
3. Ensure all token minting exclusively uses `handleLogin` via `authClient.IssueToken`.

---

#### 2. Multi-Currency Transfer Primary Balance Corruption
- **Severity**: **CRITICAL (P0)**
- **Category**: Financial Data Corruption / Inconsistent State (CWE-840)
- **Target File**: [cmd/wallet-service/main.go:649-673](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/main.go#L649-L673) & [cmd/wallet-service/main.go:702-721](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/main.go#L702-L721)

##### Vulnerability Details
When debiting a source wallet in a transfer, `updateSource` unconditionally decrements both `balance` and `balances.<currency>`:

```go
updateSource := bson.M{
    "$inc": bson.M{
        "balance":             -req.Amount.Units,
        "balances." + srcCurr: -req.Amount.Units,
    },
    "$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)},
}
```

If a wallet was created with `USD` as its base currency, and the user has 100 USD in `balance` and 50 EUR in `balances.EUR`, transferring 50 EUR decrements `balances.EUR` by 50, but **also decrements `balance` (USD) by 50**. If the wallet had 0 USD, `balance` becomes negative (-50 USD).

Additionally, when crediting the recipient:
```go
filterDest := bson.M{
    "_id":      req.DestinationWalletId,
    "currency": destCurrency,
}
```
If the destination wallet's base currency is `USD`, but it is receiving `EUR`, `filterDest` matches 0 documents. The transaction aborts with `"Destination wallet not found or currency mismatch"`, preventing multi-currency deposits.

##### Impact
- Irreversible destruction of primary account balances during multi-currency transactions.
- Inability of wallets to receive any currency other than their initial creation currency.

##### Remediation
Apply conditional updates matching [ExchangeCurrency](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/main.go#L1169-L1178):
```go
incSource := bson.M{"balances." + srcCurr: -req.Amount.Units}
if srcCurr == srcWallet.Currency {
    incSource["balance"] = -req.Amount.Units
}
updateSource := bson.M{"$inc": incSource, "$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)}}

incDest := bson.M{"balances." + dstCurr: destUnits}
if dstCurr == dstWallet.Currency {
    incDest["balance"] = destUnits
}
filterDest := bson.M{"_id": req.DestinationWalletId}
updateDest := bson.M{"$inc": incDest, "$set": bson.M{"updated_at": time.Now().UTC().Format(time.RFC3339)}}
```

---

#### 3. Cryptographic Hash Chain Breakage Under Concurrency
- **Severity**: **CRITICAL (P0)**
- **Category**: Cryptographic Integrity & Race Condition (CWE-362, CWE-353)
- **Target File**: [cmd/ledger-service/main.go:118-130](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/ledger-service/main.go#L118-L130) & [cmd/ledger-service/main.go:208-235](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/ledger-service/main.go#L208-L235)

##### Vulnerability Details
Sequence assignment in `RecordTransaction` is non-atomic:
```go
func (s *server) getPreviousSequenceAndHash(ctx context.Context, col *mongo.Collection) (int64, string) {
    var seqNumber int64 = 1
    prevHash := GenesisHash
    var lastDoc LedgerDocument
    findLastOpts := options.FindOne().SetSort(bson.D{bson.E{Key: "sequence_number", Value: -1}})
    if findErr := col.FindOne(ctx, bson.M{"sequence_number": bson.M{"$gt": 0}}, findLastOpts).Decode(&lastDoc); findErr == nil {
        seqNumber = lastDoc.SequenceNumber + 1
        prevHash = lastDoc.EntryHash
    }
    return seqNumber, prevHash
}
```
When two concurrent transactions reach the ledger, both query `lastDoc` simultaneously, receive the same sequence number `N+1` and identical `prevHash`, and attempt insertion.
1. If `sequence_number` has a unique index, one request fails with a duplicate key error and is permanently dropped by `handleInsertError`.
2. If `sequence_number` is not unique, two documents are written with identical sequence numbers, breaking the linear chain.

##### Impact
Executing [VerifyAuditChain](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/ledger-service/double_entry.go#L302) fails with `sequence gap detected` or `hash chain broken`, corrupting the audit trail and failing compliance audits.

##### Remediation
Implement an atomic monotonic sequencer in MongoDB using `FindOneAndUpdate`:
```go
type SequenceDoc struct {
    LastSequence int64  `bson:"last_sequence"`
    LastHash     string `bson:"last_hash"`
}
// Perform atomic update inside MongoDB transaction to serialize ledger block generation:
res := colSequencer.FindOneAndUpdate(
    sessCtx,
    bson.M{"_id": "ledger_head"},
    bson.M{"$inc": bson.M{"last_sequence": 1}},
    options.FindOneAndUpdate().SetReturnDocument(options.Before),
)
```

---

### Priority 1: High Severity (P1)

---

#### 4. Outbox Relay Head-of-Line Blocking & Infinite Retry Loop
- **Severity**: **HIGH (P1)**
- **Category**: Queue Starvation & Unbounded Retry (CWE-834)
- **Target File**: [cmd/wallet-service/ledger_outbox.go:102-114](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/ledger_outbox.go#L102-L114) & [cmd/wallet-service/ledger_outbox.go:520-530](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/ledger_outbox.go#L520-L530)

##### Vulnerability Details
When dispatch to `ledger-service` fails, `MarkFailed` only increments `retries`:
```go
func (m *mongoLedgerTaskStore) MarkFailed(ctx context.Context, id primitive.ObjectID, errMsg string) error {
    _, err := m.col.UpdateOne(ctx,
        bson.M{"_id": id},
        bson.M{
            "$inc": bson.M{"retries": 1},
            "$set": bson.M{"error": errMsg},
        },
    )
    return err
}
```
The task status remains `"pending"`. Because `FindPending` sorts by `created_at: 1`, the failing task remains at the head of the queue. On every iteration, it fails again, triggers exponential backoff (up to 30s), and blocks all subsequent outbox dispatches.

##### Impact
A single poison-pill transaction halts asynchronous ledger synchronization for the entire cluster.

##### Remediation
Add a maximum retry threshold (`MaxRetries = 5`). When exceeded, update status to `"failed"` and dispatch an alert event:
```go
if task.Retries >= MaxOutboxRetries {
    update = bson.M{
        "$set": bson.M{"status": LedgerTaskStatusFailed, "error": errMsg},
    }
}
```

---

#### 5. Privilege Escalation via Self-KYC Tier Upgrade
- **Severity**: **HIGH (P1)**
- **Category**: Improper Authorization / Broken Function Level Authorization (CWE-285, OWASP A01:2021)
- **Target File**: [cmd/api-gateway/main.go:614-619](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/main.go#L614-L619)

##### Vulnerability Details
In `handleUpdateWalletKYC`:
```go
if claims, ok := ClaimsFromContext(r.Context()); ok && claims != nil {
    if err := ValidateWalletOwnership(claims, walletID); err != nil && !claims.HasRole(auth.RoleAdmin) {
        writeAuthError(w, http.StatusForbidden, fmt.Sprintf(msgForbiddenFmt, err))
        return
    }
}
```
[ValidateWalletOwnership](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/middleware.go#L350) returns `nil` when `claims.Subject == walletID`. If the caller owns the wallet, `err == nil`. Therefore, the condition `err != nil && !claims.HasRole(auth.RoleAdmin)` evaluates to `false`, allowing the request.

##### Impact
Any standard user can elevate their account to `TIER_3` ($100,000 single transfer limit, 200 txns/hr), bypassing anti-money laundering (AML) controls.

##### Remediation
Restrict KYC updates to administrators or compliance officers:
```go
if !claims.HasRole(auth.RoleAdmin) && !claims.HasScope("wallet:kyc:admin") {
    writeAuthError(w, http.StatusForbidden, "Forbidden: KYC tier modification requires admin privileges")
    return
}
```

---

#### 6. Server-Side Request Forgery (SSRF) in Webhooks
- **Severity**: **HIGH (P1)**
- **Category**: Server-Side Request Forgery (CWE-918, OWASP A10:2021)
- **Target File**: [cmd/api-gateway/main.go:1212-1260](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/main.go#L1212-L1260) & [pkg/webhook/dispatcher.go:74-76](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/webhook/dispatcher.go#L74-L76)

##### Vulnerability Details
In `handleWebhookTest` and `RegisterSubscription`, URLs are validated only for `http://` or `https://` prefixes:
```go
if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
    http.Error(w, "valid webhook url (http/https) is required", http.StatusBadRequest)
    return
}
```
There is no filter against loopback (`127.0.0.1`), RFC 1918 private subnets (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), or cloud metadata endpoints (`169.254.169.254`).

##### Impact
An attacker can trigger requests to `http://169.254.169.254/latest/meta-data/` to access cloud credentials or scan internal services (`http://mongo1:27018`).

##### Remediation
Implement IP and DNS resolution filtering:
```go
func ValidateWebhookURL(targetURL string) error {
    u, err := url.Parse(targetURL)
    if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
        return errors.New("invalid url scheme")
    }
    ips, err := net.LookupIP(u.Hostname())
    if err != nil {
        return err
    }
    for _, ip := range ips {
        if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
            return errors.New("webhook destination targets restricted private/loopback IP")
        }
    }
    return nil
}
```

---

#### 7. Direct Insecure gRPC Ingress Bypass
- **Severity**: **HIGH (P1)**
- **Category**: Security Misconfiguration / Network Exposure (CWE-284)
- **Target File**: [docker-compose.yml:21-24](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/docker-compose.yml#L21-L24) & [cmd/wallet-service/main.go:1994-1997](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/main.go#L1994-L1997)

##### Vulnerability Details
1. `docker-compose.yml` binds internal gRPC services to host interfaces:
   - `50051:50051` (`wallet-primary`)
   - `50052:50052` (`ledger-service`)
   - `50053:50053` (`wallet-standby`)
2. `wallet-service` and `ledger-service` do not attach authentication interceptors; they assume all traffic has been pre-authenticated by `api-gateway`.
3. `GRPC_TLS_ENABLED` defaults to `false`.

##### Impact
Any internal pod or host network client can dial `wallet-primary:50051` via gRPC and invoke `TransferFunds` or `DepositFunds` without authentication.

##### Remediation
1. Replace `ports:` with `expose:` in `docker-compose.yml` so ports are accessible only within `banking_net`.
2. Add a gRPC authentication interceptor on core microservices to verify internal service tokens or mTLS certificates.

---

### Priority 2: Medium Severity (P2)

---

#### 8. Fixed-Rate FX Quote TOCTOU Race Condition
- **Severity**: **MEDIUM (P2)**
- **Category**: Time-of-Check to Time-of-Use (CWE-367)
- **Target File**: [cmd/fx-service/engine.go:570-600](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/fx-service/engine.go#L570-L600)

##### Vulnerability Details
In `ConvertCurrency`, the quote execution check and status update are non-atomic:
```go
qDoc, err := e.loadQuoteDoc(ctx, quoteID)
if qDoc.Status == QuoteStatusExecuted {
    return nil, fmt.Errorf("quote %s has already been executed", quoteID)
}
...
// Marked executed later without CAS filter
qDoc.Status = QuoteStatusExecuted
e.quotesMem[quoteID] = qDoc
if e.fxDB != nil {
    _, _ = e.fxDB.Collection("fx_quotes").UpdateOne(ctx,
        bson.M{"_id": quoteID},
        bson.M{"$set": bson.M{"status": QuoteStatusExecuted}},
    )
}
```

##### Impact
Two concurrent requests using the same quote ID across separate `fx-service` pods can execute simultaneously, enabling rate arbitrage.

##### Remediation
Use an atomic Compare-And-Swap (CAS) in MongoDB:
```go
res, err := e.fxDB.Collection("fx_quotes").UpdateOne(ctx,
    bson.M{"_id": quoteID, "status": QuoteStatusActive},
    bson.M{"$set": bson.M{"status": QuoteStatusExecuted}},
)
if err != nil || res.ModifiedCount == 0 {
    return nil, fmt.Errorf("quote %s is expired or already executed", quoteID)
}
```

---

#### 9. Client IP Spoofing Rate Limit Bypass
- **Severity**: **MEDIUM (P2)**
- **Category**: Improper Verification of Source (CWE-345)
- **Target File**: [cmd/api-gateway/middleware.go:246-256](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/middleware.go#L246-L256)

##### Vulnerability Details
```go
func extractClientIP(r *http.Request) string {
    if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
        parts := strings.Split(xff, ",")
        return strings.TrimSpace(parts[0])
    }
    ip, _, err := net.SplitHostPort(r.RemoteAddr)
    ...
```
The gateway trusts the client-supplied `X-Forwarded-For` header without verifying that the connection originates from a trusted reverse proxy.

##### Impact
Rotating `X-Forwarded-For` on each request assigns a new bucket, bypassing the token-bucket rate limiter.

##### Remediation
Only inspect `X-Forwarded-For` when `r.RemoteAddr` matches a configured list of trusted proxy CIDRs.

---

#### 10. Volatile Deposit/Withdraw Idempotency Key Generation
- **Severity**: **MEDIUM (P2)**
- **Category**: Missing Idempotency Enforcement (CWE-799)
- **Target File**: [cmd/api-gateway/main.go:808-815](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/main.go#L808-L815) & [cmd/api-gateway/main.go:972-979](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/api-gateway/main.go#L972-L979)

##### Vulnerability Details
In `executeDeposit` and `executeWithdraw`:
```go
idempKey := strings.TrimSpace(req.IdempotencyKey)
if idempKey == "" {
    idempKey = r.Header.Get("Idempotency-Key")
}
if idempKey == "" {
    idempKey = "idemp_dep_" + traceID
}
```
If a client omits the idempotency key and experiences a network timeout, its retry generates a new `traceID`, resulting in a new `idempKey`.

##### Impact
Retried deposits or withdrawals execute multiple times, leading to balance discrepancies.

##### Remediation
Enforce mandatory client idempotency keys on all financial mutations:
```go
if idempKey == "" {
    http.Error(w, "Idempotency-Key header is required for this operation", http.StatusBadRequest)
    return
}
```

---

#### 11. Split-Brain Write Fencing Fallback on Coordinator Partition
- **Severity**: **MEDIUM (P2)**
- **Category**: Distributed Consensus & Fault Tolerance (CWE-662)
- **Target File**: [cmd/wallet-service/main.go:82-91](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/main.go#L82-L91)

##### Vulnerability Details
```go
func (s *server) isWritable(ctx context.Context) bool {
    if s.coordinator != nil {
        if target, err := s.coordinator.GetActiveTarget(ctx); err == nil && target != "" {
            if s.targetRole != "" {
                return target == s.targetRole
            }
        }
    }
    return s.isActive
}
```
If `GetActiveTarget(ctx)` returns an error (e.g., transient network partition), the function falls back to `s.isActive` (which is `true` on `wallet-primary`).

##### Impact
If `wallet-standby` is promoted during an outage, an isolated `wallet-primary` may still accept writes, causing split-brain data divergence.

##### Remediation
Fail-closed on coordinator errors:
```go
if s.coordinator != nil {
    target, err := s.coordinator.GetActiveTarget(ctx)
    if err != nil {
        return false // Reject writes if cluster consensus cannot be determined
    }
    return target == s.targetRole
}
```

---

#### 12. Timing User Enumeration & Permanent Account Lockout
- **Severity**: **MEDIUM (P2)**
- **Category**: Side-Channel Timing & Denial-of-Service (CWE-208, CWE-400)
- **Target File**: [cmd/auth-service/user_store.go:110-142](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/auth-service/user_store.go#L110-L142)

##### Vulnerability Details
1. **Timing Leak**: When a user is not found, `Authenticate` returns immediately (~1ms). When the user exists, `bcrypt.CompareHashAndPassword` executes (~100ms).
2. **Permanent Lockout**: After 5 failed attempts, `status = "LOCKED"` is set without an automatic expiration window.

##### Impact
- Attackers can enumerate registered usernames by measuring response latencies.
- Attackers can permanently lock any target account (e.g., `admin`).

##### Remediation
1. Perform a dummy bcrypt computation on non-existent users:
   ```go
   if err != nil {
       _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
       return nil, errors.New("invalid credentials")
   }
   ```
2. Add a `locked_until` field to automatically expire lockouts after 15 minutes.

---

### Priority 3: Low Severity / Hardening (P3)

---

#### 13. Merkle Tree Duplicate Leaf Collision Bug (CVE-2012-2459)
- **Severity**: **LOW (P3)**
- **Category**: Cryptographic Hash Tree Flaw (CWE-327)
- **Target File**: [pkg/merkle/tree.go:82-91](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/merkle/tree.go#L82-L91)

##### Vulnerability Details
Odd numbers of leaves are handled by duplicating the final node:
```go
if len(currentLevel)%2 != 0 {
    lastNode := currentLevel[len(currentLevel)-1]
    dupNode := &Node{
        Hash: lastNode.Hash,
        Left: lastNode.Left,
        Right: lastNode.Right,
    }
    currentLevel = append(currentLevel, dupNode)
}
```
This produces identical Merkle roots for `[A, B, C]` and `[A, B, C, C]` (Bitcoin CVE-2012-2459). Additionally, leaf and internal node hashes lack prefix domain separation (`0x00` vs `0x01`).

##### Remediation
Promote odd leaf nodes directly to the next tree level without duplication, and use domain separation prefixes per RFC 6962.

---

#### 14. Non-Standard Currency Velocity Skew
- **Severity**: **LOW (P3)**
- **Category**: Numeric & Financial Calculation (CWE-682)
- **Target File**: [cmd/wallet-service/compliance.go:74-93](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/compliance.go#L74-L93)

##### Vulnerability Details
In `normalizeToUSDUnits`, the rate map only contains `EUR`, `GBP`, `JPY`, `CHF`, `CAD`, and `AUD`. Other supported currencies (`INR`, `BHD`, `SGD`) fall through:
```go
rate, ok := ratesToUSD[curr]
if !ok {
    return amount
}
```
For 100,000 INR paise ($12 USD), the function treats the amount as 100,000 USD cents ($1,000 USD), falsely exhausting velocity limits.

##### Remediation
Integrate live conversion rates via `fx-service` or add fallback entries for all catalog currencies.

---

#### 15. Unpopulated AI Fraud Detection Context
- **Severity**: **LOW (P3)**
- **Category**: Incomplete Integration / Defense Degradation (CWE-1007)
- **Target File**: [cmd/wallet-service/main.go:511-520](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/cmd/wallet-service/main.go#L511-L520)

##### Vulnerability Details
The AI prompt in [pkg/ai/fraud_detector.go](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/pkg/ai/fraud_detector.go#L114-L127) relies on `recent_txn_count`, `is_new_wallet_pair`, and `cross_region`.
However, `validateTransferRequest` passes uninitialized signals:
```go
signals := ai.FraudSignals{
    SourceWalletID:      req.SourceWalletId,
    DestinationWalletID: req.DestinationWalletId,
    AmountUnits:         req.Amount.Units,
    Currency:            req.Amount.Currency,
    Region:              s.region,
    Timestamp:           time.Now().UTC(),
    IsRoundNumber:       req.Amount.Units%10000 == 0,
}
```
`RecentTxnCount` is 0, `IsNewWalletPair` is false, and `CrossRegion` is false.

##### Remediation
Populate the missing historical context from `wallet_velocity_records` before invoking the AI fraud model.

---

## Actionable Remediation Implementation Checklist

| ID | Issue | Target Component | Status | Est. Effort |
|---|---|---|---|---|
| **#1** | Disable `/api/v1/auth/token` backdoor | `cmd/api-gateway/main.go` | Planned | 0.5 Day |
| **#2** | Fix transfer multi-currency balance debit & filter | `cmd/wallet-service/main.go` | Planned | 1 Day |
| **#3** | Implement atomic sequence generator for ledger | `cmd/ledger-service/main.go` | Planned | 1.5 Days |
| **#4** | Add DLQ and max retries to outbox relay | `cmd/wallet-service/ledger_outbox.go` | Planned | 1 Day |
| **#5** | Require Admin role for KYC tier updates | `cmd/api-gateway/main.go` | Planned | 0.5 Day |
| **#6** | Add private IP/DNS validation for webhooks | `pkg/webhook/dispatcher.go` | Planned | 1 Day |
| **#7** | Restrict internal gRPC container ports & add auth | `docker-compose.yml`, `cmd/wallet-service` | Planned | 1 Day |
| **#8** | Implement atomic CAS for FX quote execution | `cmd/fx-service/engine.go` | Planned | 0.5 Day |
| **#9** | Validate trusted proxies for `X-Forwarded-For` | `cmd/api-gateway/middleware.go` | Planned | 0.5 Day |
| **#10** | Require client idempotency keys on mutations | `cmd/api-gateway/main.go` | Planned | 0.5 Day |
| **#11** | Fail-closed on coordinator network errors | `cmd/wallet-service/main.go` | Planned | 0.5 Day |
| **#12** | Normalize auth timing & add temporary lock expiration | `cmd/auth-service/user_store.go` | Planned | 0.5 Day |
| **#13** | Fix Merkle duplicate odd leaves & add domain prefix | `pkg/merkle/tree.go` | Planned | 0.5 Day |
| **#14** | Expand currency velocity rate conversion catalog | `cmd/wallet-service/compliance.go` | Planned | 0.5 Day |
| **#15** | Populate historical velocity signals for AI fraud checks | `cmd/wallet-service/main.go` | Planned | 0.5 Day |
