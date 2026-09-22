# CI/CD Pipeline Implementation & Deployment Guide

**Project**: Global Multi-Currency Digital Wallet & Ledger Service  
**Workflow File**: [`.github/workflows/ci.yml`](../.github/workflows/ci.yml)  
**Target Environments**: GitHub Cloud Runners (`ubuntu-latest`) & Local Self-Hosted Runner (`[self-hosted, Windows]`)  
**Container Registry**: [Docker Hub](https://hub.docker.com)  

---

## 1. Executive Overview

The Global Wallet microservices platform features an enterprise-grade, automated Continuous Integration and Continuous Deployment (CI/CD) pipeline built with **GitHub Actions**. The pipeline enforces strict contract-first validation, automated unit and race-condition safety gates, parallel multi-target Docker container image compilation, automated registry publication to Docker Hub, and continuous rolling deployments to self-hosted environments.

### Core Objectives
1. **Contract & Quality Integrity**: Ensure Protobuf contracts compile without schema drift, Go source code is formatted according to standard guidelines, and all unit and concurrent race tests pass before any merge.
2. **Optimized Build Cycles**: Leverage GitHub Actions cache backends (`type=gha`) and multi-stage Docker builds to package all 5 microservices in parallel within minutes.
3. **Semantic Artifact Versioning**: Automatically tag container images with branch names (`latest` on `main`), Git commit short SHAs (`sha-<hash>`), and semantic version tags (`vX.Y.Z`).
4. **Resilient Local Deployment**: Automatically deploy or update local Docker Compose stacks on a self-hosted Windows runner with pre-flight health checks and graceful failure handling.
5. **Noise Reduction (`paths-ignore`)**: Prevent documentation updates (`*.md`), guides, and postman collections from triggering redundant build and publish cycles.

---

## 2. Pipeline Architecture & Workflow Diagram

The pipeline consists of three sequential jobs executed across cloud and local runner environments:

```mermaid
flowchart TD
    subgraph Triggers["Trigger Mechanisms"]
        PR["Pull Request<br/>(main, develop)"]
        Push["Git Push<br/>(main, develop)"]
        Tag["Release Tag<br/>(v*)"]
        Manual["workflow_dispatch<br/>(Manual Trigger)"]
    end

    subgraph CI["Job 1: test (Go CI) [ubuntu-latest]"]
        Checkout1["Checkout Repository"]
        GoSetup["Setup Go (go.mod)"]
        Protoc["Install protoc & Plugins"]
        GenProto["Compile Protobuf Contracts<br/>(.pb.go)"]
        Fmt["gofmt Format Verification"]
        UnitTest["Unit Tests (go test ./...)"]
        RaceTest["Race Detector (go test -race ./...)"]

        Checkout1 --> GoSetup --> Protoc --> GenProto --> Fmt --> UnitTest --> RaceTest
    end

    subgraph CD_Build["Job 2: docker-publish (Docker Hub) [ubuntu-latest]"]
        Gate1{"Branch == main<br/>OR Tag == v*<br/>OR Manual?"}
        Buildx["Setup Docker Buildx"]
        DHLogin["Authenticate Docker Hub"]
        Matrix["Parallel Matrix Build<br/>(5 Microservices)"]
        Cache["GHA Layer Caching (type=gha)"]
        PushDH["Push Images to Docker Hub<br/>(latest, semver, sha)"]

        Gate1 -->|Yes| Buildx --> DHLogin --> Matrix --> Cache --> PushDH
    end

    subgraph CD_Deploy["Job 3: deploy (Local Server) [self-hosted, Windows]"]
        Runner["Self-Hosted Runner<br/>[self-hosted, Windows]"]
        CheckDock["Pre-Flight Probe<br/>(docker info)"]
        Decision{"Docker Ready?"}
        SkipWarn["Skip Deployment<br/>(Emit Warning Annotation)"]
        PullImg["Pull Latest Images<br/>(cmd shell)"]
        NetVol["Validate Network & Volumes<br/>(wallet_shared_net)"]
        StackUp["Deploy Modular Stacks<br/>(MongoDB -> RabbitMQ -> Auth -> Logging -> Core)"]
        HealthCheck["Verify Containers<br/>(docker ps)"]

        PushDH --> Runner --> CheckDock --> Decision
        Decision -->|No / Closed| SkipWarn
        Decision -->|Yes| PullImg --> NetVol --> StackUp --> HealthCheck
    end

    PR --> CI
    Push --> CI
    Tag --> CI
    Manual --> CI
    RaceTest --> Gate1
```

---

## 3. Workflow Triggers & Path Filtering

The pipeline is configured in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) with path filtering to optimize resource consumption:

