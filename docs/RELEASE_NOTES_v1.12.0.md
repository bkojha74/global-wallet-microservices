# Global Wallet Microservices — Release v1.12.0
## Consolidated Release: Core Banking Standards, ISO 20022 Clearing Rails, Multi-Tier GAAP/IFRS Chart of Accounts, Institutional Compliance, BaaS Virtual IBANs & Open Banking PSD2

**Release Version**: `v1.12.0`  
**Previous Official Release**: `v1.10.0`  
**Consolidated Sprints**: Sprint 4 (`v1.11.0`) & Sprint 5 (`v1.12.0`)  
**Release Date**: November 2026  
**Status**: General Availability (GA)  

---

## 1. Executive Summary

**Release v1.12.0** represents a milestone consolidation of **Sprint 4** and **Sprint 5** of the Global Banking Standards & Core Banking Platform roadmap specified in [docs/GLOBAL_BANKING_STANDARDS_AND_ENHANCEMENT_ROADMAP.md](GLOBAL_BANKING_STANDARDS_AND_ENHANCEMENT_ROADMAP.md) and [docs/SPRINT_5_SPECIFICATION_AND_DELIVERABLES.md](SPRINT_5_SPECIFICATION_AND_DELIVERABLES.md).

This unified release transforms the Global Wallet Microservices platform from an internal ledger system into a Tier-1 compatible global core banking platform capable of native institutional clearing, automated regulatory compliance, multi-tenant Banking-as-a-Service (BaaS), and standardized Open Banking integration:

### Sprint 4 Deliverables (Included in v1.12.0):
1. **International Banking Identifiers (`REQ-CLR-01`)**: Implemented strict ISO 13616 International Bank Account Number (IBAN) validation and generation using ISO 7064 Mod 97-10 arbitrary-precision arithmetic (`math/big`) across global jurisdictions, accompanied by ISO 9362 Business Identifier Code (BIC / SWIFT) syntax validation.
2. **ISO 20022 Financial Messaging Engine (`REQ-ISO-01`)**: Built a native Go XML financial messaging suite supporting `pacs.008.001.08` (FI-to-FI Customer Credit Transfer), `pacs.002.001.10` (Payment Status Report), `pain.001.001.09` (Customer Credit Transfer Initiation), and `camt.053.001.08` (Bank-to-Customer Statement).
3. **Institutional Clearing Rail Ingestion (`REQ-ISO-02`)**: Added `POST /api/v1/clearing/iso20022` to `api-gateway`, enabling institutional banking networks (FedNow, SEPA Instant, SWIFT MX) to submit `pacs.008` credit transfers, execute automated counterparty validation and wallet settlement, and receive synchronous `pacs.002` ACTC/RJCT reports.
4. **Multi-Tier GAAP / IFRS Chart of Accounts (`REQ-GL-01`)**: Replaced generic account buckets with a professional 5-tier General Ledger (GL) hierarchy (1000s Assets, 2000s Liabilities, 3000s Equity, 4000s Revenue, 5000s Expenses) mapped across all atomic journal postings.
5. **Automated End-of-Day (EOD) Cutoff & Trial Balance Generator (`REQ-GL-02`)**: Added `GET /api/v1/ledger/trial-balance` and `POST /api/v1/ledger/eod` in `ledger-service` and `api-gateway` to enforce daily accounting balance parity ($\text{Assets} = \text{Liabilities} + \text{Equity} + (\text{Revenue} - \text{Expenses})$) and export certified `camt.053.001.08` XML bank statements.

