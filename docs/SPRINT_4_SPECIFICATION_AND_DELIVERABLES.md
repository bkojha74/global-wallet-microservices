# Sprint 4 Specification: ISO 20022 Clearing Rails & Multi-Tier GAAP/IFRS Chart of Accounts

- **Document Version**: 1.0.0
- **Target Sprint**: Sprint 4 (`v1.11.0`)
- **Status**: APPROVED FOR EXECUTION
- **Author**: Platform Architecture Team
- **Parent Blueprint**: [`docs/GLOBAL_BANKING_STANDARDS_AND_ENHANCEMENT_ROADMAP.md`](GLOBAL_BANKING_STANDARDS_AND_ENHANCEMENT_ROADMAP.md)

---

## 1. Executive Overview

Sprint 4 transitions the Global Wallet platform from a multi-currency wallet microservice suite to a **Tier-1 Core Banking System** capable of interfacing directly with national clearing schemes (FedNow, SEPA Instant, CHAPS, RTGS) and maintaining audit-certified financial statements compliant with **GAAP / IFRS and Basel III**.

---

## 2. Sprint 4 Requirements Breakdown

| Req ID | Component | Severity | Description | Target Deliverable |
|---|---|:---:|---|---|
| **`REQ-CLR-01`** | `pkg/banking` | 🔴 P0 | International Banking Validation (ISO 13616 IBAN Mod 97-10 & ISO 9362 BIC) | Complete validator and builder for international payment routing |
| **`REQ-ISO-01`** | `pkg/iso20022` | 🔴 P0 | ISO 20022 Financial Messaging Engine (`pain.001`, `pacs.008`, `pacs.002`, `camt.053`) | XML serializers, parsers, and schema validators with standard status codes |
| **`REQ-ISO-02`** | `cmd/api-gateway` | 🔴 P0 | Clearing Ingestion Endpoint (`POST /api/v1/clearing/iso20022`) | Accepts ISO 20022 XML, maps to core banking deposits/transfers, returns `pacs.002` |
| **`REQ-GL-01`** | `proto/ledger`, `cmd/ledger-service` | 🟡 P1 | Multi-Tier GAAP/IFRS Chart of Accounts (COA) | Five-tier accounting hierarchy (1000s–5000s) on double-entry journals |
| **`REQ-GL-02`** | `cmd/ledger-service`, `cmd/api-gateway` | 🟡 P1 | Automated End-of-Day (EOD) Cutoff & Trial Balance Generator | Validates $\text{Assets} = \text{Liab} + \text{Eq} + (\text{Rev} - \text{Exp})$ and exports `camt.053` |
| **`REQ-TEST-04`**| `bruno/`, `scripts` | 🟢 P2 | Automated Bruno E2E Test Suite & OpenAPI Parity for Clearing and EOD | Full test coverage across ISO 20022 clearing and GL balance sheet generation |

---

## 3. Detailed Architecture Specifications

### 3.1. REQ-CLR-01: International Banking Identifier Engine (`pkg/banking`)
- **IBAN Validation**:
  - Validates country code length (e.g. DE=22, FR=27, GB=22, US virtual=24).
  - Implements ISO 7064 Mod 97-10 checksum verification (moving country code and check digits to end, translating letters A-Z to 10-35, and validating `remainder == 1`).
  - Helper to generate valid test vIBANs with correct Mod 97-10 check digits.
- **BIC / SWIFT Validation**:
  - Validates 8 or 11 character ISO 9362 Bank Identifier Code syntax: `4 letters (bank) + 2 letters (country) + 2 alphanumeric (location) + optional 3 alphanumeric (branch)`.

### 3.2. REQ-ISO-01: ISO 20022 Financial Messaging Engine (`pkg/iso20022`)
- **`pacs.008.001.08`** (FI-to-FI Customer Credit Transfer):
  - Ingests institutional clearing payment orders: Group Header, Message ID, Settlement Info, Debtor/Creditor Agent (BIC), Debtor/Creditor Accounts (IBAN), Interbank Settlement Amount & Currency.
- **`pacs.002.001.10`** (Payment Status Report):
  - Returns execution confirmation:
    - Status `ACTC` (Accepted Technical Validation) when credit is executed.
    - Status `RJCT` (Rejected) with standard ISO reason codes:
      - `AC01` = Incorrect Account Number
      - `AC04` = Closed / Frozen Account
      - `AM04` = Insufficient Funds
      - `RR04` = Regulatory Reason / Compliance Rejection
- **`pain.001.001.09`** (Customer Credit Transfer Initiation):
  - Handles B2B corporate payroll and bulk credit instructions.
- **`camt.053.001.08`** (Bank-to-Customer Account Statement):
  - Generates certified daily account statements with opening and closing balance and transaction entries.

### 3.3. REQ-ISO-02: API Gateway Clearing Ingestion (`POST /api/v1/clearing/iso20022`)
- Public / Partner banking endpoint accepting `Content-Type: application/xml` or `application/json`.
- Validates XML structure, checks BIC and IBAN checksums.
- Translates `pacs.008` instruction to core banking deposit / transfer RPCs into `wallet-service`.
- Formulates `pacs.002.001.10` XML response with matching `OriginalMessageId` and `TransactionStatus`.

### 3.4. REQ-GL-01: Multi-Tier GAAP/IFRS Chart of Accounts
- Hierarchical account structure:
  - `1000`: Assets (Debit normal)
    - `1100`: Central Bank Reserves
    - `1200`: Nostro Accounts (Commercial Banks)
    - `1300`: Clearing Scheme Receivables
  - `2000`: Liabilities (Credit normal)
    - `2100`: Customer Demand Deposits (Wallet balances)
    - `2200`: Safeguarded Client Funds (Statutory Trust)
    - `2300`: Inbound Clearing Suspense
  - `3000`: Equity (Credit normal)
    - `3100`: Paid-In Capital
  - `4000`: Revenue (Credit normal)
    - `4100`: Transaction Fee Income
    - `4200`: FX Conversion Spread Margin
  - `5000`: Expenses (Debit normal)
    - `5100`: Scheme Interchange & Clearing Fees
- Updates `proto/ledger/ledger.proto` and `cmd/ledger-service` to include `gl_account_code` and account tier on all entries.

### 3.5. REQ-GL-02: Automated End-of-Day (EOD) Cutoff & Trial Balance
- Endpoint `POST /api/v1/ledger/eod` creates an atomic daily cutoff timestamp.
- Endpoint `GET /api/v1/ledger/trial-balance` aggregates net debits and credits across all GL accounts.
- Enforces and returns verification of the fundamental balance sheet equation:
  $$\sum \text{Assets} == \sum \text{Liabilities} + \sum \text{Equity} + \left(\sum \text{Revenue} - \sum \text{Expenses}\right)$$
- Supports statement export in JSON and `camt.053` XML format.

---

## 4. Verification & Quality Gates
- `go build ./...`: 0 compilation errors across all microservices.
- `gofmt -l .`: 100% formatted.
- `go vet ./...`: 0 issues.
- `gosec`: 0 security issues.
- `go test -race ./...`: 100% pass across all packages.

