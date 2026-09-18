# Keycloak Identity Provider — Administrator & Developer Guide
### Global Multi-Currency Digital Wallet Platform

---

## 1. Overview & Architecture

The Global Wallet platform uses **Keycloak** as its OpenID Connect (OIDC) / OAuth 2.0 Identity Provider. It handles:
- User identity lifecycle, password hashing, and credentials
- Role-Based Access Control (RBAC) via Realm and Client roles
- Fine-grained permission scopes (`wallet:read`, `wallet:transfer`, `cluster:admin`, `ledger:audit`)
- Standard OIDC Discovery and public key sets (JWKS) for asymmetric RS256 token verification

```
   ┌───────────────────┐               ┌────────────────────────┐
   │ Client / Frontend │               │   Keycloak (:8085)     │
   │    or Postman     │─── Login ───► │  Realm: wallet-realm   │
   └─────────┬─────────┘               └───────────┬────────────┘
             │                                     │
       Bearer JWT Token                      JWKS Certs
             │                                     │
             ▼                                     ▼
   ┌───────────────────┐               ┌────────────────────────┐
   │    API Gateway    │─── gRPC ────► │      Auth Service      │
   │      (:8080)      │ ValidateToken │        (:50054)        │
   └───────────────────┘               └────────────────────────┘
```

---

## 2. Accessing the Keycloak Portals