### Sprint 5 Deliverables (Included in v1.12.0):
6. **FATF Recommendation 16 (Travel Rule) Compliance (`REQ-COMP-01`)**: Real-time inspection and enforcement of mandatory originator and beneficiary legal entity identification on high-value and cross-border transactions ($\ge \$1,000$). Non-compliant transfers are rejected with `400 Bad Request` (`travel_rule_violation: true`).
7. **Real-Time Sanctions Screening & FinCEN SAR Generator (`REQ-COMP-02`)**: Sub-millisecond in-memory screening engine pre-loaded with OFAC SDN, UN, and high-risk sanctions profiles. Employs phonetic normalization and Jaro-Winkler string similarity matching to intercept blocked actors (`403 Forbidden`, `sanctions_blocked: true`) and produces structured FinCEN Suspicious Activity Report (SAR) XML/JSON records.
8. **Banking-as-a-Service (BaaS) Virtual IBAN Engine (`REQ-BAAS-01`)**: Dynamic issuance of ISO 13616 compliant virtual IBANs for `GB` (Barclays 200000), `DE` (Commerzbank 37040044), and `FR` (3000200550) linked directly to wallet balance pots. Incoming ISO 20022 `pacs.008` clearing messages specifying virtual IBANs automatically resolve directly to internal ledger accounts.
9. **Open Banking PSD2 / UK Open Banking Consent Framework (`REQ-OPEN-01`)**: Standardized consent lifecycle management (`AWAITING_AUTHORISATION`, `AUTHORISED`, `REVOKED`, `EXPIRED`) with granular scope validation (`ReadAccountsDetail`, `ReadBalances`, `CreatePaymentInitiation`). Provides dedicated AISP (`/open-banking/accounts`, `/open-banking/balances`) and PISP (`/open-banking/payments`) endpoints.
10. **Automated E2E Test Suite & OpenAPI 3.1 Parity (`REQ-TEST-05`)**: Comprehensive Bruno test collections (`bruno/12-Open-Banking/`, `bruno/07-Compliance-KYC/`, `bruno/06-Transfers/`), automated test harness runner [`scripts/verify_bruno_suite.ps1`](../scripts/verify_bruno_suite.ps1), and complete OpenAPI 3.1 schema definitions in [`cmd/api-gateway/openapi.json`](../cmd/api-gateway/openapi.json).

---

## 2. Comprehensive Architectural Milestones

### 2.1. ISO 13616 IBAN & ISO 9362 BIC Validation Engine (`REQ-CLR-01`)
- **Package**: `pkg/banking`
- **Features**:
  - Full country code length table across global financial jurisdictions (GB: 22, DE: 22, FR: 27, IT: 27, NL: 18, CH: 21, etc.).
  - ISO 7064 Mod 97-10 checksum validation with `big.Int` preventing 64-bit integer overflows during character-to-number expansion.
  - `GenerateIBAN(countryCode, bban)` generator calculating correct check digits.
  - ISO 9362 BIC syntax parser supporting 8-character and 11-character formats.
- **Verification**: `pkg/banking/iban_test.go` (100% test passing).

### 2.2. Native ISO 20022 Financial Messaging Engine (`REQ-ISO-01`)
- **Package**: `pkg/iso20022`
- **Features**:
  - Schema-compliant XML data models for `pacs.008.001.08`, `pacs.002.001.10`, `pain.001.001.09`, and `camt.053.001.08`.
  - `ParsePacs008` and `ParsePain001` XML deserializers.
  - `BuildPacs002` generating technical acceptance (`ACTC`) or rejection (`RJCT`) reports with standard ISO status reason codes (`AC01` Incorrect Account, `AC04` Closed Account, `AM04` Insufficient Funds, `RR04` Regulatory Reason, `FF01` Syntax Error).
  - `BuildCamt053` generating certified bank-to-customer account statements.
- **Verification**: `pkg/iso20022/iso20022_test.go` (100% test passing).

### 2.3. Institutional Clearing Gateway Rail (`REQ-ISO-02`)
- **Component**: `cmd/api-gateway`
- **Endpoint**: `POST /api/v1/clearing/iso20022`
- **Features**:
  - Ingests raw XML payloads with `application/xml` or `text/xml` headers.
  - Performs real-time syntax checking, IBAN validation, and BIC validation on Debtor and Creditor institutions.
  - Dispatches atomic transfer settlement to the active wallet engine.
  - Synchronously returns standard `pacs.002.001.10` XML response with appropriate transaction status and reason codes.
