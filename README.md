# ymca-wellness-dapp

dApp server for the YMCA Wellness Café reward flow. Runs on top of the
Rubix v2 HTTP API (`rubixgoplatform`, `release-v1` branch).

- **Per-admin queue.** Each admin DID gets its own buffered channel and
  one worker goroutine, so reward transfers serialize per admin (because
  the admin's signing key is one resource) but parallelize across admins.
- **Unified Rubix tx.** Every on-chain operation goes through
  `POST /rubix/v1/tx` + `POST /rubix/v1/signature`. `Sign()` blocks until
  quorum signing completes — no callbacks, no polling.
- **Postgres source of truth.** Admins, contracts, activities, transfer
  history, and user→admin mappings all live in Postgres. Provisioning is
  via API; restarts are safe.

---

## Table of contents

1.  [Architecture](#1-architecture)
2.  [Prerequisites](#2-prerequisites)
3.  [Setup](#3-setup)
4.  [Run it](#4-run-it)
5.  [Provision admins](#5-provision-admins)
6.  [Deploy contracts](#6-deploy-contracts)
7.  [Endpoint reference](#7-endpoint-reference)
8.  [Database schema](#8-database-schema)
9.  [Configuration](#9-configuration)
10. [Authentication](#10-authentication)
11. [Troubleshooting](#11-troubleshooting)
12. [Project layout](#12-project-layout)

---

## 1. Architecture

```
  HTTP client (Postman / curl)
          │
          ▼
  ┌──────────────────────┐         per-admin queue
  │  Gin server (:9000)  │────►  buffered chan ──► worker goroutine
  │   internal/server    │           │                  │
  └──────────┬───────────┘           │                  │
             │                       │                  ▼
             ▼                       │        ┌──────────────────┐
  ┌──────────────────────┐           │        │ service layer    │
  │ service layer        │◄──────────┘        │ (Rubix + DB)     │
  │ (Rubix + DB)         │                    └────────┬─────────┘
  └──────────┬───────────┘                             │
             │                                         ▼
             ▼                                ┌──────────────────┐
  ┌──────────────────────┐                    │  Rubix v2 HTTP   │
  │  PostgreSQL          │                    │  (release-v1)    │
  └──────────────────────┘                    └──────────────────┘
```

Reward transfers are async (202 Accepted, status polled via
`/api/rewards/status/:request_id`). Every other endpoint is synchronous
— the handler blocks for the full Rubix `Sign()` round-trip (~1.5 s on a
healthy local testnet, longer if quorum is sluggish).

---

## 2. Prerequisites

- Go 1.22+
- PostgreSQL 14+
- `rubixgoplatform` checked out on the `release-v1` branch and built
- For each admin you provision, a Rubix node running on the corresponding
  port (the dApp talks to `http://localhost:<node_port>` per admin)

---

## 3. Setup

### 3.1 Build

```bash
cd /path/to/ymca-wellness-dapp
go mod tidy
go build ./...
```

### 3.2 Postgres

```bash
createdb -U postgres ymca_wellness_cafe_v2
for f in migrations/00*.sql; do
  psql -U postgres -d ymca_wellness_cafe_v2 -f "$f"
done

# Verify
psql -U postgres -d ymca_wellness_cafe_v2 -c '\dt'
# Expect: admins, admin_contracts, activities, transfer_status, user_admins
```

### 3.3 Environment

```bash
cp .env.example .env
# Edit .env if your Postgres credentials differ from defaults.
```

See [§9 Configuration](#9-configuration) for the full env var list.

### 3.4 Bootstrap each Rubix admin (manual, one-time)

For every admin you plan to provision, the underlying Rubix node needs
test RBT and a minted ytoken FT. The dApp does **not** do this — it's a
one-time admin bootstrap done directly against the Rubix node.

```bash
export NODE=http://localhost:20000
export DID=<admin DID, after provisioning via /api/admins/setup>
export PASS=mypassword

# Generate test RBT (async: PostTx → Sign)
RBT_REQ=$(curl -s -X POST $NODE/api/generate-local-rbt \
  -H "Content-Type: application/json" \
  -d "{\"did\":\"$DID\",\"number_of_tokens\":100,\"start_index\":0}" \
  | jq -r '.result.id')
curl -s -X POST $NODE/rubix/v1/signature \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"$RBT_REQ\",\"password\":\"$PASS\"}"

# Mint ytoken FT
FT_REQ=$(curl -s -X POST $NODE/rubix/v1/fts/mint \
  -H "Content-Type: application/json" \
  -d "{\"did\":\"$DID\",\"ft_name\":\"ytoken\",\"ft_count\":10000,\"token_count\":10,\"ft_num_start_index\":0}" \
  | jq -r '.result.id')
curl -s -X POST $NODE/rubix/v1/signature \
  -H "Content-Type: application/json" \
  -d "{\"id\":\"$FT_REQ\",\"password\":\"$PASS\"}"
```

Repeat for every admin. The FT name (`ytoken`) must match the
`FT_NAME` env var.

---

## 4. Run it

```bash
go run ./cmd/server
# warning: no admins configured; provision via POST /api/admins/setup
# ymca-wellness-dapp listening on :9000 (admins=0, queue_buf=1000)
```

Smoke test:

```bash
curl -s http://localhost:9000/api/health
# {"admins":0,"ft_name":"ytoken","status":"ok"}
```

`admins=0` is expected on first boot — the dApp has no `[[admin]]` config
file. Provisioning happens through the API ([§5](#5-provision-admins)).

---

## 5. Provision admins

`POST /api/admins/setup` creates one or more admin DIDs on the Rubix
nodes you have running, persists them to the `admins` table, and writes
per-admin metadata to `dids/<did>.json`.

```bash
curl -s -X POST http://localhost:9000/api/admins/setup \
  -H "Content-Type: application/json" \
  -d '{
    "admins": [
      {"node_port": "20000"},
      {"node_port": "20001"},
      {"node_port": "20002"}
    ]
  }' | jq
```

Response:

```json
{
  "status": true,
  "data": {
    "admins": [
      {"node_port": "20000", "did": "bafybmi...A", "success": true},
      {"node_port": "20001", "did": "bafybmi...B", "success": true},
      {"node_port": "20002", "did": "bafybmi...C", "success": true}
    ],
    "summary": {"requested": 3, "created": 3, "failed": 0}
  }
}
```

For each admin, the endpoint:
1. Calls `POST /rubix/v1/dids/create` to mint the DID.
2. Calls `POST /rubix/v1/dids/<did>/register` then `Sign` (best-effort —
   logs on failure, does not abort).
3. INSERTs the row into `admins` (defaults: `password='mypassword'`).
4. Writes `dids/<did>.json` (mode 0600, contains password — **never
   commit this directory**).
5. After all admins processed, refreshes the in-memory admin map so the
   new DIDs are immediately usable on subsequent requests.

You can re-call `setup` with new ports later to add more admins
incrementally; existing rows are unaffected.

---

## 6. Deploy contracts

Each admin needs three contracts: `reward`, `add_activity`, `add_admin`.
The dApp uses these for the reward transfer, activity registration, and
admin onboarding flows respectively. The compiled WASM + Rust source for
each kind lives under `artifacts/<kind>/`:

```
artifacts/
├── reward/
│   ├── reward_transfer.wasm
│   └── lib.rs
├── add_activity/
│   ├── activity_contract.wasm
│   └── lib.rs
└── add_admin/
    ├── add_admin_contract.wasm
    └── lib.rs
```

Run `scripts/deploy-contracts.sh` to generate + sign each contract for
every admin in the `admins` table:

```bash
./scripts/deploy-contracts.sh
# [admin 1/3: bafybmi...A port=20000]
#   [reward]       generating... token=Qm...
#                  deploying... signing... done.
#   [add_activity] ...
#   [add_admin]    ...
# Summary: requested=9 deployed=9 skipped=0 failed=0
# Logs: ./logs/deploy-2026-05-08-...log
```

Per (admin, kind) pair the script runs three Rubix calls:

1. `POST /rubix/v1/smart_contracts/generate` (multipart, sync) — returns
   the smart contract token.
2. `POST /rubix/v1/tx` with `data: "deploy"` — returns a request id.
3. `POST /rubix/v1/signature` — finalizes the deploy.

Then UPSERTs `(admin_did, kind, contract_hash)` into `admin_contracts`
and updates `contracts.json`. Idempotent: pairs already in the DB are
skipped. Per-(admin, kind) failures are logged and don't stop the run.

Flags:

- `--artifacts-dir <path>` — override the default `./artifacts`.

The Rust source under `contracts/` (committed) is what produces the
`.wasm` files in `artifacts/`. Rebuilding is out of scope for this
README — see the per-contract Cargo project for instructions.

---

## 7. Endpoint reference

Base URL: `http://localhost:9000`

All responses use a common envelope:

```json
{ "status": true,  "message": "...", "data": { ... } }   // success
{ "status": false, "error":   "...", "message": "..." }  // error
```

HTTP status mapping:

- **400** — request validation (missing/invalid fields)
- **404** — not found (unknown ID, unmapped DID, missing contract)
- **502** — Rubix node returned an API error
- **503** — admin queue full (only `/api/rewards/transfer`)
- **500** — DB error or other

### Summary table

| Method | Path | Sync? | Purpose |
|---|---|---|---|
| GET  | `/api/health` | sync | Liveness (no auth) |
| POST | `/api/auth/login` | sync | Exchange email+password for an access+refresh pair (no auth) |
| POST | `/api/auth/refresh` | sync | Rotate refresh; returns new pair (no auth) |
| POST | `/api/auth/logout` | sync | Revoke a refresh token, or all for current user |
| GET  | `/api/auth/me` | sync | Authenticated user profile |
| POST | `/api/admins/setup` | sync (~1s/admin) | Provision N admin DIDs |
| GET  | `/api/admins/:admin_did/users/count` | sync | Count of mapped users |
| POST | `/api/rewards/transfer` | **async** | Queue reward transfer |
| GET  | `/api/rewards/status/:request_id` | sync | Poll transfer status |
| GET  | `/api/rewards/user/:user_did` | sync | Reward history (recipient) |
| GET  | `/api/rewards/admin/:admin_did` | sync | Reward history (sender) |
| GET  | `/api/queue/metrics` | sync | Per-admin queue depths |
| POST | `/api/activity/add` | sync (~Sign) | Add activity definition |
| POST | `/api/admin/add` | sync (~Sign) | Append to add_admin chain |
| POST | `/api/create-did-with-pubkey` | sync | Create user DID + map to admin |
| GET  | `/api/users/:user_did/admin` | sync | Look up user's admin |
| POST | `/api/deploy-contract` | sync (~Sign) | Deploy contract via dApp |
| POST | `/api/execute-contract` | sync (~Sign) | Generic contract invocation |
| GET  | `/api/contracts/:admin_did/:kind/chain` | sync | Chain audit by kind |
| GET  | `/api/contracts/by-hash/:contract_hash/chain` | sync | Chain audit by hash |

### 7.1 GET `/api/health`

Liveness probe. Always returns 200 if the server is up. Reports admin
count from the in-memory map and the configured FT name.

```bash
curl http://localhost:9000/api/health
# {"admins":3,"ft_name":"ytoken","status":"ok"}
```

### 7.2 POST `/api/admins/setup`

See [§5](#5-provision-admins).

### 7.3 GET `/api/admins/:admin_did/users/count`

Returns the number of users mapped to this admin in `user_admins`.
Validates that the admin exists; 404 if not.

```bash
curl http://localhost:9000/api/admins/bafybmi.../users/count
# {"status":true,"data":{"admin_did":"bafybmi...","user_count":7}}
```

### 7.4 POST `/api/rewards/transfer`

Queues a reward transfer. Returns 202 immediately with a `request_id`
for status polling.

```json
{
  "user_did":    "bafybmi...recipient",
  "admin_did":   "bafybmi...sender (must be in admins table)",
  "activity_id": ["ACT-001", "ACT-002"]
}
```

```bash
curl -X POST http://localhost:9000/api/rewards/transfer \
  -H "Content-Type: application/json" \
  -d '{...}'
# 202 → {"status":true,"message":"Transfer request queued for processing","data":{"request_id":"<uuid>"}}
```

The worker reads `reward_points` from `activities` for each
`activity_id`, sums them, and submits an FT+SC transaction to Rubix.
Errors are recorded asynchronously in `transfer_status` — poll status to
see them.

### 7.5 GET `/api/rewards/status/:request_id`

Fetches the `transfer_status` row.

```json
{
  "status": true,
  "data": {
    "request_id":     "<uuid>",
    "transaction_id": "<64-char hex, empty until status=success>",
    "kind":           "reward",
    "admin_did":      "...",
    "user_did":       "...",
    "activity_ids":   ["ACT-001","ACT-002"],
    "reward_points":  25,
    "contract_hash":  "Qm...",
    "status":         "queued|processing|success|failed",
    "message":        "transferred 25 ytoken to ...",
    "error_details":  "",
    "created_at":     "2026-05-08T12:38:22Z",
    "updated_at":     "2026-05-08T12:38:25Z"
  }
}
```

404 if `request_id` is unknown.

### 7.6 GET `/api/rewards/user/:user_did`

Returns successful reward transfers received by the given user, newest
first. Empty list (200, count=0) if none.

```bash
curl http://localhost:9000/api/rewards/user/bafybmi...
# {"status":true,"data":{"user_did":"...","count":2,"rewards":[{"date":"...","activity_ids":[...],"reward_points":25,"transaction_id":"...","user_did":"...","admin_did":"..."}, ...]}}
```

### 7.7 GET `/api/rewards/admin/:admin_did`

Same as 7.6 but filters by the *sender* admin.

### 7.8 GET `/api/queue/metrics`

Per-admin queue depth (in-memory; resets on restart).

```bash
curl http://localhost:9000/api/queue/metrics
# {"status":true,"data":{"total_admins":3,"total_queued":5,"admin_queues":[{"admin_did":"bafybmi...","queue_size":2}, ...]}}
```

### 7.9 POST `/api/activity/add`

Records an activity on-chain (via the admin's `add_activity` contract)
AND inserts a row into `activities`. The reward transfer worker reads
`reward_points` from this table when summing payouts.

```json
{
  "admin_did":     "bafybmi...",
  "activity_id":   "ACT-001",
  "reward_points": 10,
  "description":   "30 min cardio"
}
```

Requires the admin to have an `add_activity` contract deployed.

### 7.10 POST `/api/admin/add`

Appends a new admin DID to an existing admin's `add_admin` contract
chain. On-chain only; does not write to the `admins` table (use
`/api/admins/setup` for that).

```json
{
  "existing_admin_did": "bafybmi...A",
  "new_admin_did":      "bafybmi...B"
}
```

### 7.11 POST `/api/create-did-with-pubkey`

Creates a user DID on the admin's Rubix node from a real (uncompressed,
130-char hex) secp256k1 public key. As a side effect, records
`(user_did → admin_did)` in `user_admins`.

```json
{
  "admin_did":  "bafybmi...",
  "public_key": "04<128 hex chars>"
}
```

```bash
# Generate a key locally:
PRIV=$(mktemp); openssl ecparam -name secp256k1 -genkey -noout -out "$PRIV" 2>/dev/null
PUBKEY=$(openssl ec -in "$PRIV" -pubout -conv_form uncompressed -text -noout 2>/dev/null \
  | awk '/pub:/{flag=1;next}/ASN1 OID/{flag=0}flag' | tr -d ' :\n')
rm -f "$PRIV"
echo "${#PUBKEY}"  # must print 130

curl -X POST http://localhost:9000/api/create-did-with-pubkey \
  -H "Content-Type: application/json" \
  -d "{\"admin_did\":\"...\",\"public_key\":\"$PUBKEY\"}"
# 200 → {"status":true,"data":{"did":"bafybmi..."}}
```

`user_admins` upsert is best-effort: a DB write failure is logged but
the request still returns 200 with the DID.

### 7.12 GET `/api/users/:user_did/admin`

Returns the admin DID mapped to a user. 404 if no mapping exists.

```bash
curl http://localhost:9000/api/users/bafybmi.../admin
# {"status":true,"data":{"user_did":"...","admin_did":"..."}}
```

### 7.13 POST `/api/deploy-contract`

Deploys a contract through the dApp. **Note:** the script
`scripts/deploy-contracts.sh` is the recommended path for batch
deployment because it follows the proven 3-call flow (generate → tx →
sign). This endpoint uses an older 2-call flow that may not produce
identical contract behavior.

```json
{
  "deployer_did": "bafybmi...",
  "kind":         "reward",
  "wasm_path":    "/abs/path/to/contract.wasm",
  "lib_path":     "/abs/path/to/lib.rs"
}
```

### 7.14 POST `/api/execute-contract`

Generic contract invocation. `contract_input` is forwarded verbatim as
the SmartContract leg's `Data` field.

```json
{
  "contract_hash":  "Qm...",
  "executor_did":   "bafybmi... (must be in admins table)",
  "contract_input": "{\"action\":\"foo\",\"x\":1}"
}
```

### 7.15 GET `/api/contracts/:admin_did/:kind/chain`

Chain audit for the contract registered under `(admin_did, kind)`.

```bash
curl http://localhost:9000/api/contracts/bafybmi.../reward/chain
# {"status":true,"data":{"admin_did":"...","kind":"reward","contract_hash":"Qm...","count":3,"chain":[{"transactionId":"...","initiator":"...","epoch":1778224102,"data":"..."}, ...]}}
```

### 7.16 GET `/api/contracts/by-hash/:contract_hash/chain`

Same payload as 7.15 but keyed by contract hash. The admin is resolved
from `admin_contracts`.

---

## 8. Database schema

Five tables, all in the `public` schema.

| Table | Purpose |
|---|---|
| `admins` | One row per provisioned admin (`did, node_port, password, created_at`) |
| `admin_contracts` | `(admin_did, contract_kind) → contract_hash` |
| `activities` | `(admin_did, activity_id) → reward_points + description` |
| `transfer_status` | Async ledger of every reward transfer |
| `user_admins` | `user_did → admin_did` mapping |

See `migrations/001_init.sql`, `002_admins.sql`, `003_user_admins.sql`
for full column definitions.

Useful one-shot queries:

```sql
-- Recent transfer activity
SELECT request_id, status, admin_did, user_did, reward_points, created_at
FROM transfer_status
ORDER BY created_at DESC LIMIT 20;

-- All contracts deployed per admin
SELECT admin_did, contract_kind, contract_hash, deployed_at
FROM admin_contracts ORDER BY admin_did;

-- Failed transfers with error details
SELECT request_id, error_details, message
FROM transfer_status WHERE status = 'failed';

-- Reset everything for a clean test run
TRUNCATE admins, admin_contracts, activities, transfer_status, user_admins;
```

---

## 9. Configuration

All runtime config is sourced from environment variables (or `.env`).
There is no `config.toml`.

| Variable | Default | Notes |
|---|---|---|
| `SERVER_PORT` | `9000` | dApp HTTP port |
| `DB_HOST` | `localhost` | Postgres host |
| `DB_PORT` | `5432` | Postgres port |
| `DB_USER` | `postgres` | DB role |
| `DB_PASSWORD` | `postgres` | DB password |
| `DB_NAME` | `ymca_wellness_cafe_v2` | DB name |
| `DB_SSLMODE` | `disable` | TLS mode |
| `FT_NAME` | `ytoken` | FT name minted by each admin and used in reward transfers |
| `RUBIX_HTTP_TIMEOUT_SECONDS` | `120` | Per-request timeout to Rubix |
| `QUEUE_BUFFER_SIZE` | `1000` | Per-admin channel capacity |

Admins (DID, password, node port) are **not** in env — they live in the
`admins` table, populated by `/api/admins/setup`.

| Variable | Default | Notes |
|---|---|---|
| `JWT_PRIVATE_KEY_PATH` | `./keys/jwt_private.pem` | RS256 private key (PEM). Required. |
| `JWT_PUBLIC_KEY_PATH` | `./keys/jwt_public.pem` | RS256 public key (PEM). Required. |
| `ACCESS_TOKEN_TTL` | `15m` | Access JWT lifetime (Go duration). |
| `REFRESH_TOKEN_TTL` | `168h` | Refresh token lifetime (7 days). |
| `BOOTSTRAP_EMAIL` | — | First-operator email; seeded on first boot only if `auth_users` is empty. |
| `BOOTSTRAP_PASSWORD` | — | First-operator password (≥ 8 chars). |

---

## 10. Authentication

All endpoints except `GET /api/health`, `POST /api/auth/login`, and
`POST /api/auth/refresh` require a bearer token:

```
Authorization: Bearer <access_token>
```

### 10.1 Bootstrap (one-time)

1. Generate the RS256 keypair:

   ```bash
   mkdir -p keys
   openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
     -out keys/jwt_private.pem
   openssl rsa -in keys/jwt_private.pem -pubout -out keys/jwt_public.pem
   chmod 600 keys/jwt_private.pem
   ```

2. Set bootstrap credentials in `.env` *before* starting the server for
   the first time:

   ```bash
   BOOTSTRAP_EMAIL=ops@example.com
   BOOTSTRAP_PASSWORD=change-me-now
   ```

3. Start the server. On first boot, with `auth_users` empty, the
   bootstrap operator is created. The log line confirms it:

   ```
   bootstrap operator created: ops@example.com (id=...)
   ```

   Subsequent boots no-op (the table is no longer empty). You can
   unset `BOOTSTRAP_*` after the first successful boot.

### 10.2 Login

```bash
curl -X POST http://localhost:9000/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"ops@example.com","password":"change-me-now"}'
```

Response:

```json
{
  "access_token":  "eyJhbGciOiJSUzI1NiIs...",
  "refresh_token": "raw-base64url-...",
  "expires_in":    900,
  "token_type":    "Bearer"
}
```

Store the refresh token securely — it's only returned once per issue.

### 10.3 Calling protected endpoints

```bash
TOKEN=eyJhbGciOiJSUzI1NiIs...
curl http://localhost:9000/api/queue/metrics \
  -H "Authorization: Bearer $TOKEN"
```

### 10.4 Refresh

When the access token expires (401), exchange the refresh token for a
new pair:

```bash
curl -X POST http://localhost:9000/api/auth/refresh \
  -H "Content-Type: application/json" \
  -d '{"refresh_token":"raw-base64url-..."}'
```

Refresh tokens rotate on every use: the old token is revoked and a new
one is returned alongside the new access token. Presenting a
previously-revoked refresh token triggers a theft response — every
active refresh for that user is revoked and the request fails 401.

### 10.5 Logout

```bash
# Revoke a single refresh token (no auth required if you have the token).
curl -X POST http://localhost:9000/api/auth/logout \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"refresh_token":"raw-base64url-..."}'

# Revoke ALL active refresh tokens for the current user.
curl -X POST http://localhost:9000/api/auth/logout \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"all":true}'
```

### 10.6 Who am I?

```bash
curl http://localhost:9000/api/auth/me -H "Authorization: Bearer $TOKEN"
# {"status":true,"data":{"id":"...","email":"ops@example.com","role":"operator","created_at":"..."}}
```

### 10.7 Operational notes

- **Single operator, multiple admin DIDs.** The current model has one
  operator role that can act on behalf of any `admin_did`. Per-admin
  scoping is intentionally deferred — see the TODO in
  `internal/auth/middleware.go`.
- **Key rotation.** Replace the PEM files and restart. All existing
  access tokens become invalid immediately; refresh tokens become
  unusable on their next refresh attempt.
- **Adding more operators.** No public signup. Insert directly via SQL
  with a bcrypt-hashed password, or build a follow-up admin endpoint.
- **Resetting credentials.** `UPDATE auth_users SET password_hash = '...'
  WHERE email = '...'` with a fresh bcrypt hash, then optionally
  `DELETE FROM refresh_tokens WHERE user_id = ...` to force re-login.

---

## 11. Troubleshooting

### `warning: no admins configured` on startup
Expected on first boot. Provision via [§5](#5-provision-admins).

### `unknown admin_did "..."` from any endpoint
The DID isn't in the `admins` table. Either the admin was never
provisioned, or the in-memory map is stale (call any provisioning
endpoint to refresh).

### `admin has no reward contract`
Run `scripts/deploy-contracts.sh` for that admin, or call
`POST /api/deploy-contract` with `kind="reward"`.

### `activities resolved to zero reward points`
The `activity_id`s in the transfer request weren't found for that admin.
Check that `POST /api/activity/add` ran successfully against the same
admin; activities are scoped per-admin.

### `insufficient FTs: have 0, need N for ft_name=ytoken`
The admin's Rubix node has no spendable `ytoken` FTs. Run the manual
mint flow from [§3.4](#34-bootstrap-each-rubix-admin-manual-one-time).

### Transfer stuck on `queued` / `processing`
Either the per-admin worker is busy on an earlier job, or `Sign()` is
hanging waiting for quorum. Check the dApp's terminal logs and the
Rubix node logs. After `RUBIX_HTTP_TIMEOUT_SECONDS`, the worker marks
the row `failed`.

### Queue saturation
Inspect `GET /api/queue/metrics`. If one admin's `queue_size` is at
1000, the worker is wedged on a single Sign. Restart loses in-flight
jobs (no persistent queue) — prefer reducing `RUBIX_HTTP_TIMEOUT_SECONDS`
to fail faster.

### Port conflicts
- dApp: `SERVER_PORT` env var (default 9000).
- Rubix nodes: the `-port` flag passed to `rubixgoplatform run`, must
  match the `node_port` value in `admins` table.

---

## 12. Project layout

```
cmd/server/                Entry point (main.go)
internal/
  auth/                    JWT (RS256), bcrypt, refresh-token rotation, Gin middleware
  config/                  .env loader; in-memory adminByDID map (DB-backed)
  database/                pgxpool, models, queries (incl. auth_users / refresh_tokens)
  rubix/                   Rubix v2 HTTP client (DID, FT, contract, tx, sign)
  service/                 Business logic (orchestrates Rubix + DB)
  queue/                   Per-admin buffered channel + worker goroutine
  server/                  Gin engine, routes, handlers, DTOs
migrations/
  001_init.sql             admin_contracts, activities, transfer_status, user_did_registry
  002_admins.sql           admins table (replaces config.toml admins)
  003_user_admins.sql      user_admins; drops user_did_registry; backfills from transfer_status
  004_activities_tx_id.sql adds transaction_id column to activities
  005_auth.sql             auth_users, refresh_tokens (bearer-token authentication)
scripts/
  deploy-contracts.sh      generate + tx + sign for every (admin, kind)
artifacts/                 Built WASM + lib.rs per kind (committed)
contracts/                 Rust source projects (committed; target/ ignored)
dids/                      Per-admin DID metadata (NEVER committed)
logs/                      Deploy script logs (NEVER committed)
contracts.json             Per-machine admin → contract hash snapshot (NEVER committed)
```

### Rubix API surface used

| Endpoint | Used for |
|---|---|
| `POST /rubix/v1/dids/create` | `/api/admins/setup`, `/api/create-did-with-pubkey` |
| `POST /rubix/v1/dids/<did>/register` | `/api/admins/setup` (best-effort) |
| `POST /rubix/v1/smart_contracts/generate` | `scripts/deploy-contracts.sh`, `/api/deploy-contract` |
| `GET  /rubix/v1/smart_contracts/<id>/chain` | `/api/contracts/.../chain` |
| `POST /rubix/v1/tx` | reward transfer, activity/add, admin/add, deploy, execute-contract |
| `POST /rubix/v1/signature` | finalizes every async Rubix flow |
| `POST /api/generate-local-rbt` | manual admin bootstrap (§3.4) |
| `POST /rubix/v1/fts/mint` | manual admin bootstrap (§3.4) |

### Correlation keys

- **`request_id`** — UUID generated by the dApp when a reward request is
  accepted. Client polls status with this. Stable forever.
- **`transaction_id`** — Returned by `POST /rubix/v1/signature` on
  success. Stored in `transfer_status.transaction_id` and on the chain
  audit log. Empty for failed/pending jobs.
- **`contract_hash`** — Rubix v2 smart-contract token id (per-node,
  encodes PeerID). Stored per (admin, kind) in `admin_contracts`.
- **`user_did`** ↔ **`admin_did`** — `user_admins` maps each user to the
  admin who minted/onboarded them. One admin per user.

---

## License

TBD.
