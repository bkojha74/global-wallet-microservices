# Keycloak Identity Provider — Administrator & Developer Guide
### Global Multi-Currency Digital Wallet Platform

---

## 1. Overview & Architecture

The Global Wallet platform uses **Keycloak** (built on Quarkus) as its central OpenID Connect (OIDC) and OAuth 2.0 Identity Provider. It provides:
- **Centralized Identity Management**: Single sign-on (SSO), secure credential hashing (bcrypt/pbkdf2), and token lifecycle.
- **Role-Based Access Control (RBAC)**: Fine-grained platform permissions via Realm roles (`admin`, `user`, `auditor`, `compliance`).
- **OAuth 2.0 Scopes**: Permission boundaries attached to access tokens (`wallet:read`, `wallet:transfer`, `cluster:admin`, `ledger:audit`).
- **Asymmetric Cryptography**: Issues RS256 digitally-signed JWT tokens verified downstream via standard OIDC Discovery and JSON Web Key Sets (JWKS).
- **Hybrid Auth Support**: The platform's `auth-service` validates incoming tokens using Keycloak's published JWKS keys, with automated local MongoDB fallback if Keycloak is temporarily unavailable.

```
   ┌────────────────────────┐                   ┌───────────────────────────────┐
   │   Client / Frontend    │                   │       Keycloak (:8085)        │
   │    Bruno / Postman     │─── 1. Login ────► │      Realm: wallet-realm      │
   └───────────┬────────────┘                   └───────────────┬───────────────┘
               │                                                │
      2. Bearer JWT (RS256)                             3. JWKS Public Keys
               │                                                │
               ▼                                                ▼
   ┌────────────────────────┐                   ┌───────────────────────────────┐
   │   API Gateway (:8080)  │─── 4. gRPC ─────► │     Auth Service (:50054)     │
   │   Reverse Proxy & RBAC │    ValidateToken  │ Hybrid JWKS Signature Verifier│
   └───────────┬────────────┘                   └───────────────────────────────┘
               │
        5. Forward Claims
               ▼
   ┌────────────────────────┐
   │    Wallet / Ledger     │
   │      Microservices     │
   └────────────────────────┘
```

---

## 2. How `wallet-realm` is Created & Provisioned

### What is a Realm in Keycloak?
In Keycloak, a **Realm** is an isolated security and tenancy boundary. It manages its own set of users, credentials, roles, client applications, and cryptographic signing keys. 
* The **`master`** realm is the administrative realm reserved exclusively for Keycloak super-administrators to manage Keycloak itself.
* The **`wallet-realm`** is our dedicated application realm housing all Global Wallet users, microservice clients, and banking permission scopes.

Depending on how you deploy the platform, `wallet-realm` is created through one of the following methods:

---

### Method 1: Automated GitOps Boot-Time Import (Default & Recommended)

In standard containerized environments (Docker Compose, Kubernetes Helm, Plain K8s), Keycloak is configured with declarative GitOps auto-import. On fresh container startup, Keycloak reads the realm definition file and automatically provisions `wallet-realm` before accepting traffic.

The declarative realm specification is maintained in version control at:
- [`deploy/keycloak/wallet-realm-realm.json`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/deploy/keycloak/wallet-realm-realm.json)
- [`deploy/keycloak/wallet-realm.json`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/deploy/keycloak/wallet-realm.json)

#### A. Docker Compose Deployment (`docker-compose.auth.yml`)
Keycloak runs with the `--import-realm` flag and mounts the realm JSON file into Keycloak's designated import directory (`/opt/keycloak/data/import/`):

```yaml
  keycloak:
    image: quay.io/keycloak/keycloak:latest
    container_name: wallet_keycloak
    command: start-dev --import-realm
    environment:
      - KEYCLOAK_ADMIN=admin
      - KEYCLOAK_ADMIN_PASSWORD=admin
      - KC_HEALTH_ENABLED=true
    volumes:
      - ./deploy/keycloak/wallet-realm-realm.json:/opt/keycloak/data/import/wallet-realm-realm.json:ro
    ports:
      - "8085:8080"
```

