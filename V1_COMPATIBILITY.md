# v1 client-contract compatibility

This document describes how `ymca-wellness-dapp` honors the original
client API contract documented at `proxy.rubix.network` (henceforth "the
v1 doc"), and where it deliberately diverges.

The native v2 API (`/api/...`) is unchanged and remains the canonical
surface. The v1-compat endpoints below are **additive aliases** that
reuse the same service-layer logic with reshaped request/response
formats.

---

## Table of contents

1. [Strategy](#1-strategy)
2. [Endpoint mapping](#2-endpoint-mapping)
3. [Field mapping conventions](#3-field-mapping-conventions)
4. [Per-endpoint reference](#4-per-endpoint-reference)
5. [What is intentionally not implemented](#5-what-is-intentionally-not-implemented)
6. [Source layout](#6-source-layout)
7. [Testing](#7-testing)

---

## 1. Strategy

The v1 doc was written against an older dApp architecture that used a
callback flow (`POST /api/call-back-trigger`) and exposed several fields
derived from extracting the latest block (`BlockNo`, `BlockId`, `Epoch`,
`InitiatorSignature`, etc.). v2 uses Rubix v2's blocking unified
transaction (`POST /rubix/v1/tx` + `POST /rubix/v1/signature`) with no
callbacks. Several v1 fields therefore have no v2 equivalent.

The compatibility layer follows three rules:

1. **Path and request body shapes** match the v1 doc exactly. Existing
   client integrations should be able to swap base URLs without code
   changes.
2. **Response top-level shape** matches v1 (`{status, message, result}`
   envelope). Field-by-field within `result` matches where v2 has a
   reasonable equivalent.
3. **Fields that v2 cannot meaningfully produce** are returned as `nil`
   or `""` rather than omitted. This preserves the JSON shape so client
   destructuring keeps working; the data is advisory in those slots.

No v2-only endpoint was removed; both surfaces coexist.

---

## 2. Endpoint mapping

| v1 path | HTTP | Handler | Reuses v2 logic from |
|---|---|---|---|
| `/createdid` | POST | `handleV1CreateDID` | `service.CreateDIDWithPubKey` |
| `/admin/activity/add` | POST | `handleV1AddActivity` | `service.AddActivity` |
| `/admin/activity/list` | GET | `handleV1ActivityList` | `database.ListActivities` (new query) |
| `/admin/payouts` | POST | `handleV1Payouts` | `database.CreateTransferStatus` + `queue.Enqueue` |
| `/admin/payouts/status/:request_id` | GET | `handleV1PayoutStatus` | `database.GetTransferStatusByRequestID` |
| `/admin/user/add` | POST | `handleV1UserAdd` | `service.AddAdmin` |
| `/users/:user_did/payouts` | GET | `handleV1UserPayouts` | `database.FTInfoForUser` (new query) |

All seven handlers live in `internal/server/v1_aliases.go`. Routes are
registered in `internal/server/server.go` *outside* the `/api` group so
the v1 paths sit at the root.

---

## 3. Field mapping conventions

Wherever a v1 doc field has no direct v2 equivalent, we use these
substitutions consistently:

| v1 doc field | v2 source | Notes |
|---|---|---|
| `block_id` | `transfer_status.transaction_id` | Same value, two names — v1 doc treats them as separate |
| `blockchain_tx_id` | `transfer_status.transaction_id` | Same as above |
| `ft_transfer_txid` | `transfer_status.transaction_id` | v1 had a separate FT-leg tx; v2's unified tx covers both |
| `started_at` | `transfer_status.created_at` | v2 has no separate started_at column |
| `completed_at` | `transfer_status.updated_at` | v2 has no separate completed_at column |
| `queued_at` | `""` (empty string) | No equivalent |
| `BlockNo` | `nil` | Was extracted from chain in v1; v2 doesn't extract |
| `Epoch` | `nil` | Same as BlockNo |
| `InitiatorSignature` | `nil` | v2 doesn't surface raw signatures |
| `InitiatorSignData` | `nil` | Same as above |
| `SmartContractData` | synthesized JSON string | Built from the request body, not extracted from chain |
| `ExecutorDID` (admin/user/add) | `existing_admin_did` from request | The DID that initiated the on-chain write |
| `block_hash` (in activity list) | `activities.transaction_id` (column added in migration `004_activities_tx_id.sql`) | Pre-existing rows return `""` |
| `ft_name` (user payouts) | `cfg.Env.FTName` (default `ytoken`) | Per-deployment configurable |

---

## 4. Per-endpoint reference

All handlers reside in `internal/server/v1_aliases.go`.

### 4.1 `POST /createdid`

**Handler:** `handleV1CreateDID` (lines ~36–58)

Creates a user DID on the admin's Rubix node from a public key. As a
side effect, populates `user_admins` with `(user_did → admin_did)` (this
behavior comes from the underlying `service.CreateDIDWithPubKey` and is
the same as `/api/create-did-with-pubkey`).

**Request:**
```json
{
  "admin_did":  "bafybmi...",
  "public_key": "04<128 hex chars>"
}
```

**Response (200):**
```json
{ "did": "bafybmi..." }
```

**Errors:** 400 (validation), 502 (Rubix rejects the key, e.g. wrong
length or not on curve), 500 (other).

---

### 4.2 `POST /admin/activity/add`

**Handler:** `handleV1AddActivity` (lines ~64–96)

Records an activity on-chain via the admin's `add_activity` contract
AND inserts into the `activities` table. Identical to
`/api/activity/add` semantically.

**Request:**
```json
{
  "activity_id":   "1004",
  "reward_points": 1,
  "admin_did":     "bafybmi..."
}
```

**Response (200):**
```json
{
  "status": true,
  "message": "Activity added successfully",
  "result": [
    {
      "BlockNo":           null,
      "BlockId":           "<64-char hex tx id>",
      "SmartContractData": "{\"activity_id\":\"1004\",\"reward_points\":1}",
      "Epoch":             null
    }
  ]
}
```

`BlockNo` and `Epoch` are always null. `BlockId` is the on-chain
transaction id. `SmartContractData` is synthesized from the request
body.

---

### 4.3 `GET /admin/activity/list`

**Handler:** `handleV1ActivityList` (lines ~178–193)

Lists activities from the local `activities` table.

**Query params:**
- `admin_did` (optional) — filter by admin. If omitted, returns all
  activities across every admin.

**Response (200):**
```json
[
  {
    "activity_id":   "1004",
    "block_hash":    "<64-char hex tx id, or empty string for pre-migration rows>",
    "reward_points": 1
  },
  ...
]
```

**Note:** activities created before migration `004_activities_tx_id.sql`
have a NULL `transaction_id` column and return `block_hash: ""`. New
activities have a populated value.

---

### 4.4 `POST /admin/payouts` (queues async transfer)

**Handler:** `handleV1Payouts` (lines ~102–162)

Queues a reward transfer. Returns 202 Accepted immediately — the actual
transfer is processed in the background by the per-admin worker. Use
`/admin/payouts/status/:request_id` to poll completion.

**Request:**
```json
{
  "activity_id": ["999"],
  "user_did":    "bafybmi...",
  "admin_did":   "bafybmi..."
}
```

`activity_id` is an array (note the singular field name).

**Response (202):**
```json
{
  "status": true,
  "message": "Transfer request queued for processing",
  "result": { "request_id": "<uuid>" }
}
```

**Error responses:**
- 400: validation (invalid DID format, empty activity list, unknown admin)
- 503: per-admin queue at capacity (1000 jobs)
- 500: DB persistence error

---

### 4.5 `GET /admin/payouts/status/:request_id`

**Handler:** `handleV1PayoutStatus` (lines ~168–209)

Fetches a `transfer_status` row.

**Response (200):**
```json
{
  "status": true,
  "message": "<transfer_status.message>",
  "result": {
    "request_id":       "<uuid>",
    "activity_ids":     ["..."],
    "admin_did":        "bafybmi...",
    "user_did":         "bafybmi...",
    "reward_points":    25,
    "contract_hash":    "Qm...",
    "status":           "queued|processing|success|failed",
    "message":          "transferred 25 ytoken to ...",
    "error_details":    "",
    "block_id":         "<tx id>",
    "blockchain_tx_id": "<tx id>",
    "ft_transfer_txid": "<tx id>",
    "queued_at":        "",
    "started_at":       "<created_at>",
    "completed_at":     "<updated_at>",
    "created_at":       "<RFC3339>",
    "updated_at":       "<RFC3339>"
  }
}
```

**Field redundancy is intentional.** `block_id`, `blockchain_tx_id`, and
`ft_transfer_txid` all carry the same value (`transfer_status.transaction_id`).

`started_at == created_at` and `completed_at == updated_at` always.
`queued_at` is always `""`. See [§3](#3-field-mapping-conventions).

**Error responses:**
- 400: missing request_id (path routing typically prevents this)
- 404: unknown request_id
- 500: DB error

---

### 4.6 `POST /admin/user/add`

**Handler:** `handleV1UserAdd` (lines ~215–245)

Appends a new admin DID to the existing admin's `add_admin` contract
chain. This is on-chain only — it does **not** insert into the
`admins` table. To make the new DID known to the dApp, also call
`POST /api/admins/setup` (or insert into `admins` directly).

**Request:**
```json
{
  "new_admin_did":      "bafybmi...new",
  "existing_admin_did": "bafybmi...existing"
}
```

**Response (200):**
```json
{
  "status": true,
  "message": "Fetched latest block smart contract data",
  "result": [
    {
      "BlockNo":            null,
      "BlockId":            "<tx id>",
      "SmartContractData":  "{\"add_admin\": {\"admin_did\":\"bafybmi...new\"}}",
      "Epoch":              null,
      "InitiatorSignature": null,
      "ExecutorDID":        "bafybmi...existing",
      "InitiatorSignData":  null
    }
  ]
}
```

`ExecutorDID` is the existing admin (the request's `existing_admin_did`
field). The other null fields have no v2 equivalent.

---

### 4.7 `GET /users/:user_did/payouts`

**Handler:** `handleV1UserPayouts` (lines ~251–279)

Returns aggregate FT totals received by `user_did`, grouped by the
admin (creator) who sent them. Only counts successful reward transfers.

**Response (200):**
```json
{
  "status": true,
  "message": "Got FT info successfully",
  "result": null,
  "ft_info": [
    {
      "ft_name":     "ytoken",
      "ft_count":    50,
      "creator_did": "bafybmi..."
    }
  ]
}
```

`result: null` matches the v1 doc literally — the data is in `ft_info`,
not `result`. `ft_count` is the sum of `reward_points` across all
successful reward rows for that (user, admin) pair.

If the user has never received a reward, returns `ft_info: []`.

---

## 5. What is intentionally not implemented

These v1 endpoints exist in the doc but are not implemented in v2:

| v1 endpoint | Reason |
|---|---|
| `/register` | Auth/account management. v2 has no auth layer. |
| `/get-token` | Same as above. |
| `/logout` | Same as above. |
| `/refresh-token` | Same as above. |
| `/api/register-did` | v2 bundles register-DID inside `/api/admins/setup` (best-effort). The two-step "register then sign" flow is not exposed. |
| `/api/signature-response` | Was a generic Rubix signature relay in v1. In v2, the dApp calls `POST /rubix/v1/signature` internally; clients never sign anything against the dApp directly. |

Additionally:

- **`Authorization: Bearer <token>` headers** on v1-compat endpoints are
  silently accepted but ignored. The dApp does not verify them. Clients
  authenticating against v1 will not get any false security signal —
  there is no signal at all.
- **Multipart file uploads** (e.g. for contract deploy) are routed via
  `/api/deploy-contract`. There is no v1-compat alias because the doc
  did not specify one; deploys are typically batch-driven via
  `scripts/deploy-contracts.sh` instead.

---

## 6. Source layout

```
internal/server/
├── server.go              # Routes registered for both /api/* and v1 paths
├── handlers.go            # v2 handlers (canonical surface)
├── admins_handler.go      # /api/admins/setup
├── v1_aliases.go          # ← all v1-compat handlers live here
└── dto.go                 # Request DTOs (shared between v1 and v2)

internal/database/
├── queries.go             # Includes ListActivities + FTInfoForUser
│                            (added for the v1 surface)
└── models.go              # Activity struct now has TransactionID

migrations/
└── 004_activities_tx_id.sql   # Adds transaction_id column to activities
```

The v1 layer is deliberately a thin file — every handler delegates to
existing v2 service-layer methods. The only new database queries are
the ones genuinely needed (`ListActivities`, `FTInfoForUser`); they are
also usable by v2-side endpoints if needed.

---

## 7. Testing

Restart the dApp and exercise each endpoint:

```bash
export BASE=http://localhost:9000

# Use existing admins/users from the database. Pick any admin DID:
export ADMIN=$(PGPASSWORD=postgres psql -h localhost -U postgres -d ymca_wellness_cafe_v2 \
  -tAc "SELECT did FROM admins LIMIT 1" | tr -d ' ')

# 1. Activity list (no filter)
curl -s $BASE/admin/activity/list | jq

# 2. Activity list (per admin)
curl -s "$BASE/admin/activity/list?admin_did=$ADMIN" | jq

# 3. Add activity — block_hash will be populated for new rows
curl -s -X POST $BASE/admin/activity/add \
  -H "Content-Type: application/json" \
  -d "{\"activity_id\":\"ACT-V1-TEST\",\"reward_points\":7,\"admin_did\":\"$ADMIN\"}" | jq

# 4. Re-list — confirm the new row has block_hash populated
curl -s "$BASE/admin/activity/list?admin_did=$ADMIN" | jq '[.[] | select(.activity_id == "ACT-V1-TEST")]'

# 5. Payout (queue a transfer)
export USER=$(PGPASSWORD=postgres psql -h localhost -U postgres -d ymca_wellness_cafe_v2 \
  -tAc "SELECT user_did FROM transfer_status WHERE status='success' LIMIT 1" | tr -d ' ')
RESP=$(curl -s -X POST $BASE/admin/payouts \
  -H "Content-Type: application/json" \
  -d "{\"activity_id\":[\"ACT-V1-TEST\"],\"user_did\":\"$USER\",\"admin_did\":\"$ADMIN\"}")
echo "$RESP" | jq
REQ_ID=$(echo "$RESP" | jq -r '.result.request_id')

# 6. Poll status
sleep 5
curl -s "$BASE/admin/payouts/status/$REQ_ID" | jq

# 7. User payouts (FT info aggregation)
curl -s "$BASE/users/$USER/payouts" | jq

# 8. /createdid — needs a real secp256k1 key
PRIV=$(mktemp)
openssl ecparam -name secp256k1 -genkey -noout -out "$PRIV" 2>/dev/null
PUBKEY=$(openssl ec -in "$PRIV" -pubout -conv_form uncompressed -text -noout 2>/dev/null \
  | awk '/pub:/{flag=1;next}/ASN1 OID/{flag=0}flag' | tr -d ' :\n')
rm -f "$PRIV"
curl -s -X POST $BASE/createdid \
  -H "Content-Type: application/json" \
  -d "{\"admin_did\":\"$ADMIN\",\"public_key\":\"$PUBKEY\"}" | jq
```

Expected outputs match the per-endpoint shapes in [§4](#4-per-endpoint-reference).
