# docs

Operations and client-facing API documentation. The HTML files are
self-contained — open them in a browser, or publish them as-is.

**After a VM reboot, start with [`RESTART.md`](RESTART.md).**

| File | Audience | Covers |
|---|---|---|
| `RESTART.md` | Operators | Bringing the dApp back up after a reboot — the short path, with the checks that catch silent failures |
| `RUNBOOK.md` | Operators | First-time setup, running the service, systemd, known gaps |
| `v1-payouts-api.html` | Client integrators | The seven v1-contract endpoints: `/createdid`, `/admin/activity/add`, `/admin/activity/list`, `/admin/payouts`, `/admin/payouts/status/:id`, `/admin/user/add`, `/users/:did/payouts` — plus bearer-token auth |
| `v1-api-reference.md` | Client integrators | The same seven endpoints as a plain reference — one line per endpoint plus its response structure. Source for the Word version. |
| `api-guide.html` | Internal / full surface | The v2 `/api/*` endpoints, including admin provisioning and contract deploy/execute |

To produce the Word version for sharing (the `.docx` is gitignored — it is
generated, not authored):

```bash
pandoc docs/v1-api-reference.md -o docs/v1-api-reference.docx --toc --toc-depth=2
```

Edit the Markdown, never the `.docx` — otherwise the two drift apart.

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
