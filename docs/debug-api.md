# Debug API (`/api/debug/*`)

Read-only operator diagnostics. Every endpoint is one of the hand-written
`psql` or `curl` checks used during the September 2026 chain-fork incident
(`docs/INCIDENT-2026-09-04-chain-fork.md`), so the next incident can be
diagnosed from a laptop without a shell on the VM. Endpoints 1 to 5 read
the dApp's own database and the nodes' HTTP API; 6 to 11 read each node's
own Postgres (setup below).

- All routes are `GET`, sit behind the normal operator bearer token, and
  never write anything.
- All timestamps are UTC, RFC3339 (`2026-09-08T10:30:00Z`). Nullable
  timestamps are JSON `null`.
- `since` accepts RFC3339, or `YYYY-MM-DD HH:MM[:SS]` and `YYYY-MM-DD`
  read as UTC (the form `morning-check.sh` takes). URL-encode the space
  (`%20`) or use the `T` form.
- Responses use the usual envelope: `{"status":true,"data":{...}}`, or
  `{"status":false,"error":"...","message":"..."}` with 400 for bad
  parameters and 500 for a database failure. A node that cannot be reached
  is reported inside the data, never as a 500.
- Only `kind = 'reward'` rows are considered; deploys, add_activity and
  add_admin are excluded.

## Getting a token

```bash
TOKEN=$(curl -s -X POST http://localhost:9100/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"allen.i@rubix.net","password":"<BOOTSTRAP_PASSWORD from .env>"}' | jq -r .access_token)
H="Authorization: Bearer $TOKEN"
```

Use `https://yqa.rubix.network` instead of `localhost:9100` when nginx is up.

## 1. `GET /api/debug/payouts/summary?since=&admin_did=&user_did=`

Per-admin counts of reward transfers created at or after `since`
(default: 24 hours ago). Optional `admin_did` narrows to one admin;
optional `user_did` counts only that user's requests (still grouped by
the admin that served them). Admins with no rows in the window are
omitted.

```bash
# how many requests one user sent, and how they ended
curl -s -H "$H" "http://localhost:9100/api/debug/payouts/summary?since=2026-09-01&user_did=$USER" | jq '.data.admins[] | {node_port, total, success, failed, in_flight}'
```

```bash
curl -s -H "$H" 'http://localhost:9100/api/debug/payouts/summary?since=2026-09-08T10:30:00Z' | jq .
```

```json
{"status":true,"data":{"since":"2026-09-08T10:30:00Z","admins":[
  {"admin_did":"bafybmid3lona…","node_port":"8003","total":412,"success":409,"failed":3,
   "chain_mismatch":3,"in_flight":0,
   "first_mismatch_at":"2026-09-08T10:41:02Z","last_mismatch_at":"2026-09-08T10:41:09Z",
   "last_success_at":"2026-09-08T10:40:58Z","last_success_tx":"f84eaac6ac56…"}
]}}
```

| Field | Meaning |
|---|---|
| `total` | rows in the window |
| `success` / `failed` | by status |
| `chain_mismatch` | failed rows whose `error_details` contain `chain mismatch` (a quorum/owner fork) |
| `in_flight` | `queued` + `processing` |
| `first_mismatch_at` / `last_mismatch_at` | in the window |
| `last_success_at` | in the window |
| `last_success_tx` | the admin's newest successful transaction, regardless of window |

**Read:** `chain_mismatch > 0` on any admin means that node has forked
again; go to `fork-check`. This replaces the "payouts per node" block of
`morning-check.sh`.

## 2. `GET /api/debug/payouts/failures?since=&admin_did=&limit=`

Failed reward transfers created at or after `since` (default: 24 hours
ago), grouped by normalised reason, most frequent first. Before grouping,
64-hex transaction and contract ids, `bafybmi…` DIDs and `127.0.0.1:<port>`
are replaced by `#` and the text is cut at 200 characters, exactly as in
`morning-check.sh`. `limit` defaults to 20, maximum 200.

```bash
curl -s -H "$H" 'http://localhost:9100/api/debug/payouts/failures?since=2026-09-08%2006:00&limit=5' | jq .
```

