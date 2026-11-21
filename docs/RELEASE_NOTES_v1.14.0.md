# Global Wallet Microservices — Release v1.14.0
## Sprint 7 Release: Multi-Entity Tenant Partitioning, CQRS Event Sourcing, Automated Treasury Sweeps & AI FinCEN SAR Filing

**Release Version**: `v1.14.0`  
**Previous Release**: `v1.13.0`  
**Milestone**: Sprint 7 — Multi-Entity Core Banking, Immutable Event Sourcing & Corporate Treasury  
**Release Date**: November 2026  
**Status**: General Availability (GA)  
**Standard References**: IFRS 9 / SOX 404 (Point-in-Time Auditing), FinCEN Form 111 (Suspicious Activity Report), FCA / BaFin / MAS Safeguarding Regulations, Basel III Liquidity Standards  

---

## 1. Executive Summary

**Release v1.14.0** delivers **Sprint 7** of the Global Banking Standards & Core Banking Platform roadmap specified in [docs/SPRINT_7_SPECIFICATION_AND_DELIVERABLES.md](SPRINT_7_SPECIFICATION_AND_DELIVERABLES.md). 

This release transforms the platform into an international multi-entity banking core equipped with strict subsidiary partitioning, immutable CQRS event sourcing, point-in-time financial auditing, automated corporate treasury liquidity sweeping, and automated AI FinCEN Suspicious Activity Report (SAR) dossier generation:

1. **Multi-Entity Tenant Partitioning & Omnibus Settlement (`REQ-CORE-01`)**: Strict logical and financial segregation across global legal banking entities (`US_CORP`, `EU_GMBH`, `GB_LTD`, `SG_PTE`), automated Due-To / Due-From omnibus accounts, and atomic 4-legged cross-entity clearing journals.
2. **Distributed CQRS Event Sourcing & Point-in-Time Auditing (`REQ-EVENT-01`)**: Immutable, append-only event stream (`AccountOpened`, `FundsCredited`, `FundsDebited`, `FundsHeld`, `FundsReleased`) with nanosecond point-in-time balance reconstruction for IFRS 9 and SOX 404 audits.
3. **Automated Treasury Liquidity Sweeping & Target Balancing (`REQ-TREAS-01`)**: Automated Zero-Balance Account (ZBA) sweeps, target operating balance maintenance, and automated regulatory deposit safeguarding calculations (100% backing of customer deposits).
4. **AI-Powered FinCEN SAR Narrative & XML Dossier Generator (`REQ-AI-03`)**: Transforms graph money mule alerts, rapid passthroughs, and sanctions matches into regulator-ready FinCEN Suspicious Activity Report (SAR Form 111) XML payloads and Part V narrative dossiers.
5. **Comprehensive Test Suite & OpenAPI 3.1 Parity (`REQ-TEST-07`)**: Added Bruno collections `bruno/15-Multi-Entity-Treasury/` and `bruno/16-CQRS-Event-Sourcing/`, extended [`scripts/verify_bruno_suite.ps1`](../scripts/verify_bruno_suite.ps1), updated [`deploy/helm/global-wallet/Chart.yaml`](../deploy/helm/global-wallet/Chart.yaml) to `1.14.0`, and expanded [`cmd/api-gateway/openapi.json`](../cmd/api-gateway/openapi.json).

---

## 2. Detailed Technical Deliverables

### 2.1. Multi-Entity Partitioning & Inter-Entity Omnibus Settlement (`REQ-CORE-01`)
- **Package**: `pkg/multientity`
- **Source Files**: [`pkg/multientity/entity.go`](../pkg/multientity/entity.go), [`pkg/multientity/multientity_test.go`](../pkg/multientity/multientity_test.go)
- **Key Capabilities**:
  - `LegalEntity` data model capturing corporate subsidiaries (`US_CORP`, `EU_GMBH`, `GB_LTD`, `SG_PTE`), home jurisdictions, base currencies, and primary regulators (FinCEN, BaFin, FCA, MAS).
  - `EntityRegistry` providing thread-safe customer wallet-to-entity mapping and conflict validation.
  - `InterEntitySettler` managing cross-entity transactions. When wallets belong to distinct corporate entities, it generates a balanced 4-legged double-entry clearing record:
    - Leg 1 (Source Entity): Debit Source Customer Depository Liability
    - Leg 2 (Source Entity): Credit Due-To Target Entity Clearing Payable (`2100-DUE-TO-<Entity>`)
    - Leg 3 (Target Entity): Debit Due-From Source Entity Clearing Receivable (`1300-DUE-FROM-<Entity>`)
    - Leg 4 (Target Entity): Credit Target Customer Depository Liability
  - Gateway endpoints:
    - `GET /api/v1/entities` — Lists all registered legal banking subsidiaries.
    - `POST /api/v1/entities/settlement` — Executes inter-entity omnibus settlement.