```yaml
on:
  push:
    branches: [main, develop]
    tags: ['v*']
    paths-ignore:
      - '**.md'
      - 'docs/**'
      - 'LICENSE'
      - 'SECURITY.md'
      - '.gitignore'
      - 'postman/**'
  pull_request:
    branches: [main, develop]
    paths-ignore:
      - '**.md'
      - 'docs/**'
      - 'LICENSE'
      - 'SECURITY.md'
      - '.gitignore'
      - 'postman/**'
  workflow_dispatch:
```

### Trigger Matrix

| Event | Branches / Tags | Jobs Executed | Purpose |
|---|---|---|---|
| `pull_request` | `main`, `develop` | `test` | Quality gate for incoming code changes before merge |
| `push` | `develop` | `test` | Integration verification on active development branch |
| `push` | `main` | `test` $\rightarrow$ `docker-publish` $\rightarrow$ `deploy` | End-to-end production testing, container publishing, and deployment |
| `push` (tag) | `refs/tags/v*` | `test` $\rightarrow$ `docker-publish` $\rightarrow$ `deploy` | Release creation with semantic version container tagging |
| `workflow_dispatch` | Any branch | `test` $\rightarrow$ `docker-publish` $\rightarrow$ `deploy` | On-demand manual triggering from GitHub Actions Web UI |

> [!NOTE]
> **Path Filtering (`paths-ignore`)**: Any commits that strictly update documentation, markdown files, licenses, `.gitignore`, or Postman test files bypass the pipeline entirely, preventing unnecessary test runs and registry build charges.

---

## 4. Pipeline Jobs & Execution Stages

### Stage 1: Continuous Integration (`test`)
* **Runner**: `ubuntu-latest`
* **Execution Steps**:
  1. **Source Checkout**: `actions/checkout@v4` pulls the commit tree.
  2. **Go Toolchain**: `actions/setup-go@v5` configures Go using the version specified in `go.mod` and enables Go module caching.
  3. **Protobuf Compiler**: Installs system `protobuf-compiler` alongside Go plugins:
     - `google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11`
     - `google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.0`
  4. **Contract Compilation**: Compiles the gRPC Protobuf v3 definitions:
     - `proto/wallet/wallet.proto`
     - `proto/ledger/ledger.proto`
     - `proto/auth/auth.proto`
  5. **Formatting Gate**: Runs `test -z "$(gofmt -l .)"` to ensure zero formatting deviations.
  6. **Automated Unit Testing**: Runs all tests with fresh state (`go test ./... -count=1`).
  7. **Race Condition Detector**: Executes tests under the Go race detector (`go test -race ./... -count=1`) to identify data races, goroutine leaks, or locking deadlocks.

---

### Stage 2: Multi-Target Container Packaging (`docker-publish`)
* **Runner**: `ubuntu-latest`
* **Condition**: Triggered only when `test` succeeds and the ref is `main`, a version tag (`v*`), or a manual dispatch.
* **Matrix Strategy**: 5 microservices build simultaneously in parallel:

| Service Matrix Identifier | Dockerfile Target | Container Repository |
|---|---|---|
| `wallet-api-gateway` | `api-gateway` | `${DOCKERHUB_USERNAME}/wallet-api-gateway` |
| `wallet-service` | `wallet-service` | `${DOCKERHUB_USERNAME}/wallet-service` |
| `wallet-ledger-service` | `ledger-service` | `${DOCKERHUB_USERNAME}/wallet-ledger-service` |
| `wallet-auth-service` | `auth-service` | `${DOCKERHUB_USERNAME}/wallet-auth-service` |
| `wallet-logging-service` | `logging-service` | `${DOCKERHUB_USERNAME}/wallet-logging-service` |

* **Buildx & Cache Backend**:
  ```yaml
  cache-from: type=gha
  cache-to: type=gha,mode=max
  ```
  GitHub Actions cache stores intermediate Docker build layers, ensuring incremental builds finish in under 30 seconds.
* **Automated Tagging Engine**:
  - `latest` (applied when pushing to `main`)
  - `vX.Y.Z` & `vX.Y` (applied on version tags)
  - `sha-<short>` (e.g. `sha-3685067`, applied on every build for complete provenance tracking)

