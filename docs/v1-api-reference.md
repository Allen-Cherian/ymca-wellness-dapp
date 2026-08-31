# Wellness Café Payouts API — Reference

Base URL: `https://yqa.rubix.network`

All endpoints require a bearer token:

```
Authorization: Bearer <access_token>
```

---

## Authentication

### POST /api/auth/users

Create an account. Open — no token required. Run once per account.

Request:
```json
{ "email": "you@example.com", "password": "your-password" }
```

Response — 201:
```json
{
  "status": true,
  "data": {
    "id": "f8f82dc3-3d98-4ff0-a7d6-0365b20b6e22",
    "email": "you@example.com",
    "role": "operator",
    "created_at": "2026-08-25T07:51:11.673478Z"
  }
}
```

---

### POST /api/auth/token

Generate an access token. Valid 15 minutes. Call as often as needed — no session is created, and earlier tokens stay valid. `/api/auth/login` is an alias.

Request:
```json
{ "email": "you@example.com", "password": "your-password" }
```

Response — 200:
```json
{
  "access_token": "eyJhbGciOiJSUzI1NiIs...",
  "refresh_token": "...",
  "expires_in": 900,
  "token_type": "Bearer"
}
```

Note: the response is flat — read `access_token`, not `data.access_token`.

---

### POST /api/auth/refresh

Exchange a refresh token for a new pair. Only needed for jobs running longer than 15 minutes.

Request:
```json
{ "refresh_token": "..." }
```

Response — 200: same shape as `/api/auth/token`.

Note: refresh tokens rotate. Reusing a spent one returns 401 and revokes all refresh tokens for that account.

---

## Users

### POST /createdid

Create a user DID from a secp256k1 public key (`04` + 128 hex chars). Also records which admin owns the user.

Request:
```json
{
  "admin_did": "bafybmicvcvtn3l4sizaocgwtjq7pdvparyrdvnbqj6b4jrugzu4wgtw3uu",
  "public_key": "046dfd30bc498a825f647ffe8c9e5c105785b4450cf8cfaaa79c2c1786835dcc99211017b00b37217be4a41e32f762d794187a8270f8fd1c3c6316a1c112d526fc"
}
```

Response — 200:
```json
{ "did": "bafybmiap7cpgjs3s3hr4qfje37itwb6t5cmik7xjnbbtdys5wcdx3tzudm" }
```

Note: no `status`/`message`/`result` envelope on this endpoint.

---

### GET /users/{user_did}/payouts

Fungible-token holdings for a user, grouped by the admin that issued them.

Response — 200:
```json
{
  "status": true,
  "message": "Got FT info successfully",
  "ft_info": [
    {
      "creator_did": "bafybmicvcvtn3l4sizaocgwtjq7pdvparyrdvnbqj6b4jrugzu4wgtw3uu",
      "ft_name": "ytoken",
      "ft_count": 15
    }
  ],
  "result": null
}
```

Note: read `ft_info`. `result` is always `null` here.

---

## Activities

### POST /admin/activity/add

Register an activity and its reward points against an admin. Writes on-chain; takes 1–3 seconds.

Request:
```json
{
  "admin_did": "bafybmicvcvtn3l4sizaocgwtjq7pdvparyrdvnbqj6b4jrugzu4wgtw3uu",
  "activity_id": "cycle-5k",
  "reward_points": 15,
  "description": "5km cycle"
}
```

Response — 200:
```json
{
  "status": true,
  "message": "Activity added successfully",
  "result": [
    {
      "BlockId": "241ee2ced0fc5aee4081d1d5763f80920aa8da483edafe54de0587de3eca283a",
      "BlockNo": null,
      "Epoch": null,
      "SmartContractData": "{\"activity_id\":\"cycle-5k\",\"reward_points\":15}"
    }
  ]
}
```

Note: `activity_id` is unique per admin. Re-posting the same ID updates the activity rather than creating a duplicate.

---

### GET /admin/activity/list

List registered activities. Optional `?admin_did=` filter.

Response — 200:
```json
[
  { "activity_id": "yoga-001", "reward_points": 10, "block_hash": "36e40f144977e464300574bc71906124e19bb42b0a8caceb66bf36e8974dca6a" },
  { "activity_id": "swim-100", "reward_points": 25, "block_hash": "5053075f7898650053d56f0581b7caad91e9fcdcb72209026cd1fc8afa6fed36" },
  { "activity_id": "cycle-5k", "reward_points": 15, "block_hash": "241ee2ced0fc5aee4081d1d5763f80920aa8da483edafe54de0587de3eca283a" }
]
```

Note: returns a bare array — no envelope.

---

## Payouts

### POST /admin/payouts

Queue a reward payout. Asynchronous — returns immediately with a request ID. Points are summed across the activity IDs given.

