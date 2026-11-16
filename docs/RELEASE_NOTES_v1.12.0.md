# Global Wallet Microservices — Release v1.12.0
## Sprint 5: Institutional Regulatory Compliance, BaaS Virtual IBANs & Open Banking PSD2

**Release Version**: `v1.12.0`  
**Previous Version**: `v1.11.0`  
**Release Date**: November 2026  
**Status**: General Availability (GA)  

---

## 1. Executive Summary

**Release v1.12.0** delivers **Sprint 5** of the Global Banking Standards & Core Banking Platform roadmap specified in [docs/SPRINT_5_SPECIFICATION_AND_DELIVERABLES.md](SPRINT_5_SPECIFICATION_AND_DELIVERABLES.md).

Following the delivery of **v1.11.0** (ISO 20022 Clearing Rails & Multi-Tier GAAP/IFRS Chart of Accounts), **v1.12.0** equips the platform with institutional regulatory compliance controls, multi-tenant Banking-as-a-Service (BaaS) capabilities, and PSD2 Open Banking interfaces:

1. **FATF Recommendation 16 (Travel Rule) Compliance (`REQ-COMP-01`)**: Real-time inspection and enforcement of mandatory originator and beneficiary legal entity identification on high-value and cross-border transactions ($\ge \$1,000$). Non-compliant transfers are rejected with `400 Bad Request` (`travel_rule_violation: true`).
2. **Real-Time Sanctions Screening & FinCEN SAR Generator (`REQ-COMP-02`)**: Sub-millisecond in-memory screening engine pre-loaded with OFAC SDN, UN, and high-risk sanctions profiles. Employs phonetic normalization and Jaro-Winkler string similarity matching to intercept blocked actors (`403 Forbidden`, `sanctions_blocked: true`) and produces structured FinCEN Suspicious Activity Report (SAR) XML/JSON records.
3. **Banking-as-a-Service (BaaS) Virtual IBAN Engine (`REQ-BAAS-01`)**: Dynamic issuance of ISO 13616 compliant virtual IBANs for `GB` (Barclays 200000), `DE` (Commerzbank 37040044), and `FR` (3000200550) linked directly to wallet balance pots. Incoming ISO 20022 `pacs.008` clearing messages specifying virtual IBANs automatically resolve directly to internal ledger accounts.
4. **Open Banking PSD2 / UK Open Banking Consent Framework (`REQ-OPEN-01`)**: Standardized consent lifecycle management (`AWAITING_AUTHORISATION`, `AUTHORISED`, `REVOKED`, `EXPIRED`) with granular scope validation (`ReadAccountsDetail`, `ReadBalances`, `CreatePaymentInitiation`). Provides dedicated AISP (`/open-banking/accounts`, `/open-banking/balances`) and PISP (`/open-banking/payments`) endpoints.
5. **Automated E2E Test Suite & OpenAPI 3.1 Parity (`REQ-TEST-05`)**: Comprehensive Bruno test collections (`bruno/12-Open-Banking/`, `bruno/07-Compliance-KYC/`, `bruno/06-Transfers/`), automated test harness runner [`scripts/verify_bruno_suite.ps1`](../scripts/verify_bruno_suite.ps1), and complete OpenAPI 3.1 schema definitions in [`cmd/api-gateway/openapi.json`](../cmd/api-gateway/openapi.json).

---

## 2. Key Milestones Delivered in v1.12.0

### 2.1. REQ-COMP-01: FATF Recommendation 16 (Travel Rule) Compliance
- **Package**: `pkg/compliance`
- **Features**:
  - Implements `TravelRuleParty` (Legal Name, Account Number, Physical Address, National ID / Tax Number, Country, BIC FI).
  - Implements `ValidateTravelRule(amount float64, currency string, payload *TravelRulePayload) error`.
  - Enforces mandatory originator and beneficiary identification on transfers exceeding \$1,000 USD (or equivalent EUR/GBP thresholds).
  - Integrated into API Gateway transfer middleware.
- **Verification**: `pkg/compliance/compliance_test.go`, `cmd/api-gateway/main_test.go:TestHandleTransfer_TravelRuleEnforcement`.