---

### Stage 3: Continuous Deployment (`deploy`)
* **Runner**: Self-hosted Windows runner (`[self-hosted, Windows]`)
* **Shell**: Native Windows `cmd` (`shell: cmd`) to avoid PowerShell script execution policy restrictions (`Restricted`).
* **Pre-Flight Docker Daemon Readiness Check**:
  Before running any deployment commands, the runner executes a pre-flight probe:
  ```cmd
  docker info >nul 2>&1
  if errorlevel 1 (
    echo ::warning::Docker daemon is not running or accessible on this Windows runner. Skipping local deployment.
    echo DOCKER_READY=false>> "%GITHUB_ENV%"
  ) else (
    echo Docker daemon is running and accessible.
    echo DOCKER_READY=true>> "%GITHUB_ENV%"
  )
  exit /b 0
  ```
  - **If Docker is offline or stopped**: Sets `DOCKER_READY=false`, emits a warning annotation, and cleanly exits with code `0`. Subsequent steps (`pull`, `up`, `ps`) are skipped automatically. The GitHub Actions run stays **GREEN**.
  - **If Docker is running and healthy**: Sets `DOCKER_READY=true` and executes the rolling deployment.
* **Step-Level Fault Tolerance (`continue-on-error: true`)**:
  All deployment steps include `continue-on-error: true` to ensure local hardware or network glitches never mark a GitHub pull request or release as failed.
* **Rolling Deployment Order**:
  1. `docker compose -f docker-compose.mongodb.yml pull && up -d` (MongoDB Replica Set)
  2. `docker compose -f docker-compose.rabbitmq.yml pull && up -d` (AMQP Broker)
  3. `docker compose -f docker-compose.auth.yml pull && up -d` (Keycloak & Auth Service)
  4. `docker compose -f docker-compose.logging.yml pull && up -d` (Logging Service & Consumer)
  5. `docker compose -f docker-compose.yml pull && up -d` (API Gateway, Wallets, Ledger)
* **Verification**: Executes `docker ps` to display live container statuses and exposed ports.

---

## 5. Configuring GitHub Secrets & Permissions

To allow GitHub Actions to build, publish, and pull container images from Docker Hub, you must configure two encrypted secrets in your GitHub repository.