| Portal | URL | Purpose |
|--------|-----|---------|
| **Admin Console** | [http://localhost:8085/admin](http://localhost:8085/admin) | Full management of realms, users, roles, and clients |
| **Account Console** | [http://localhost:8085/realms/wallet-realm/account](http://localhost:8085/realms/wallet-realm/account) | Self-service portal for users (manage profile, change password) |
| **OIDC Discovery** | [http://localhost:8085/realms/wallet-realm/.well-known/openid-configuration](http://localhost:8085/realms/wallet-realm/.well-known/openid-configuration) | Public metadata (token endpoints, JWKS URI) |
| **JWKS Public Keys** | [http://localhost:8085/realms/wallet-realm/protocol/openid-connect/certs](http://localhost:8085/realms/wallet-realm/protocol/openid-connect/certs) | Public certificates used by `auth-service` for signature verification |

### Master Admin Login Credentials
* **Username**: `admin`
* **Password**: `admin`

---

## 3. Switching to `wallet-realm`

When you first log in, you will be on the `master` realm.

1. Look at the **top-left corner** of the Keycloak console.
2. Click on the dropdown menu displaying **`master`**.
3. Select **`wallet-realm`** from the list.

> **Important**: Always verify that you are operating inside **`wallet-realm`**, not `master`. All wallet clients, users, and roles belong to `wallet-realm`.

---

## 4. How to Add a User

### Step 1: Create the User Profile
1. In the left navigation menu, click **Users** (under the *Manage* section).
2. Click the blue **Add user** button.
3. Fill in the user profile:
   * **Username**: e.g., `bob`
   * **Email**: e.g., `bob@wallet.local`
   * **First Name**: e.g., `Bob`
   * **Last Name**: e.g., `Builder`
   * **Email verified**: Toggle to **On** (avoids mandatory email confirmation in dev).
   * **Enabled**: Ensure toggle is **On**.
4. Click **Create** (or **Save**).

### Step 2: Set the User Password
1. Click on the **Credentials** tab at the top of the user details page.
2. Click **Set password**.
3. In the modal:
   * **Password**: e.g., `bob123`
   * **Password confirmation**: `bob123`
   * **Temporary**: Toggle to **OFF** *(Crucial: if left ON, the user cannot authenticate via APIs without first changing their password in the UI)*.
4. Click **Save**, then confirm by clicking **Save password**.

### Step 3: Assign Roles to the User
1. Click on the **Role mapping** tab at the top of the user details page.
2. Click **Assign role**.
3. Select the desired realm role (e.g. `user` or `admin`).
4. Click **Assign**.

The user is now fully provisioned and can immediately log in via API or frontend.

---

## 5. How to Add a Role

Roles define permissions across the platform (e.g., standard users, auditors, cluster administrators).

### Adding a Realm Role
1. In the left navigation menu, click **Realm roles** (under the *Manage* section).
2. Click **Create role**.
3. Fill in:
   * **Role name**: e.g., `auditor` or `compliance-officer`
   * **Description**: e.g., `Read-only access to audit logs and double-entry ledger`
4. Click **Save**.

### Creating Role Hierarchies (Composite Roles)
To make a role automatically inherit all permissions of another role (for example, making `admin` inherit all `user` capabilities):
1. In **Realm roles**, click on the parent role (e.g., `admin`).
2. In the **Action** dropdown at the top right, select **Add associated roles** (or navigate to the *Associated roles* tab).
3. Check the child role (e.g., `user`).
4. Click **Assign**. Now any user with `admin` automatically has `user` privileges.

---

## 6. How to Add a Client (Application / Microservice)

A **Client** in Keycloak represents an application (API, frontend web app, mobile app, or backend service) that can request authentication or receive tokens.

### Step 1: General Settings
1. In the left navigation menu, click **Clients** (under the *Manage* section).
2. Click **Create client**.
3. **Client type**: `OpenID Connect`
4. **Client ID**: e.g., `wallet-mobile-app` or `partner-banking-service`
5. **Name**: Descriptive display name (e.g., `Global Wallet Mobile App`)
6. Click **Next**.

### Step 2: Capability Config
1. **Client authentication**:
   * Toggle **On** for backend services / confidential clients that can safely store a secret (e.g., microservices, server-side APIs).
   * Toggle **Off** for public clients (e.g., Single Page Apps (React/Vue), Mobile Apps).
2. **Authentication flow**:
   * Check **Standard flow**: Enables OAuth 2.0 Authorization Code flow with PKCE (standard for Web/Mobile UI).
   * Check **Direct access grants**: Enables Resource Owner Password Credentials flow (`username` + `password` via `/token` endpoint — used by automated test scripts and CLI tools).
   * Check **Service accounts roles**: Enables Machine-to-Machine `client_credentials` grant.
3. Click **Next**.

### Step 3: Login Settings
1. **Valid redirect URIs**: Add your frontend URL or wildcard for development:
   ```text
   http://localhost:3000/*
   http://localhost:8080/*
   ```
2. **Web origins**: Set to `+` (inherits redirect URIs) or `*` for development CORS support.
3. Click **Save**.

### Step 4: Obtaining Client Credentials (for Confidential Clients)
If you enabled **Client authentication**:
1. Click on the **Credentials** tab of the newly created client.
2. You will see the generated **Client secret**.
3. Click the copy icon to use in your `.env` or application configuration:
   ```env
   KEYCLOAK_CLIENT_ID=your-client-id
   KEYCLOAK_CLIENT_SECRET=your-copied-secret
   ```

---

## 7. How to Add Custom Client Scopes

Custom scopes (such as `wallet:read`, `wallet:transfer`, `cluster:admin`) allow fine-grained OAuth2 scope-based authorization.

### Step 1: Create the Client Scope
1. In the left navigation menu, click **Client scopes** (under the *Manage* section).
2. Click **Create client scope**.
3. Enter:
   * **Name**: e.g., `wallet:freeze`
   * **Description**: e.g., `Ability to freeze and unfreeze customer wallets`
   * **Type**: `Default` (automatically attached to all tokens) or `Optional` (attached only if explicitly requested in `scope` parameter)
   * **Protocol**: `OpenID Connect`
   * **Include in token scope**: Toggle **On**
4. Click **Save**.

### Step 2: Attach the Scope to a Client
1. In the left navigation menu, click **Clients**.
2. Select your client (e.g., `wallet-api`).
3. Click the **Client scopes** tab.
4. Click **Add client scope**.
5. Check your new scope (e.g. `wallet:freeze`).
6. Select **Default** or **Optional**, then click **Add**.

---

## 8. Verifying & Testing with cURL / Postman

### A. Login & Token Retrieval (Direct Access Grant)
Execute this command in your terminal to obtain an RS256 JWT access token:

```bash
curl -X POST http://localhost:8085/realms/wallet-realm/protocol/openid-connect/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=password" \
  -d "client_id=wallet-api" \
  -d "client_secret=wallet-client-secret-12345" \
  -d "username=alice" \
  -d "password=alice123" \
  -d "scope=openid wallet:read wallet:transfer"
```

**Expected Response**:
```json
{
  "access_token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9...",
  "expires_in": 900,
  "refresh_expires_in": 1800,
  "refresh_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "token_type": "Bearer",
  "scope": "openid wallet:read wallet:transfer"
}
```

### B. Calling API Gateway with Keycloak Token
Use the `access_token` returned above to make an authorized call through the API Gateway:

```bash
# Read Alice's wallet
curl -X GET http://localhost:8080/api/v1/wallets?id=alice \
  -H "Authorization: Bearer <paste_access_token_here>"
```

### C. Refreshing an Expired Access Token
```bash
curl -X POST http://localhost:8085/realms/wallet-realm/protocol/openid-connect/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=refresh_token" \
  -d "client_id=wallet-api" \
  -d "client_secret=wallet-client-secret-12345" \
  -d "refresh_token=<paste_refresh_token_here>"
```

---

## 9. Persisting Changes to Version Control

When running in local Docker, Keycloak runs in development mode (`start-dev --import-realm`). 
If the container volume is removed (`docker compose down -v`), custom changes created exclusively in the web UI will be lost unless saved to the realm export file.

To permanently persist new users, clients, or roles into your Git repository:

1. Update [`deploy/keycloak/wallet-realm-realm.json`](file:///c:/workarea/personal/After-equifax/global-wallet-microservices/deploy/keycloak/wallet-realm-realm.json).
2. Add your new role under `"roles": { "realm": [ ... ] }`.
3. Add your new user under `"users": [ ... ]`.
4. Next time the container starts up, your configurations are automatically imported on boot.

---

## 10. Troubleshooting Quick Reference

| Issue | Cause | Fix |
|-------|-------|-----|
| **`401 Unauthorized: Invalid user credentials`** | Password incorrect or username typo | Verify password in Admin Console under user **Credentials** tab; ensure **Temporary** is toggled **OFF**. |
| **`403 Forbidden: missing scope`** | Requested scope was not assigned to client | Open Client &rarr; **Client scopes** tab &rarr; add the missing scope as Default or Optional. |
| **`403 Forbidden: subject does not own wallet`** | IDOR ownership check failed | User subject (`preferred_username`) must match the wallet owner or user must possess the `admin` role. |
| **Port 8085 unreachable** | Container not running | Run `docker compose up -d keycloak` and inspect status with `docker logs wallet_keycloak`. |
| **`invalid_client` on token endpoint** | Client secret mismatch | Check **Credentials** tab in `wallet-api` client; verify matching `KEYCLOAK_CLIENT_SECRET` in `.env`. |