### 2.2. REQ-COMP-02: Real-Time Sanctions Screening & FinCEN SAR Generator
- **Package**: `pkg/compliance`
- **Features**:
  - `SanctionsEngine` pre-loaded with OFAC SDN, UN Counter-Terrorism, and high-risk transnational crime designations.
  - Sub-millisecond in-memory string matching with phonetic preprocessing and Jaro similarity scoring (threshold $\ge 0.85$).
  - `GenerateSAR`: Produces audit-ready FinCEN Suspicious Activity Report (SAR) XML/JSON records containing suspicious activity classification, transaction legs, and identifying narrative.
  - Integrated into both internal transfer flows and incoming ISO 20022 clearing pipelines.
- **Verification**: `pkg/compliance/compliance_test.go`, `cmd/api-gateway/main_test.go:TestHandleTransfer_SanctionsBlocking`.

### 2.3. REQ-BAAS-01: Virtual IBAN (vIBAN) Management Engine
- **Package**: `pkg/banking`, `cmd/api-gateway`
- **Endpoints**:
  - `POST /api/v1/wallets/:id/vibans` — Issue compliant ISO 13616 vIBAN mapped to wallet.
  - `GET /api/v1/wallets/:id/vibans` — List active vIBANs for a wallet.
  - `GET /api/v1/wallets/vibans?iban=...` — Resolve vIBAN to internal wallet.
  - `DELETE /api/v1/wallets/:id/vibans?iban=...` — Revoke virtual IBAN.
- **Features**:
  - ISO 7064 Mod 97-10 check-digit verified vIBANs for GB, DE, and FR.
  - Inbound ISO 20022 `pacs.008` messages specifying a vIBAN in `txInf.CdtrAcct.Id.IBAN` automatically resolve directly to the customer's wallet account.
- **Verification**: `pkg/banking/viban_test.go`, `cmd/api-gateway/main_test.go:TestHandleVirtualIBANs`.

### 2.4. REQ-OPEN-01: Open Banking PSD2 / UK Open Banking Consent Framework
- **Package**: `pkg/openbanking`, `cmd/api-gateway`
- **Endpoints**:
  - `POST /api/v1/open-banking/consents` — TPP creates consent (status `AWAITING_AUTHORISATION`).
  - `POST /api/v1/open-banking/consents/authorise` — Customer grants SCA authorisation (`AUTHORISED`).
  - `GET /api/v1/open-banking/consents?consent_id=...` — Query consent status and metadata.
  - `DELETE /api/v1/open-banking/consents?consent_id=...` — Revoke consent (`REVOKED`).
  - `GET /api/v1/open-banking/accounts` — AISP: Read authorized customer account list.
  - `GET /api/v1/open-banking/balances` — AISP: Read authorized account balance breakdown.
  - `POST /api/v1/open-banking/payments` — PISP: Execute domestic payment initiation.
- **Features**:
  - Granular permission enforcement (`ReadAccountsDetail`, `ReadBalances`, `CreatePaymentInitiation`).
  - Built-in expiry handling (default 90-day PSD2 validity) and revocation rejection.
- **Verification**: `pkg/openbanking/consent_test.go`, `cmd/api-gateway/main_test.go:TestOpenBankingConsentAndAISP_PISP`.

---

## 3. Pre-Push CI Gate Verification

| Step | Gate Check | Tool / Command | Result |
| :--- | :--- | :--- | :--- |
| **1** | Go Formatting | `gofmt -l .` (`go1.26.11`) | **PASS** (0 unformatted files) |
| **2** | Static Linting | `staticcheck ./pkg/... ./cmd/...` | **PASS** (0 warnings or errors) |
| **3** | Go Vet | `go vet ./cmd/... ./pkg/...` | **PASS** (0 issues) |
| **4** | Security SAST | `gosec -severity medium ./cmd/... ./pkg/...` | **PASS** (70 files, 24,080 LOC scanned, 0 issues) |
| **5** | Race Detection & Tests | `go test -race ./cmd/... ./pkg/...` | **PASS** (100% across all packages) |
| **6** | Binary Compilation | `go build ./cmd/...` | **PASS** (all binaries clean) |

---

## 4. Migration & Upgrade Notes

- **API Consumers**: Cross-border transactions $\ge \$1,000$ USD (or EUR/GBP equivalent) must now include a `travel_rule` object containing `originator` and `beneficiary` legal details.
- **BaaS Partners**: Virtual IBANs can be generated on-demand via `POST /api/v1/wallets/:id/vibans` and immediately receive incoming SEPA / BACS clearing transfers via `/api/v1/clearing/iso20022`.
- **Open Banking TPPs**: Integrate with `/api/v1/open-banking/consents` and provide the `Consent-ID` header for all subsequent AISP/PISP calls.