```json
{"status":true,"data":{"since":"2026-09-08T06:00:00Z","limit":5,"failures":[
  {"count":6412,"reason":"initiateConsensusHandler: … SC lock failed …",
   "first_at":"2026-09-08T00:12:40Z","latest_at":"2026-09-08T06:33:51Z"},
  {"count":3,"reason":"… TokenChainIntigrityCheck: token # (smart_contract) chain mismatch after sync from #: local latest # != expected #",
   "first_at":"2026-09-08T10:41:02Z","latest_at":"2026-09-08T10:41:09Z"}
]}}
```

**Read:** `SC lock failed` = contract stuck locked after a crash (cured by
the startup unlock at node restart). `chain mismatch` = fork.
`connection refused` = the node process was down at that moment.

## 3. `GET /api/debug/payouts/history?admin_did=&user_did=&status=&since=&limit=`

Every column of `transfer_status`, newest first, for one admin, one
user, or one user under one admin. At least one of `admin_did` and
`user_did` is required. `status` is optional and must be one of
`queued`, `processing`, `success`, `failed`. `since` is optional with no
default. `limit` defaults to 100, maximum 1000.

```bash
# last 5 successes for node4's admin
curl -s -H "$H" "http://localhost:9100/api/debug/payouts/history?admin_did=$DID&status=success&limit=5" | jq .
# what failed in the last 30 minutes
curl -s -H "$H" "http://localhost:9100/api/debug/payouts/history?admin_did=$DID&status=failed&since=$(date -u -d '30 min ago' +%FT%TZ)" | jq .
```

Row fields: `request_id`, `transaction_id`, `kind`, `admin_did`,
`user_did`, `activity_ids`, `reward_points`, `contract_hash`, `status`,
`message`, `error_details`, `created_at`, `updated_at`. `data.count` is the
number of rows returned.

**Read:** three successes with `created_at` within a few hundred
milliseconds of each other followed by a mismatch is the burst pattern
that crashes the owner (see the incident doc, "Recurrence").

## 4. `GET /api/debug/contracts?admin_did=`

Every registered contract (`admin_contracts`) with the owner node's
current chain head, fetched live from
`GET http://localhost:<node_port>/rubix/v1/smart_contracts/{hash}/chain`.
Optional `admin_did` narrows to one admin. Node calls run five at a time
with a 15 s cap each; a node that fails is reported on its contracts in
`error` and the rest of the response is unaffected.

```bash
curl -s -H "$H" 'http://localhost:9100/api/debug/contracts' | jq '.data.admins[] | {node_port, c: [.contracts[] | {kind, head_position, head_tx, error}]}'
```

```json
{"status":true,"data":{"admin_did":"","admins":[
  {"admin_did":"bafybmid3lona…","node_port":"8003","contracts":[
    {"kind":"add_activity","contract_hash":"Qm…","chain_length":1,"head_position":0,"head_tx":"…","head_epoch":1756…},
    {"kind":"reward","contract_hash":"QmSDKA…","chain_length":4221,"head_position":4220,"head_tx":"f84eaac6ac56…","head_epoch":1757…}
  ]},
  {"admin_did":"bafybmig3n5xn…","node_port":"8004","contracts":[
    {"kind":"reward","contract_hash":"Qmc5PE…","chain_length":0,"head_position":-1,"head_tx":"","head_epoch":0,
     "error":"rubix call /rubix/v1/smart_contracts/Qmc5PE…/chain: dial tcp 127.0.0.1:8004: connect: connection refused"}
  ]}
]}}
```

`chain_length` counts every block including the deploy block, so
`head_position = chain_length - 1` and equals `tokens.latest_position` in
the node's Postgres. `head_tx` is the owner's head transaction id (what the
incident doc calls the owner head).

## 5. `GET /api/debug/fork-check?admin_did=`

The refork audit for one admin in one call. `admin_did` is required.

