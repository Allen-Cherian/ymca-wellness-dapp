# Runbook

Operating the dApp on the deployment VM. Two parts: **first-time setup**
(once per machine) and **after a reboot** (every time the VM restarts).

Current deployment:

| | |
|---|---|
| Repo | `/datadrive/ymca-wellness-dapp` |
| dApp port | `9100` (9000–9009 are the Rubix nodes' own Postgres) |
| dApp Postgres | `localhost:5445`, database `ymca_wellness_dapp` |
| Postgres data | `/datadrive/pgdata-ymca` (bind mount) |
| Rubix nodes | ports `8000`–`8009`, ten containers |
| Public URL | `https://yqa.rubix.network` (nginx → 9100) |

---

## Part 1 — First-time setup

### 1. Prerequisites

```bash
sudo apt update
sudo apt install -y postgresql-client jq curl openssl build-essential screen

wget -q https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc && export PATH=$PATH:/usr/local/go/bin
go version   # want go1.25+
```

Do **not** run `go` commands under `sudo` — it resets PATH and writes a
root-owned build cache.

### 2. Postgres

Keep the data directory **outside** the repo. `go mod tidy` walks the
whole tree and dies on Postgres's `0700` permissions.

```bash
sudo mkdir -p /datadrive/pgdata-ymca

docker run -d --name ymca-pg \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=ymca_wellness_dapp \
  -e PGDATA=/var/lib/postgresql/pgdata \
  -v /home/rubix/pgdata-ymca:/var/lib/postgresql \
  -p 5445:5432 \
  --restart unless-stopped \
  postgres:18

until docker exec ymca-pg pg_isready -U postgres -q; do sleep 1; done; echo ready
```

The mount point matters. `postgres:18` declares `/var/lib/postgresql` as
a VOLUME — one level above the `/var/lib/postgresql/data` that older tags
used. Mounting *inside* it lets Docker create an anonymous volume for the
parent, which silently shadows the bind mount: the container starts
clean, reports healthy, and writes to the wrong place. Mount the parent
and set `PGDATA` beneath it, as above.

Verify exactly one mount:

```bash
docker inspect ymca-pg -f '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'
```

### 3. Migrations

```bash
cd /datadrive/ymca-wellness-dapp
export PGPASSWORD=postgres
for f in migrations/00*.sql; do
  echo "-- $f"
  psql -h 127.0.0.1 -p 5445 -U postgres -d ymca_wellness_dapp -v ON_ERROR_STOP=1 -f "$f" || break
done
psql -h 127.0.0.1 -p 5445 -U postgres -d ymca_wellness_dapp -c '\dt'
```

Expect 7 tables: `admins`, `admin_contracts`, `activities`,
`transfer_status`, `user_admins`, `auth_users`, `refresh_tokens`. The
`|| break` matters — `003` drops a table `001` creates, so a partial run
leaves the schema wrong.

### 4. Keys and directories

`keys/`, `logs/`, and `dids/` are gitignored, so a fresh clone has none
of them. The server will not start without the JWT keypair.

```bash
mkdir -p keys logs dids
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out keys/jwt_private.pem
openssl rsa -in keys/jwt_private.pem -pubout -out keys/jwt_public.pem
chmod 600 keys/jwt_private.pem
chmod +x scripts/deploy-contracts.sh
```

### 5. Environment

```bash
cp .env.example .env
sed -i 's/^SERVER_PORT=.*/SERVER_PORT=9100/' .env
sed -i 's/^DB_PORT=.*/DB_PORT=5445/' .env
sed -i 's/^DB_NAME=.*/DB_NAME=ymca_wellness_dapp/' .env
sed -i 's/^BOOTSTRAP_EMAIL=.*/BOOTSTRAP_EMAIL=allen.i@rubix.net/' .env
sed -i 's/^BOOTSTRAP_PASSWORD=.*/BOOTSTRAP_PASSWORD=rubix@123/' .env
grep -E '^SERVER_PORT|^DB_|^BOOTSTRAP_|^FT_NAME' .env
```

`SERVER_PORT` must not be 9000 — that is node1's Postgres.

The bootstrap operator is seeded **only on a boot where `auth_users` is
empty**. Set these before the first start or you will have to insert the
row by hand.

### 6. Register the admin DIDs

The ten node DIDs live in `/datadrive/rubix-docker/did_results.json`.
Insert them directly — do **not** call `POST /api/admins/setup`, which
mints *new* DIDs rather than registering existing ones.

```bash
export PGPASSWORD=postgres
jq -r '.[] | "\(.did) \(.port)"' /datadrive/rubix-docker/did_results.json |
while read -r did port; do
  psql -h 127.0.0.1 -p 5445 -U postgres -d ymca_wellness_dapp -q -c \
    "INSERT INTO admins (did, node_port, password)
     VALUES ('$did', '$port', 'mypassword')
     ON CONFLICT (did) DO UPDATE SET node_port = EXCLUDED.node_port;"
done

psql -h 127.0.0.1 -p 5445 -U postgres -d ymca_wellness_dapp \
  -c "SELECT node_port, did FROM admins ORDER BY node_port;"
```

Want 10 rows, ports 8000–8009. The `password` column must match what
each node expects for signing, or every `Sign()` fails later.

### 7. Build and start

```bash
go build -o ymca-dapp ./cmd/server && echo BUILD OK
```

See [Part 3](#part-3--running-the-service) for how to run it. On the
first start, look for `bootstrap operator created:` in the log.

### 8. Deploy the contracts

Each node needs an RBT balance first — contract deployment spends a small
amount per contract. **Funding is a Rubix node operation, not a dApp one**,
and it happens outside this repo when the testnet is stood up.

The `POST /api/generate-local-rbt` call that older documentation used is
deprecated and now returns 404. Use whatever funding mechanism the current
`rubixgoplatform` build provides.

A node with no spendable RBT fails deployment at the transaction step with:

```
failed to lock RBT for SC committed tokens: lockSelectedTokens: no tokens provided
```

If you see that, fund the nodes and re-run — the script skips contracts
that already deployed.

Then deploy all thirty contracts (10 admins × `reward`, `add_activity`,
`add_admin`):

```bash
cd /datadrive/ymca-wellness-dapp
export DB_HOST=localhost DB_PORT=5445 DB_USER=postgres \
       DB_PASSWORD=postgres DB_NAME=ymca_wellness_dapp
./scripts/deploy-contracts.sh
```

**The exports are mandatory.** The script reads `DB_*` from the
environment only — it never loads `.env` — and defaults to port 5432 and
the old database name. Without them it reports "no admins in DB".

Run it from the repo root: `--artifacts-dir` defaults to `./artifacts`
and logs go to `./logs`.

Want `requested=30 deployed=30 failed=0`. The script is idempotent, so
re-run it after fixing any failure — already-deployed pairs are skipped.

```bash
psql -h 127.0.0.1 -p 5445 -U postgres -d ymca_wellness_dapp -c \
  "SELECT contract_kind, count(*) FROM admin_contracts GROUP BY 1 ORDER BY 1;"
```

Want 10 for each kind. **Back up now** — thirty deploys are slow to redo:

```bash
docker exec ymca-pg pg_dump -U postgres ymca_wellness_dapp \
  > /datadrive/backup-ymca-$(date +%F).sql
```

---

## Part 2 — After a reboot

A reboot brings back the containers and nginx but **not** the dApp, and
it can leave the Rubix network unable to sign.

### What returns on its own

- All Docker containers (`--restart unless-stopped`)
- nginx (systemd)
- Everything on disk under `/datadrive`

### What does not

- **The dApp**, if it runs under `screen` — screen sessions die with the
  machine. See [Part 3](#part-3--running-the-service).
- **Rubix quorum peer mappings.** Nodes restart without them, so every
  on-chain write fails while reads keep working.

### Post-reboot checks, in order

```bash
# 1. containers up?
docker ps --filter name=node --format '{{.Names}}\t{{.Status}}' | sort
docker ps --filter name=ymca-pg --format '{{.Status}}'

# 2. Postgres mounted correctly — want ONE line, /datadrive/pgdata-ymca
docker inspect ymca-pg -f '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'

# 3. database present
docker exec ymca-pg psql -U postgres -lqt | cut -d'|' -f1 | grep ymca

# 4. start the dApp (Part 3), then:
curl -s https://yqa.rubix.network/api/health
```

### The check that actually matters

`/api/health` returning `admins:10` proves only that the database is
readable. After a reboot the chain is usually the broken part, and no
read-only endpoint will reveal it. Test a **write**:

```bash
BASE=https://yqa.rubix.network
TOKEN=$(curl -sS -X POST "$BASE/api/auth/token" \
  -H 'Content-Type: application/json' \
  -d '{"email":"<operator email>","password":"<password>"}' | jq -r '.access_token')

curl -s -X POST "$BASE/api/activity/add" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"admin_did":"<any admin did>","activity_id":"healthcheck","reward_points":1}' \
  | jq -c '{status, message}'
```

`status: true` with a transaction id means the chain works.

Two failures seen in practice, both fixed on the Rubix side:

- `No quorums available for transaction` — quorum not configured on the
  nodes after restart.
- `peer ID not found for DID: …` — a quorum member is listed but its peer
  mapping is missing. Check whether the DID is even one of your ten:
  `grep <did> /datadrive/rubix-docker/did_results.json`.

---

## Part 3 — Running the service

### Option A: screen (ad-hoc)

```bash
screen -S dapp
cd /datadrive/ymca-wellness-dapp
./ymca-dapp
# Ctrl-A then D to detach
```

`screen -r dapp` to reattach, `screen -ls` to list. The `cd` is required
— the binary resolves `.env`, `keys/`, `dids/`, and `artifacts/` relative
to its working directory.

Does not survive a reboot.

### Option B: systemd (recommended)

Survives reboots and restarts on crash. Stop the screen session first so
both do not fight over port 9100.

```bash
sudo tee /etc/systemd/system/ymca-dapp.service >/dev/null <<'EOF'
[Unit]
Description=YMCA Wellness dApp
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
User=rubix
WorkingDirectory=/datadrive/ymca-wellness-dapp
ExecStart=/datadrive/ymca-wellness-dapp/ymca-dapp
Restart=always
RestartSec=10
Environment=GIN_MODE=release

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now ymca-dapp
systemctl status ymca-dapp --no-pager
journalctl -u ymca-dapp -f
```

`WorkingDirectory` is essential for the same reason as the `cd` above.
`Restart=always` also covers the case where Postgres is not ready yet at
boot — the dApp exits and retries until the database answers.

### Deploying a code change

```bash
cd /datadrive/ymca-wellness-dapp
git pull
go build -o ymca-dapp ./cmd/server && echo BUILD OK
sudo systemctl restart ymca-dapp     # or Ctrl-C and rerun inside screen
curl -s https://yqa.rubix.network/api/health
```

---

## Part 4 — nginx and TLS

`yqa.rubix.network` terminates TLS and proxies to `127.0.0.1:9100`.
Config lives at `/etc/nginx/sites-available/yqa.rubix.network`, with a
Certbot-managed certificate.

The proxy timeouts are raised deliberately:

```nginx
proxy_read_timeout 300s;
proxy_send_timeout 300s;
```

Chain writes block on quorum signing, and nginx's 60 s default cuts them
off mid-transaction.

After editing: `sudo nginx -t && sudo systemctl reload nginx`.

---

## Known gaps

- **The FT leg is disabled** on `main` (`internal/service/service.go`,
  `ProcessTransferReward`). Payouts settle with `status: success` and a
  real transaction id, and the audit record reaches the chain, but no
  `ytoken` balance moves. Branch `feat/enable-ft-transfer` turns it on;
  it needs `FT_CREATOR_DID` set and the FT minted and distributed first.
- **Registration is open.** Any account created via
  `POST /api/auth/users` holds the full API surface, since `operator` is
  the only role. Appropriate for a test deployment only.
- **Port 9100 may still be reachable directly** on the VM's public IP,
  bypassing TLS. Close it in the cloud firewall so traffic only arrives
  via 443.
- **Node container volumes** — check whether the ten `node*-postgres`
  containers use bind mounts or anonymous volumes. Anonymous volumes lose
  the DIDs and RBT whenever a container is recreated, and unlike the
  dApp's data there is no backup to restore from:

  ```bash
  docker inspect node1-postgres -f '{{range .Mounts}}{{.Type}} {{.Source}}{{end}}'
  ```