### Step 5.1: Create a Docker Hub Personal Access Token (PAT)
1. Log in to [Docker Hub](https://hub.docker.com).
2. Click on your profile avatar in the upper right corner and select **Account Settings**.
3. In the left navigation menu, click **Security**.
4. Click **New Access Token**.
5. Provide a description:
   - **Access Token Description**: `github-actions-global-wallet`
   - **Access permissions**: Select **Read & Write** (or **Read, Write, Delete**).
6. Click **Generate**.
7. **Copy the generated token immediately** (it will not be shown again).

---

### Step 5.2: Configure Secrets in GitHub Repository
1. Navigate to your repository on GitHub:  
   `https://github.com/<owner>/global-wallet-microservices`
2. Click on the **Settings** tab at the top of the repository.
3. In the left sidebar, expand **Secrets and variables** and select **Actions**.
4. In the **Repository secrets** section, click **New repository secret**.
5. Add the **Username Secret**:
   - **Name**: `DOCKERHUB_USERNAME`
   - **Secret**: Your Docker Hub username (e.g., `bkojha`)
   - Click **Add secret**.
6. Click **New repository secret** again to add the **Token Secret**:
   - **Name**: `DOCKERHUB_TOKEN`
   - **Secret**: Paste the Personal Access Token generated in Step 5.1.
   - Click **Add secret**.

```
┌─────────────────────────────────────────────────────────────────────────┐
│                       GITHUB ACTIONS SECRETS MATRIX                     │
├─────────────────────┬───────────────────────────────────────────────────┤
│ Secret Name         │ Value / Example                                   │
├─────────────────────┼───────────────────────────────────────────────────┤
│ DOCKERHUB_USERNAME  │ bkojha                                            │
│ DOCKERHUB_TOKEN     │ dckr_pat_xxxxxxxxxxxxxxxxxxxxxxxxxxxx            │
└─────────────────────┴───────────────────────────────────────────────────┘
```

---

### Step 5.3: Repository Workflow Permissions
1. Under **Settings** -> **Actions** -> **General**.
2. Scroll to **Workflow permissions**.
3. Ensure **Read repository contents and packages permissions** is selected.
4. Click **Save**.

---

## 6. Self-Hosted Runner Setup (Windows)

The deployment job targets a self-hosted Windows runner registered with the labels `[self-hosted, Windows]`.

### Step 6.1: Registering the Runner
1. In your GitHub repository, go to **Settings** -> **Actions** -> **Runners**.
2. Click **New self-hosted runner**.
3. Select **Windows** as the Runner image architecture (**x64**).
4. On your Windows machine, open an administrative terminal and create the runner directory:
   ```cmd
   mkdir D:\actions-runner && cd /d D:\actions-runner
   ```
5. Download the latest runner package:
   ```powershell
   Invoke-WebRequest -Uri https://github.com/actions/runner/releases/download/v2.327.0/actions-runner-win-x64-2.327.0.zip -OutFile actions-runner.zip
   Expand-Archive -Path actions-runner.zip -DestinationPath .
   ```
6. Run the configuration script using the token provided in the GitHub UI:
   ```cmd
   config.cmd --url https://github.com/<owner>/global-wallet-microservices --token <RUNNER_REGISTRATION_TOKEN>
   ```
7. When prompted:
   - **Enter the name of runner**: Press Enter for default hostname or specify a custom name (e.g. `NANDITA`).
   - **Enter runner group**: Press Enter for default (`Default`).
   - **Enter additional labels**: `Windows`

---

### Step 6.2: Running the Runner (Interactive vs Service)

#### Option A: Interactive Mode (Recommended for Docker Desktop)
Running the runner interactively ensures it executes under your active user account (`bkojh`), giving it direct, unhindered access to Docker Desktop's named pipe (`//./pipe/docker_engine`):
```cmd
cd /d D:\actions-runner
run.cmd
```

#### Option B: Windows Service Mode
If you prefer running the runner as a continuous Windows Service:
1. Stop and uninstall any existing default service:
   ```cmd
   cd /d D:\actions-runner
   .\svc.cmd stop
   .\svc.cmd uninstall
   ```
2. Install the service under your local user account (not `NT AUTHORITY\NETWORK SERVICE`):
   ```cmd
   .\svc.cmd install .\bkojh
   ```
   *(Enter your Windows account password when prompted).*
3. Start the service:
   ```cmd
   .\svc.cmd start
   ```

> [!TIP]
> Running the service under your user account ensures the runner shares the same desktop session permissions as Docker Desktop, enabling seamless automated deployments.

---

## 7. Troubleshooting & Frequently Asked Questions

### 1. Error: `open //./pipe/docker_engine: The system cannot find the file specified`
* **Cause**: Docker Desktop is either stopped, starting up, or running in a different user session than the GitHub Actions runner.
* **Resolution**:
  1. Open and start **Docker Desktop**.
  2. Verify that `docker info` succeeds in your command prompt.
  3. The workflow's pre-flight probe automatically detects this condition and cleanly skips deployment with a warning annotation instead of failing your pipeline.

### 2. Error: `File ... cannot be loaded because running scripts is disabled on this system`
* **Cause**: Windows PowerShell default execution policy is set to `Restricted`.
* **Resolution**: The workflow explicitly uses `shell: cmd` for all deployment steps on Windows, completely bypassing PowerShell execution policy restrictions.

### 3. Error: `Process completed with exit code 1` in Readiness Step
* **Cause**: Windows `cmd` retains the exit code of `docker info` even when executing `echo`.
* **Resolution**: The step concludes with an explicit `exit /b 0`, ensuring the probe step always exits cleanly regardless of Docker's status.

### 4. Updating Documentation Triggers Full Builds
* **Cause**: Missing path filters.
* **Resolution**: Path filtering (`paths-ignore`) is configured for all markdown files (`**.md`), documentation directories (`docs/**`), licenses, and test collections.

---

## 8. Summary of Published Docker Hub Images

Once published by the pipeline, all microservice container images are publicly available under your Docker Hub namespace:

```bash
# Pull the latest production images
docker pull bkojha/wallet-api-gateway:latest
docker pull bkojha/wallet-service:latest
docker pull bkojha/wallet-ledger-service:latest
docker pull bkojha/wallet-auth-service:latest
docker pull bkojha/wallet-logging-service:latest
```

These images can be run with standard Docker Compose or deployed onto Kubernetes clusters using the manifests in [`k8s/`](../k8s/).
