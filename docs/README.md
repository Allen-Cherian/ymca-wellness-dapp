# docs

Client-facing API documentation. Both files are self-contained HTML —
open them in a browser, or publish them as-is.

| File | Audience | Covers |
|---|---|---|
| `v1-payouts-api.html` | Client integrators | The seven v1-contract endpoints: `/createdid`, `/admin/activity/add`, `/admin/activity/list`, `/admin/payouts`, `/admin/payouts/status/:id`, `/admin/user/add`, `/users/:did/payouts` — plus bearer-token auth |
| `api-guide.html` | Internal / full surface | The v2 `/api/*` endpoints, including admin provisioning and contract deploy/execute |

Every request and response example in both files was captured from a
live run against `https://yqa.rubix.network` on 2026-08-25. When an
endpoint's behavior changes, re-run it and update the example rather
than editing the JSON by hand.

For the *why* behind the v1 layer — field-mapping conventions, what has
no v2 equivalent — see `../V1_COMPATIBILITY.md`. That document is the
internal reference; `v1-payouts-api.html` is the client-facing view of
the same surface.

## Known gaps, stated in both documents

- **The FT leg is disabled** (`internal/service/service.go`, in
  `ProcessTransferReward`). Payouts settle with `status: success` and
  real transaction ids, and the audit record reaches the chain, but no
  `ytoken` balance moves.
- **Registration is open** (`internal/server/server.go`). Any account
  self-registered via `POST /api/auth/users` holds the full API surface,
  since `operator` is the only role. Test deployment only.