Request:
```json
{
  "user_did": "bafybmiap7cpgjs3s3hr4qfje37itwb6t5cmik7xjnbbtdys5wcdx3tzudm",
  "admin_did": "bafybmicvcvtn3l4sizaocgwtjq7pdvparyrdvnbqj6b4jrugzu4wgtw3uu",
  "activity_id": ["cycle-5k"]
}
```

Response — 202:
```json
{
  "status": true,
  "message": "Transfer request queued for processing",
  "result": { "request_id": "8eb60a34-4678-4f5b-a033-0d746afa61b2" }
}
```

Note: `activity_id` is an array.

---

### GET /admin/payouts/status/{request_id}

Check payout status. Poll until `status` is `success` or `failed`.

Response — 200:
```json
{
  "status": true,
  "message": "transferred 15 ytoken to bafybmiap7cpgjs3s3hr4qfje37itwb6t5cmik7xjnbbtdys5wcdx3tzudm",
  "result": {
    "request_id": "8eb60a34-4678-4f5b-a033-0d746afa61b2",
    "status": "success",
    "reward_points": 15,
    "activity_ids": ["cycle-5k"],
    "user_did": "bafybmiap7cpgjs3s3hr4qfje37itwb6t5cmik7xjnbbtdys5wcdx3tzudm",
    "admin_did": "bafybmicvcvtn3l4sizaocgwtjq7pdvparyrdvnbqj6b4jrugzu4wgtw3uu",
    "block_id": "1e24967b21f7a00c23dce8afc3fa1df69f530f8771283723c95bfb95d078f1e9",
    "blockchain_tx_id": "1e24967b21f7a00c23dce8afc3fa1df69f530f8771283723c95bfb95d078f1e9",
    "ft_transfer_txid": "1e24967b21f7a00c23dce8afc3fa1df69f530f8771283723c95bfb95d078f1e9",
    "contract_hash": "QmdtEWSHnUug1HvNFzpQsFGXgiGYwWfBfjrEQmVuv7rR4X",
    "started_at": "2026-08-25T08:26:09Z",
    "completed_at": "2026-08-25T08:26:10Z",
    "queued_at": "",
    "error_details": ""
  }
}
```

Status values: `queued`, `processing`, `success`, `failed`.

Note: `block_id`, `blockchain_tx_id`, and `ft_transfer_txid` all carry the same transaction ID.

---

## Admins

### POST /admin/user/add

Record a new admin DID on an existing admin's chain. On-chain record only — does not make the DID usable for payouts. Ten admins are already provisioned.

Request:
```json
{
  "new_admin_did": "bafybmie4ztg3q35pae6yfupzganlglxrwtilp6xes5t6redlwrrb34qi5i",
  "existing_admin_did": "bafybmicvcvtn3l4sizaocgwtjq7pdvparyrdvnbqj6b4jrugzu4wgtw3uu"
}
```

Response — 200:
```json
{
  "status": true,
  "message": "Fetched latest block smart contract data",
  "result": [
    {
      "BlockId": "34b7e25a6d834ced9b14ad8875e75fb2a618adb9f4c2246148968d96baea0a3c",
      "ExecutorDID": "bafybmicvcvtn3l4sizaocgwtjq7pdvparyrdvnbqj6b4jrugzu4wgtw3uu",
      "SmartContractData": "{\"add_admin\": {\"admin_did\":\"bafybmie4ztg3q35pae6yfupzganlglxrwtilp6xes5t6redlwrrb34qi5i\"}}",
      "BlockNo": null,
      "Epoch": null,
      "InitiatorSignature": null,
      "InitiatorSignData": null
    }
  ]
}
```

---

## Status codes

| Code | Meaning |
|---|---|
| 200 | Success |
| 201 | Account created |
| 202 | Payout queued |
| 400 | Validation failed |
| 401 | Missing or expired token |
| 404 | Unknown request ID or user |
| 409 | Email already registered |
| 500 | Server or contract lookup failure |
| 502 | Rubix node rejected the call |

Errors return: `{ "status": false, "error": "...", "message": "..." }`

---

## Notes

**Call order.** Create the user's DID via `POST /createdid` before paying them. A payout to a DID created elsewhere succeeds, but the user has no admin mapping, so `GET /users/{did}/payouts` will not find them.

**Always-empty fields.** `BlockNo`, `Epoch`, `InitiatorSignature`, `InitiatorSignData` return `null`; `queued_at` returns `""`. Kept for response-shape compatibility; they carry no data.

**Timeouts.** Chain writes block on signing. Allow up to 300 seconds.

**One account per job.** Refresh-token rotation means concurrent jobs sharing an account will revoke each other's tokens.

**Test environment.** Payouts settle with `status: success` and real transaction IDs, but no `ytoken` balance moves yet — the fungible-token leg is currently disabled server-side.

---

*Verified against https://yqa.rubix.network on 25 August 2026.*