#### B. Kubernetes Helm Chart (`deploy/helm/global-wallet/templates/keycloak.yaml`)
In Kubernetes, the realm JSON is packaged inside a `ConfigMap` (`keycloak-realm-config`) and mounted into the Keycloak pod at `/opt/keycloak/data/import/wallet-realm.json`:

```yaml
volumeMounts:
  - name: realm-config
    mountPath: /opt/keycloak/data/import/wallet-realm.json
    subPath: wallet-realm-realm.json
    readOnly: true
args: ["start-dev", "--import-realm"]
```

#### C. Plain Kubernetes Manifests (`k8s/11-auth-service.yaml`)
Plain Kubernetes manifests follow the identical pattern using the `keycloak-realm-config` ConfigMap mounted to `/opt/keycloak/data/import/wallet-realm-realm.json`.

> **How `--import-realm` Works**:
> When Keycloak boots with `--import-realm`, it scans `/opt/keycloak/data/import/` for any `*-realm.json` files. If the database does not already contain a realm with `id: "wallet-realm"`, Keycloak imports all roles, scopes, clients, and the bootstrap admin account automatically. If the realm already exists, Keycloak preserves existing data and skips the import (strategy: `IGNORE_EXISTING`).

---

### Method 2: One-Click Web Console Import (For Fresh / Vanilla Keycloak)

If you are running an existing or unmanaged Keycloak instance (e.g., enterprise Red Hat build of Keycloak, AWS ECS, or bare metal) where `wallet-realm` does not exist yet:

#### Step 1: Navigating to the "Create Realm" Screen
When you first log in to `http://localhost:8085/admin` (using `admin` / `admin`), Keycloak lands by default on the **Realm settings** page for the `master` realm (URL: `.../#/master/realm-settings`). The **Resource file** field is NOT on this page because this page only edits the existing `master` realm.

To reach the **Create Realm** page where the import field is located, use any of these 3 routes:

* **Route A (Left Sidebar — Recommended)**:
  1. In the left navigation sidebar, locate **`Manage realms`** (the 2nd item near the top, directly beneath the Keycloak logo / `Current realm` indicator).
  2. Click **`Manage realms`**.
  3. On the realms list page that opens, click the blue **`Create realm`** button at the top-right.

* **Route B (Realm Dropdown)**:
  1. Click directly on the realm switcher at the top-left of the sidebar (where it displays **`Keycloak`** with the blue **`Current realm`** badge).
  2. A dropdown drawer will open showing existing realms.
  3. Click **`Create realm`** at the top/bottom of the dropdown.

* **Route C (Direct URL)**:
  Paste this URL directly into your browser address bar:
  ```text
  http://localhost:8085/admin/master/console/#/create/realm
  ```

#### Step 2: Locating the "Resource File" Field & Importing
Once you are on the **Create realm** page (`.../#/create/realm`):
1. Right at the top of the form, you will see the **Resource file** section with a file upload box / dropzone:
   * Label: **`Resource file`** (or **`Import realm`**)
   * Control: A button labeled **`Browse...`** alongside **`Drag and drop file here`**.
2. Click **`Browse...`** and select [`deploy/keycloak/wallet-realm-realm.json`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/deploy/keycloak/wallet-realm-realm.json) from your local project repository.
3. Keycloak will automatically read the JSON payload, populate **`Realm name: wallet-realm`**, and prepare all clients (`wallet-api`), scopes (`wallet:read`, `wallet:transfer`, etc.), and roles.
4. Click the blue **`Create`** button at the bottom of the page.
5. `wallet-realm` is now created and active!

---

### Method 3: Step-by-Step Manual Realm Creation from Scratch

If you ever need to manually construct `wallet-realm` from a blank Keycloak instance without using the JSON import file, follow these precise configuration steps:

#### Step 3.1: Create the Realm
1. Click the top-left realm dropdown &rarr; click **Create Realm**.
2. **Realm name**: `wallet-realm`
3. **Enabled**: `ON`
4. Click **Create**.

#### Step 3.2: Configure Token Lifespans & SSO
1. In the left menu, click **Realm settings**.
2. Click the **Tokens** tab:
   * **Access Token Lifespan**: `15 Minutes` (900 seconds)
   * **SSO Session Idle**: `30 Minutes` (1800 seconds)
   * **SSO Session Max**: `10 Hours` (36000 seconds)