- **Verification**: 100% test pass on intra-entity pass-through, cross-entity 4-leg balancing, and registry conflict defense.

### 2.2. Distributed CQRS Event Sourcing & Point-in-Time Auditing (`REQ-EVENT-01`)
- **Package**: `pkg/eventsourcing`
- **Source Files**: [`pkg/eventsourcing/events.go`](../pkg/eventsourcing/events.go), [`pkg/eventsourcing/eventsourcing_test.go`](../pkg/eventsourcing/eventsourcing_test.go)
- **Key Capabilities**:
  - Immutable event model supporting `AccountOpened`, `FundsCredited`, `FundsDebited`, `FundsHeld`, and `FundsReleased`.
  - Sequential monotonic ordering enforcement (`Sequence`) preventing split-stream divergence.
  - `ReconstructBalanceAt(accountID, timestamp)` replaying historical events up to the exact microsecond cutoff, computing `PostedBalance`, `HeldBalance`, and `AvailableBalance`.
  - Gateway endpoint:
    - `GET /api/v1/ledger/events?account_id=...&as_of=...` — Returns point-in-time audited balance snapshots.
- **Verification**: 100% test pass across full event lifecycle, point-in-time snapshots, and sequence integrity.

### 2.3. Automated Treasury Sweeping & Target Balancing (`REQ-TREAS-01`)
- **Package**: `pkg/treasury`
- **Source Files**: [`pkg/treasury/sweeper.go`](../pkg/treasury/sweeper.go), [`pkg/treasury/treasury_test.go`](../pkg/treasury/treasury_test.go)
- **Key Capabilities**:
  - `SweepRule` model configuring Zero-Balance Accounts (`ZBA`) and `TARGET_BALANCE` concentration strategies with minimum transfer thresholds.
  - `TreasuryEngine` executing multi-account sweeps: sweeping surplus cash to master concentration accounts or replenishing operational deficits.
  - `SafeguardingReport` calculating customer trust deposit backing and identifying surplus/deficit per FCA/BaFin/MAS safeguarding rules.
  - Gateway endpoints:
    - `GET /api/v1/treasury/rules` & `POST /api/v1/treasury/rules` — Manage sweep rules.
    - `POST /api/v1/treasury/sweeps/execute` — Execute automated liquidity sweeps.
    - `GET /api/v1/treasury/safeguarding` — Regulatory safeguarding reserves coverage report.
- **Verification**: 100% test pass on ZBA sweeps, excess/deficit target balancing, and safeguarding coverage calculations.

### 2.4. AI-Powered FinCEN SAR Filing & Regulatory XML Generator (`REQ-AI-03`)
- **Package**: `pkg/ai`
- **Source Files**: [`pkg/ai/sar.go`](../pkg/ai/sar.go), [`pkg/ai/sar_test.go`](../pkg/ai/sar_test.go)
- **Key Capabilities**:
  - `SARNarrativeGenerator` transforming graph mule alerts (circular loops, rapid passthroughs, smurfing rings) and OFAC matches into comprehensive FinCEN Form 111 dossiers.
  - Standardized Part V Narrative generation detailing:
    - Section 1: Executive Summary
    - Section 2: Topological Evidence & Transaction Breakdown
    - Section 3: Behavioral Indicators & Typology
    - Section 4: Statutory & Regulatory Citations (31 U.S.C. 5318(g), 31 CFR 1020.320)
    - Section 5: Institutional Action Taken
  - Electronic XML payload export matching FinCEN BSA filing schema specifications.
  - Gateway endpoint:
    - `POST /api/v1/compliance/sar/generate` — Generates regulator-ready SAR dossier.
- **Verification**: 100% test pass on XML generation, narrative structure, and BSA compliance citations.

---

## 3. Pre-Push Quality Gate Results

| Step | Gate | Tool / Command | Result |
| :--- | :--- | :--- | :--- |
| **1** | Formatting | `gofmt -l .` (Go 1.26.11) | **0 unformatted files** |
| **2** | Static Analysis | `staticcheck ./pkg/... ./cmd/...` | **0 issues** |
| **3** | Go Vet | `go vet ./cmd/... ./pkg/...` | **0 issues** |
| **4** | Security SAST | `gosec -exclude-generated -severity medium ./...` | **0 issues** |
| **5** | Race Detection & Unit Tests | `go test -race ./cmd/... ./pkg/...` | **100% PASS** (25/25 packages) |
| **6** | Binary Compilation | `go build ./cmd/...` | **100% Clean build** |