- **Verification**: `cmd/api-gateway/main_test.go:TestHandleClearingISO20022`.

### 2.4. Multi-Tier GAAP/IFRS Chart of Accounts & EOD Cutoff (`REQ-GL-01`, `REQ-GL-02`)
- **Components**: `proto/ledger/ledger.proto`, `cmd/ledger-service`, `cmd/api-gateway`
- **Endpoints**:
  - `GET /api/v1/ledger/trial-balance` — Verifies mathematical balance sheet equation.
  - `POST /api/v1/ledger/eod` — Generates cryptographic EOD cutoff report and exports `camt.053` statement.
- **Chart of Accounts Hierarchy**:
  - `1100`: Central Bank Reserve Cash (Asset)
  - `1210-1230`: Commercial Bank Nostro Accounts (Asset)
  - `1310-1340`: Clearing Rail Settlement Receivables (Asset)
  - `1400`: Safeguarded Customer Trust Funds (Asset)
  - `2110-2120`: Customer Fiat Deposit Liabilities (Liability)
  - `2200`: Clearing Rail Settlement Payables (Liability)
  - `2300`: Inbound Unallocated Wire Suspense (Liability)
  - `3100`: Common Tier-1 Capital Stock (Equity)
  - `3200`: Retained Earnings & Undistributed Surplus (Equity)
  - `4100-4200`: FX Margin Spread & Fee Revenues (Revenue)
  - `5100-5200`: Liquidity Provider & Rail Intermediary Expenses (Expense)
- **Verification**: `cmd/ledger-service/eod_test.go`, `cmd/api-gateway/main_test.go:TestHandleTrialBalanceAndEOD`.

### 2.5. FATF Recommendation 16 (Travel Rule) Compliance (`REQ-COMP-01`)
- **Package**: `pkg/compliance`
- **Features**:
  - Implements `TravelRuleParty` (Legal Name, Account Number, Physical Address, National ID / Tax Number, Country, BIC FI).
  - Implements `ValidateTravelRule(amount float64, currency string, payload *TravelRulePayload) error`.
  - Enforces mandatory originator and beneficiary identification on transfers exceeding \$1,000 USD (or equivalent EUR/GBP thresholds).
  - Integrated into API Gateway transfer middleware.
- **Verification**: `pkg/compliance/compliance_test.go`, `cmd/api-gateway/main_test.go:TestHandleTransfer_TravelRuleEnforcement`.

### 2.6. Real-Time Sanctions Screening & FinCEN SAR Generator (`REQ-COMP-02`)
- **Package**: `pkg/compliance`
- **Features**:
  - `SanctionsEngine` pre-loaded with OFAC SDN, UN Counter-Terrorism, and high-risk transnational crime designations.
  - Sub-millisecond in-memory string matching with phonetic preprocessing and Jaro similarity scoring (threshold $\ge 0.85$).
  - `GenerateSAR`: Produces audit-ready FinCEN Suspicious Activity Report (SAR) XML/JSON records containing suspicious activity classification, transaction legs, and identifying narrative.
  - Integrated into both internal transfer flows and incoming ISO 20022 clearing pipelines.
- **Verification**: `pkg/compliance/compliance_test.go`, `cmd/api-gateway/main_test.go:TestHandleTransfer_SanctionsBlocking`.

### 2.7. Virtual IBAN (vIBAN) Management Engine (`REQ-BAAS-01`)
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

### 2.8. Open Banking PSD2 / UK Open Banking Consent Framework (`REQ-OPEN-01`)
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
- **Financial Institutions**: Institutional clearing messages formatted in `pacs.008.001.08` XML can be submitted directly to `/api/v1/clearing/iso20022`.
- **BaaS Partners**: Virtual IBANs can be generated on-demand via `POST /api/v1/wallets/:id/vibans` and immediately receive incoming SEPA / BACS clearing transfers via `/api/v1/clearing/iso20022`.
- **Open Banking TPPs**: Integrate with `/api/v1/open-banking/consents` and provide the `Consent-ID` header for all subsequent AISP/PISP calls.