3. Click **Save**.

#### Step 3.3: Create Custom Client Scopes
In the left menu, click **Client scopes** &rarr; click **Create client scope** for each of the 4 platform scopes:

| Scope Name | Description | Protocol | Type | Include in Token Scope |
| :--- | :--- | :--- | :--- | :--- |
| **`wallet:read`** | Read wallet balances and transaction history | `OpenID Connect` | `Default` | `ON` |
| **`wallet:transfer`** | Initiate funds transfers between wallets | `OpenID Connect` | `Default` | `ON` |
| **`cluster:admin`** | Trigger cluster failover and administrative operations | `OpenID Connect` | `Optional` | `ON` |
| **`ledger:audit`** | Access immutable ledger audit trails | `OpenID Connect` | `Optional` | `ON` |

#### Step 3.4: Create the API Gateway & Auth Client (`wallet-api`)
1. In the left menu, click **Clients** &rarr; click **Create client**.
2. **General Settings**:
   * **Client type**: `OpenID Connect`
   * **Client ID**: `wallet-api`
   * **Name**: `Global Wallet API Gateway & Auth Service`
   * Click **Next**.
3. **Capability Config**:
   * **Client authentication**: Toggle `ON` (Confidential client)
   * **Authentication flow**: Check `Standard flow`, `Direct access grants`, and `Service accounts roles`.
   * Click **Next**.
4. **Login Settings**:
   * **Valid redirect URIs**: `http://localhost:8080/*`, `http://localhost:3000/*`
   * **Web origins**: `+` (or `*`)
   * Click **Save**.
5. **Set Client Secret**:
   * Click the **Credentials** tab of client `wallet-api`.
   * The generated client secret is displayed. Set or copy this secret into `.env`:
     ```env
     KEYCLOAK_CLIENT_ID=wallet-api
     KEYCLOAK_CLIENT_SECRET=wallet-client-secret-12345
     ```
6. **Assign Client Scopes**:
   * Click the **Client scopes** tab of client `wallet-api`.
   * Click **Add client scope** &rarr; select `wallet:read`, `wallet:transfer`, `cluster:admin`, `ledger:audit` &rarr; click **Add** (as Default/Optional as needed).

#### Step 3.5: Create Realm Roles
In the left menu, click **Realm roles** &rarr; click **Create role**:
* Role: **`admin`** &mdash; Description: `Platform administrator with full cluster access`
* Role: **`user`** &mdash; Description: `Standard wallet account holder`
* Role: **`auditor`** &mdash; Description: `Compliance and ledger auditor`

#### Step 3.6: Create Bootstrap Admin User
1. In the left menu, click **Users** &rarr; click **Add user**.
2. **Username**: `admin`, **Email**: `admin@wallet.local`, **Email verified**: `ON`, **Enabled**: `ON` &rarr; click **Create**.
3. **Credentials** tab &rarr; **Set password**: enter `admin123` &rarr; toggle **Temporary** to `OFF` &rarr; click **Save**.
4. **Role mapping** tab &rarr; click **Assign role** &rarr; select **`admin`** &rarr; click **Assign**.

---

### Method 4: Automated Admin REST API / CLI (`kcadm.sh`)

You can also programmatically create `wallet-realm` on any freshly deployed Keycloak instance using Keycloak's Admin REST API:

```powershell
# 1. Obtain Keycloak master admin bearer token
$token = (Invoke-RestMethod -Uri "http://localhost:8085/realms/master/protocol/openid-connect/token" `
  -Method Post `
  -Body "client_id=admin-cli&grant_type=password&username=admin&password=admin" `
  -ContentType "application/x-www-form-urlencoded").access_token

# 2. Post the full realm JSON file to create the realm instantly
$realmJson = Get-Content -Raw "deploy/keycloak/wallet-realm-realm.json"
Invoke-RestMethod -Uri "http://localhost:8085/admin/realms" `
  -Method Post `
  -Headers @{ Authorization = "Bearer $token" } `
  -Body $realmJson `
  -ContentType "application/json"