1. Takes the newest failed reward row whose `error_details` contain
   `chain mismatch` and parses `local latest <64hex>` (the quorum's head)
   and `expected <64hex>` (the owner's head as the quorum saw it), plus the
   contract id from `token <id> (smart_contract)`.
2. Fetches the owner's current head from its node, as in (4).
3. Reports the admin's newest success and the first mismatch after it.

```bash
curl -s -H "$H" "http://localhost:9100/api/debug/fork-check?admin_did=$DID" | jq .data
```

```json
{"admin_did":"bafybmid3lona…","node_port":"8003","contract":"QmSDKA…",
 "quorum_head":"21cad7a6af11…","owner_expected":"f84eaac6ac56…","owner_current_head":"f84eaac6ac56…",
 "owner_chain_length":4221,"forked":true,
 "mismatch_seen_at":"2026-09-08T10:41:09Z","mismatch_request_id":"…",
 "first_mismatch_at":"2026-09-08T09:10:12Z",
 "last_success_tx":"f84eaac6ac56…","last_success_at":"2026-09-08T09:10:09Z"}
```

| Field | Meaning |
|---|---|
| `quorum_head` | from the error text: the quorum's copy is at this block |
| `owner_expected` | from the error text: the owner's head at the time of that failure |
| `owner_current_head` | live from the owner node now |
| `owner_chain_length` | live; head is at `owner_chain_length - 1` |
| `owner_has_quorum_head` | the quorum's block is somewhere on the owner's chain now: the fork was repaired |
| `mismatch_stale` | the newest mismatch is older than the newest success, so it describes a fork that has since been repaired |
| `forked` | true only when the owner was reached, the mismatch is not stale, and the quorum's block is not on the owner's chain |
| `mismatch_seen_at`, `mismatch_request_id` | the row the heads were parsed from |
| `first_mismatch_at` | oldest mismatch after `last_success_at`: when the fork happened; `null` if the newest mismatch predates the last success |
| `last_success_tx`, `last_success_at` | the block the owner is expected to be sitting on |
| `node_error` | present when the owner node could not be reached; `forked` is then `false` and must not be trusted |
| `note` | present when there are no mismatch rows, the row could not be parsed, or the fork is already repaired |

**Read:**

- `forked: true` with `owner_current_head == owner_expected`: the classic
  fork. The quorum is one block ahead. Run the replay procedure in the
  incident doc for this node; the lost block is `quorum_head`.
- `forked: false` with `owner_has_quorum_head: true`: repaired. The
  owner may have moved on since; `owner_current_head` is then newer than
  `quorum_head`, which is fine.
- `forked: false` with `mismatch_stale: true` and
  `owner_has_quorum_head: false`: payouts succeed again but the quorum's
  block never reached the owner. Only possible after a redeploy; check
  the `note`.
- `note: "no chain-mismatch failures recorded for this admin"`: nothing
  to do for this node.

## Daily check in three calls

```bash
curl -s -H "$H" 'http://localhost:9100/api/debug/payouts/summary?since=2026-09-08T10:30:00Z' | jq '.data.admins[] | {node_port, success, failed, chain_mismatch, in_flight}'
curl -s -H "$H" 'http://localhost:9100/api/debug/payouts/failures?since=2026-09-08T10:30:00Z' | jq '.data.failures[] | {count, reason: .reason[0:110]}'
curl -s -H "$H" 'http://localhost:9100/api/debug/contracts' | jq '.data.admins[] | {node_port, heads: [.contracts[] | select(.kind=="reward") | {head_position, error}]}'
```

## Node database endpoints (`/api/debug/node/*`)

These read each Rubix node's **own Postgres** (the `nodeN-postgres`
containers), which is where the owner-side half of every incident query
ran. Same rules as above: `GET`, operator token, read-only, UTC. They
answer **503** until `NODE_DB_PASSWORD` is set in `.env`.

### Setup

The nodes publish Postgres on `127.0.0.1:<node API port + 1000>` (node1
API 8000, Postgres 9000). The dApp connects with the node's own
credentials and opens every session with
`default_transaction_read_only=on`, so this code path only ever reads.

In `.env` on the yqa VM (values from `/datadrive/rubix-docker/docker-compose.yml`):

```
NODE_DB_HOST=127.0.0.1
NODE_DB_PORT_OFFSET=1000
NODE_DB_USER=rubix
NODE_DB_PASSWORD=<POSTGRES_PASSWORD from the compose file>
NODE_DB_NAME=rubix
```

and restart the dApp. The startup log prints `node db access enabled`.

Every response carries `node_port` and `db_port` so you can see which
database answered. A node whose database is unreachable is reported in
`error` on its entry in the fleet-wide endpoints, or as **502** on the
single-node ones.

### 6. `GET /api/debug/node/contracts?admin_did=`

The `tokens` row for every registered contract, plus the chain-consistency
triple from `morning-check.sh`. Optional `admin_did`.

```bash
curl -s -H "$H" 'http://localhost:9100/api/debug/node/contracts' | jq -c '.data.admins[] | {node_port, error, c: [.contracts[] | select(.kind=="reward") | {status_name, latest_position, head_tx: .head_tx[0:12], locked, consistent}]}'
```

| Field | Meaning |
|---|---|
| `found` | the hash exists in the node's `tokens` table |
| `status`, `status_name` | `token_status`: 10 deployed, 11 executed, 1 locked, … |
| `latest_position`, `latest_role`, `latest_role_name` | head position and the role that wrote it |
| `head_tx` | `tokens.transaction_id`, the owner head |
| `lock_reference_id` | request id of the lock holder |
| `tokenchain_rows`, `index_len`, `joined_rows`, `max_position` | the consistency triple; all should be `latest_position + 1` (and max = latest) |
| `locked` | `status == 1` |
| `stale_lock_ref` | lock reference left on a non-locked token; harmless residue of the failed-transaction release path |
| `consistent` | all four numbers agree |

### 7. `GET /api/debug/node/chain?admin_did=&kind=&limit=`

The newest `limit` rows of `tokenchain` for one contract, highest position
first. `admin_did` required, `kind` defaults to `reward`, `limit` defaults
to 20, maximum 500. Each row: `position`, `transaction_id`, `previous_tx`,
`role`, `role_name`, `created_at`, and `has_transaction` (false when the
`transactions` row is missing, i.e. an orphaned chain entry).

This is the owner tail you compare with the quorum's during a replay.

### 8. `GET /api/debug/node/transaction?admin_did=&tx=`

One `transactions` row from the admin's node: `info` and `signature` as
JSON, `created_at`, the `transaction_units` rows and every `tokenchain`
row that references it. `info_hash` is SHA3-256 of the stored info text
and `info_hash_matches_id` is the same check `check-orphans.py` does. A
missing transaction returns 200 with `found: false`.

Use it to answer "does the block the quorum committed exist on the owner?"
before and after a replay.

### 9. `GET /api/debug/node/locks?admin_did=`

Every token with `token_status = 1` across all nodes (or one admin's node),
with its type, lock reference, position, `locked_since` and `locked_for`.
`kind` is filled when the token is a registered contract. `data.nodes`
lists each node queried and any connection error.

```bash
curl -s -H "$H" 'http://localhost:9100/api/debug/node/locks' | jq '.data | {count, nodes: [.nodes[] | select(.error)], locks: [.locks[] | {node_port, kind, locked_for, lock_reference_id}]}'
```

**Read:** a reward contract locked for minutes with no `processing`
request in `payouts/history` is a lock left by a crash. The startup unlock
clears it on the next node restart.

### 10. `GET /api/debug/node/tokens?admin_did=`

The admin DID's tokens on its node grouped by type and status (count and
summed value), its `token_denom` rows, and the node's pending
`unpledge_sequence_info` rows (first 20, plus the total).

**Read:** `rbt / free` is how many contract deploys the node can still pay
for. `token_denom` not matching the free count is the platform bug noted in
the incident doc. `pending_unpledge_count` on an owner node should be 0;
on a quorum node it is the number of pledges awaiting release.

### 11. `GET /api/debug/node/quorum?admin_did=`

The `quorum_manager` DIDs of every node (or one). Replaces the
`/api/get-all-quorum` command in `docs/RESTART.md`, which is not a route on
this build.

### `fork-check` addition

When node database access is configured, `fork-check` also returns
`owner_db_head`, `owner_db_position`, `owner_db_status` and
`owner_db_lock_ref` from the owner's Postgres, so the audit still works
when the node's HTTP API is down. `owner_db_error` says why when it could
not be read.

## What the API still cannot see

- Node process panics: still `docker logs nodeN-node | grep -c '^panic:'`.
- The quorum side. Both VMs bind Postgres to `127.0.0.1`, so the quorum
  VM's databases are unreachable from yqa. The endpoints above would work
  against them unchanged through an SSH tunnel (`ssh -L 19000:127.0.0.1:9000 …`
  per node and a second `NODE_DB_HOST`/offset); until then the quorum
  head and `check-pledges.sh` stay on the quorum VM.

`morning-check.sh` remains the tool for those.
