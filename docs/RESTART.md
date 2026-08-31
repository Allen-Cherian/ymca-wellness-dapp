# Restarting the dApp after a VM reboot

Test deployment on `EXTSTVM01`. Work through this top to bottom — the
checks are ordered so a failure tells you which layer broke.

Nothing here needs `sudo` except the nginx check.

## The names you need

| Thing | Value |
|---|---|
| Repo / working dir | `/datadrive/ymca-wellness-dapp` |
| Binary | `./ymca-dapp` |
| dApp port | `9100` |
| Postgres container | `ymca-pg` |
| Postgres port / database | `5445` / `ymca_wellness_dapp` |
| Postgres data on disk | `/datadrive/pgdata-ymca` |
| Node containers | `node1-node` … `node10-node` (Rubix API on `8000`–`8009`) |
| Node DID list | `/datadrive/rubix-docker/did_results.json` |
| Public URL | `https://yqa.rubix.network` |
| Operator login | `allen.i@rubix.net` |

`node1` is port `8000`, `node10` is port `8009` — the numbering is off by
one.

---

## Step 1 — Are the containers back?

```bash
docker ps --filter name=node --format '{{.Names}}\t{{.Status}}' | sort
docker ps --filter name=ymca-pg --format '{{.Names}}\t{{.Status}}'
```

Expect 20 node containers (10 `-node` + 10 `-postgres`) and `ymca-pg`,
all `Up`. They carry `--restart unless-stopped`, so they should return by
themselves.

If any are missing: `docker start <name>`.

---

## Step 2 — Is Postgres mounted correctly?

This one looks fine when it is not. Run it every time.

```bash
docker inspect ymca-pg -f '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'
```

**Want exactly one line:**

```
/datadrive/pgdata-ymca -> /var/lib/postgresql
```

**If a second line appears** pointing at `/var/lib/docker/volumes/<hash>/_data`,
Docker created an anonymous volume that shadows the real data. The
container will be healthy and the database will appear empty. Fix:

```bash
docker stop ymca-pg && docker rm ymca-pg

docker run -d --name ymca-pg \
  -e POSTGRES_PASSWORD=postgres \
  -e PGDATA=/var/lib/postgresql/pgdata \
  -v /datadrive/pgdata-ymca:/var/lib/postgresql \
  -p 5445:5432 \
  --restart unless-stopped \
  postgres:18

until docker exec ymca-pg pg_isready -U postgres -q; do sleep 1; done; echo ready
```

Your data is on disk at `/datadrive/pgdata-ymca` and is not lost — only
the mount path is wrong.

Then confirm the database is visible:

```bash
docker exec ymca-pg psql -U postgres -lqt | cut -d'|' -f1 | grep ymca
```

Want `ymca_wellness_dapp`.

---

## Step 3 — Is the data intact?

```bash
docker exec ymca-pg psql -U postgres -d ymca_wellness_dapp -c \
  "SELECT 'admins' t, count(*) FROM admins
   UNION ALL SELECT 'admin_contracts', count(*) FROM admin_contracts
   UNION ALL SELECT 'activities', count(*) FROM activities
   UNION ALL SELECT 'transfer_status', count(*) FROM transfer_status
   UNION ALL SELECT 'auth_users', count(*) FROM auth_users ORDER BY 1;"
```

Baseline as of 31 Aug 2026: **10 admins, 30 admin_contracts, 2 auth_users**.
Activities and transfers grow with use — they should never shrink.

If the counts are zero, stop and go back to Step 2 before doing anything
else.

---

## Step 4 — Start the dApp

```bash
screen -S dapp
cd /datadrive/ymca-wellness-dapp
./ymca-dapp
```

Then **Ctrl-A** then **D** to detach.

The `cd` is required — the binary reads `.env`, `keys/`, `dids/`, and
`artifacts/` relative to its working directory, and will not start from
elsewhere.

Watch for this line before detaching:

```
ymca-wellness-dapp listening on :9100 (admins=10, queue_buf=1000)
```