Write-Host "wallet-realm created successfully via Keycloak Admin API!" -ForegroundColor Green
```

---

## 3. Accessing the Keycloak Portals

| Portal | URL | Purpose |
| :--- | :--- | :--- |
| **Admin Console** | [http://localhost:8085/admin](http://localhost:8085/admin) | Full management of realms, users, roles, and clients |
| **Account Console** | [http://localhost:8085/realms/wallet-realm/account](http://localhost:8085/realms/wallet-realm/account) | Self-service portal for users (manage profile, change password) |
| **OIDC Discovery** | [http://localhost:8085/realms/wallet-realm/.well-known/openid-configuration](http://localhost:8085/realms/wallet-realm/.well-known/openid-configuration) | Public metadata (endpoints, JWKS URI, supported claims) |
| **JWKS Public Keys** | [http://localhost:8085/realms/wallet-realm/protocol/openid-connect/certs](http://localhost:8085/realms/wallet-realm/protocol/openid-connect/certs) | Public RSA certificates used by `auth-service` for signature verification |

### Master Admin Login Credentials
* **Username**: `admin`
* **Password**: `admin`

---

## 4. Navigating & Verifying `wallet-realm`

When you first log in to the Keycloak Admin Console, you will be placed in the `master` realm.

1. Look at the **top-left corner** of the Keycloak console.
2. Click on the dropdown menu displaying **`master`**.
3. Select **`wallet-realm`** from the list.

> [!IMPORTANT]
> Always verify that you are operating inside **`wallet-realm`**, not `master`. All wallet clients, customer users, and banking roles must belong to `wallet-realm`.

---

## 5. Customer Onboarding (Creating Users in Production)

In a clean production environment, no customer accounts (`alice`, `bob`, `charlie_owner`) exist by default. Customers are provisioned dynamically.

### Option A: Via Keycloak Web Console
1. In the left navigation menu, click **Users** (under *Manage*).
2. Click **Add user**.
3. Fill in the user profile:
   * **Username**: e.g. `alice` or customer identifier
   * **Email**: e.g. `alice@wallet.local`
   * **First name**: e.g. `Alice` *(Mandatory in Keycloak 26! If left blank, login fails with "Account is not fully set up")*
   * **Last name**: e.g. `Smith` *(Mandatory in Keycloak 26!)*
   * **Email verified**: Toggle to **ON**
   * Click **Create**.
4. Click on the **Credentials** tab:
   * Click **Set password** &rarr; enter password (e.g. `Password123!`).
   * Toggle **Temporary** to **OFF** *(Crucial: if left ON, the user cannot authenticate via APIs without first changing their password via browser)*.
   * Click **Save** &rarr; **Save password**.
5. Click on the **Role mapping** tab:
   * Click **Assign role** &rarr; select **`user`** &rarr; click **Assign**.

### Option B: Automated 1-Line PowerShell Onboarding Script
Run this script to onboard any customer into `wallet-realm` via Keycloak's REST API:

```powershell
param(
  [string]$Username = "user1",
  [string]$Password = "Password123!",
  [string]$Email = "user1@example.com"
)

# Fetch master admin token
$adminToken = (Invoke-RestMethod -Uri "http://localhost:8085/realms/master/protocol/openid-connect/token" `
  -Method Post `
  -Body "client_id=admin-cli&grant_type=password&username=admin&password=admin" `
  -ContentType "application/x-www-form-urlencoded").access_token

# Create user with permanent password
$newUser = @{
  username = $Username
  enabled = $true
  email = $Email
  credentials = @(@{ type = "password"; value = $Password; temporary = $false })
} | ConvertTo-Json

Invoke-RestMethod -Uri "http://localhost:8085/admin/realms/wallet-realm/users" `
  -Method Post `
  -Headers @{ Authorization = "Bearer $adminToken" } `
  -Body $newUser `
  -ContentType "application/json"

# Assign realm role 'user'
$userObj = Invoke-RestMethod -Uri "http://localhost:8085/admin/realms/wallet-realm/users?username=$Username" `
  -Headers @{ Authorization = "Bearer $adminToken" }
$roleObj = Invoke-RestMethod -Uri "http://localhost:8085/admin/realms/wallet-realm/roles/user" `
  -Headers @{ Authorization = "Bearer $adminToken" }

Invoke-RestMethod -Uri "http://localhost:8085/admin/realms/wallet-realm/users/$($userObj[0].id)/role-mappings/realm" `
  -Method Post `
  -Headers @{ Authorization = "Bearer $adminToken" } `
  -Body (@($roleObj) | ConvertTo-Json) `
  -ContentType "application/json"

Write-Host "Customer '$Username' successfully created and assigned 'user' role!" -ForegroundColor Green
```

---

## 6. How to Manage Roles & Hierarchies (RBAC)

### Adding a Realm Role
1. In the left navigation menu, click **Realm roles** (under *Manage*).
2. Click **Create role**.
3. Fill in:
   * **Role name**: e.g., `auditor` or `compliance-officer`
   * **Description**: e.g., `Read-only access to audit logs and double-entry ledger`
4. Click **Save**.

### Creating Composite Roles (Role Hierarchies)
To make a high-privilege role inherit lower-privilege permissions automatically (for example, making `admin` inherit all `user` capabilities):
1. In **Realm roles**, click on the parent role (e.g., `admin`).
2. In the top-right **Action** dropdown, select **Add associated roles**.
3. Select the child role (e.g., `user`).
4. Click **Assign**. Any user assigned `admin` will now automatically possess `user` role capabilities.

---

## 7. How to Manage Clients (Microservices & Frontends)

A **Client** in Keycloak represents an application (API Gateway, Web frontend, mobile app, or backend service) that can request authentication or receive tokens.

### Configuration for `wallet-api`:
* **Client ID**: `wallet-api`
* **Client Authentication**: `ON` (Confidential client requiring client secret)
* **Direct Access Grants Enabled**: `ON` (Allows Direct Grant `POST /token` using username + password)
* **Service Accounts Enabled**: `ON` (Allows Machine-to-Machine `client_credentials` grant)
* **Standard Flow Enabled**: `ON` (Allows Authorization Code flow with PKCE for UI frontends)

### Accessing the Client Secret:
1. Navigate to **Clients** &rarr; click **`wallet-api`**.
2. Click the **Credentials** tab.
3. Click the copy icon next to **Client secret**. Ensure this matches `KEYCLOAK_CLIENT_SECRET` in `.env`:
   ```env
   KEYCLOAK_CLIENT_ID=wallet-api
   KEYCLOAK_CLIENT_SECRET=wallet-client-secret-12345
   ```

---

## 8. Verifying & Testing with cURL / PowerShell / Bruno

### A. Login & Token Retrieval via Keycloak Direct Grant
Execute this command in your terminal to obtain an asymmetric RS256 JWT access token directly from Keycloak:

```bash
curl -X POST http://localhost:8085/realms/wallet-realm/protocol/openid-connect/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=password" \
  -d "client_id=wallet-api" \
  -d "client_secret=wallet-client-secret-12345" \
  -d "username=admin" \
  -d "password=admin123" \
  -d "scope=openid wallet:read wallet:transfer"
```

**Expected Response**:
```json
{
  "access_token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9...",
  "expires_in": 900,
  "refresh_expires_in": 1800,
  "refresh_token": "eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9...",
  "token_type": "Bearer",
  "scope": "openid wallet:read wallet:transfer"
}
```

### B. Calling API Gateway Login Endpoint
The API Gateway forwards credentials to `auth-service`, which verifies against Keycloak and returns the token pair:

```powershell
Invoke-RestMethod -Uri "http://localhost:8080/api/v1/auth/login" `
  -Method Post `
  -Body (@{ username = "admin"; password = "admin123" } | ConvertTo-Json) `
  -ContentType "application/json"
```

### C. Calling Protected Microservice via Gateway
Use the `access_token` returned above to make an authorized call through the API Gateway:

```bash
curl -X GET http://localhost:8080/api/v1/cluster/status \
  -H "Authorization: Bearer <paste_access_token_here>"
```

### D. Refreshing an Expired Access Token
```bash
curl -X POST http://localhost:8085/realms/wallet-realm/protocol/openid-connect/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=refresh_token" \
  -d "client_id=wallet-api" \
  -d "client_secret=wallet-client-secret-12345" \
  -d "refresh_token=<paste_refresh_token_here>"
```

---

## 9. Persisting Changes to Version Control (GitOps)

When running locally in Docker, Keycloak runs in development mode (`start-dev --import-realm`). 
If the container or volume is removed, modifications created exclusively in the Keycloak Web Console will be lost on container restart unless saved to the realm export file.

### Exporting Realm Configurations from Keycloak
To capture changes made via the Web Console and commit them to Git:

1. **Option A: Export via Docker CLI**
   ```powershell
   docker exec wallet_keycloak /opt/keycloak/bin/kc.sh export `
     --realm wallet-realm `
     --file /opt/keycloak/data/import/wallet-realm-export.json `
     --users skip
   
   docker cp wallet_keycloak:/opt/keycloak/data/import/wallet-realm-export.json ./deploy/keycloak/wallet-realm-realm.json
   ```

2. **Option B: Export via Keycloak Admin Console**
   * Switch to **`wallet-realm`** in the top-left dropdown.
   * Click **Realm settings** in the left menu.
   * In the top-right **Action** dropdown, select **Export**.
   * Toggle **Export clients**: `ON`, **Export groups and roles**: `ON`, **Export users**: `OFF` (to preserve clean production state).
   * Click **Export**.
   * Replace [`deploy/keycloak/wallet-realm-realm.json`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/deploy/keycloak/wallet-realm-realm.json) with the downloaded file and commit to Git.

---

## 10. Production Hardening & Best Practices

1. **Production Mode Execution**:
   In production, do not run with `start-dev`. Run `kc.sh start` with a persistent database backend (PostgreSQL) instead of the in-memory H2 database.
   ```bash
   kc.sh start --optimized --db=postgres --db-url=jdbc:postgresql://postgres:5432/keycloak
   ```
2. **Reverse Proxy & TLS**:
   Configure `KC_PROXY_HEADERS=xforwarded` when placing Keycloak behind an NGINX, Traefik, or Ingress Controller TLS terminator.
3. **Secret Rotation**:
   Never commit production `KEYCLOAK_CLIENT_SECRET` or `KEYCLOAK_ADMIN_PASSWORD` to source code. Store them in HashiCorp Vault, AWS Secrets Manager, or Kubernetes Secrets.
4. **Token Expiry**:
   Keep access token lifespans short (15 minutes). Rely on refresh tokens with sliding session timeouts for user experience without compromising security.

---

## 11. Troubleshooting Quick Reference

| Issue | Cause | Fix |
| :--- | :--- | :--- |
| **`wallet-realm` missing on startup** | Realm file not mounted or `--import-realm` missing | Verify volume mount `./deploy/keycloak/wallet-realm-realm.json:/opt/keycloak/data/import/wallet-realm-realm.json:ro` and command `start-dev --import-realm`. |
| **`401 Unauthorized: Invalid user credentials`** | Password incorrect or user not yet onboarded | Verify user exists in `wallet-realm` &rarr; **Users**; verify password under **Credentials**; ensure **Temporary** is toggled **OFF**. |
| **`Account is not fully set up`** | First Name or Last Name left blank during user creation | In Keycloak 26 User Profile, `First name` and `Last name` are mandatory. Go to **Users** &rarr; select user &rarr; fill in **First name** and **Last name** &rarr; click **Save**. |
| **`403 Forbidden: missing scope`** | Requested scope was not assigned to client | Open Client `wallet-api` &rarr; **Client scopes** tab &rarr; add the missing scope as Default or Optional. |
| **`403 Forbidden: subject does not own wallet`** | IDOR ownership check failed | User subject (`preferred_username`) must match the wallet owner or user must possess the `admin` role. |
| **Port 8085 unreachable** | Keycloak container still booting or stopped | Keycloak takes ~15 seconds to augment Quarkus; run `docker ps` and check logs with `docker logs -f wallet_keycloak`. |
| **`invalid_client` on token endpoint** | Client secret mismatch | Check **Credentials** tab in `wallet-api` client; verify matching `KEYCLOAK_CLIENT_SECRET` in `.env` and `docker-compose.auth.yml`. |
| **`unsupported token algorithm "HS256" (expected RS256)`** | Gateway or auth-service mismatch | Keycloak issues RS256 tokens; ensure `auth-service` has fetched Keycloak JWKS certs on `:8085`. |