`admins=10` means it read the database. `admins=0` means it connected to
the wrong one — check `DB_PORT=5445` and `DB_NAME=ymca_wellness_dapp` in
`.env`.

Screen commands: `screen -ls` to list, `screen -r dapp` to reattach,
`screen -X -S dapp quit` to kill.

If the binary is missing or you have pulled new code:

```bash
cd /datadrive/ymca-wellness-dapp
go build -o ymca-dapp ./cmd/server && echo BUILD OK
```

---

## Step 5 — Is it reachable?

```bash
curl -s https://yqa.rubix.network/api/health
```

Want `{"admins":10,"ft_name":"ytoken","status":"ok"}`.

If this fails but `curl -s localhost:9100/api/health` works, the problem
is nginx:

```bash
sudo systemctl status nginx --no-pager
sudo nginx -t && sudo systemctl reload nginx
```

---

## Step 6 — Does the chain actually work?

**Do not skip this.** Steps 1–5 can all pass while every on-chain write
fails — reads come from Postgres, so nothing above touches Rubix. After
a reboot the chain is usually the broken part.

```bash
BASE=https://yqa.rubix.network
ADMIN=bafybmicvcvtn3l4sizaocgwtjq7pdvparyrdvnbqj6b4jrugzu4wgtw3uu

TOKEN=$(curl -sS -X POST "$BASE/api/auth/token" \
  -H 'Content-Type: application/json' \
  -d '{"email":"allen.i@rubix.net","password":"<password>"}' \
  | jq -r '.access_token')

curl -s -X POST "$BASE/api/activity/add" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"admin_did\":\"$ADMIN\",\"activity_id\":\"healthcheck\",\"reward_points\":1}" \
  | jq -c '{status, message: (.message // .data.message)}'
```

**Success** looks like `"status": true` with a transaction id, or a
`data.transaction_id` in the full response.

### Failure: `No quorums available for transaction`

The nodes restarted without their quorum configuration. Reads still work,
writes are all blocked. This needs re-running the quorum setup on the
Rubix nodes — it is a `rubixgoplatform` operation, not a dApp one.

> **TODO:** paste the exact quorum setup commands here once confirmed.
> This happened on 31 Aug 2026 and took several attempts to identify.

### Failure: `peer ID not found for DID: <did>`

A quorum member is configured but its peer mapping is missing. First
check whether that DID is even one of yours:

```bash
grep <did-fragment> /datadrive/rubix-docker/did_results.json
```

No match means the quorum list references a DID from a different setup
and should be corrected to your ten. A match means the peer mapping needs
re-adding on that node.

Useful while diagnosing:

```bash
curl -s http://localhost:8000/api/get-all-quorum | jq
curl -s http://localhost:8000/api/get-all-peers  | jq
docker logs --tail 40 node1-node 2>&1 | grep -i -E 'quorum|peer|error'
```

---

## Step 7 — Back up

Once everything above passes:

```bash
docker exec ymca-pg pg_dump -U postgres ymca_wellness_dapp \
  > /datadrive/backup-ymca-$(date +%F).sql
ls -lh /datadrive/backup-ymca-*.sql
```

### Restoring, if it ever comes to that

```bash
docker exec ymca-pg psql -U postgres -c "CREATE DATABASE ymca_wellness_dapp;"
docker exec -i ymca-pg psql -U postgres -d ymca_wellness_dapp \
  < /datadrive/backup-ymca-<date>.sql
```

---

## Quick reference

```bash
# full restart, assuming containers came back cleanly
cd /datadrive/ymca-wellness-dapp
docker inspect ymca-pg -f '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}'   # one line?
screen -S dapp
./ymca-dapp                                                              # Ctrl-A D
curl -s https://yqa.rubix.network/api/health                             # admins:10
# then the write test in Step 6
```

---

## Not covered here

- **systemd.** Deferred to the production build. Until then the dApp must
  be started by hand after every reboot — that is what this document is
  for. `docs/RUNBOOK.md` has the unit file when you want it.
- **First-time setup** — migrations, keys, admin registration, contract
  deployment. See `docs/RUNBOOK.md` Part 1. None of it is needed for a
  restart.
