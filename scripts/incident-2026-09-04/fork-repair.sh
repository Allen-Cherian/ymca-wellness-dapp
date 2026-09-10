#!/usr/bin/env bash
# fork-repair.sh — detect and repair "chain mismatch" forks between yqa owner nodes and
# their quorums (owner crashed after the quorum committed; quorum is one block ahead).
#
# DISTRIBUTE VIA GIT. Never paste this file into a terminal: it contains heredocs.
# Run as user rubix. Every write is guarded and backed up (pg_dump) first.
#
#   yqa VM:
#     fork-repair.sh detect              -> $WORK/forks.txt  (N:lostTxId per forked node)
#     fork-repair.sh apply [--dry-run]   -> replay each lost block on its owner, align dApp row,
#                                           restart the repaired nodes (dry-run: guards only)
#     fork-repair.sh verify              -> one real payout through each repaired admin
#     fork-repair.sh auto [--dry-run]    -> detect + export over ssh + apply + verify + pledges
#   quorum VM (directly, or over ssh from `auto`):
#     fork-repair.sh export  <forks.txt> -> $WORK/orphans.tgz (the quorum's transaction rows)
#     fork-repair.sh pledges <forks.txt> -> confirm pledges released after `verify`
#
# Env:  WORK=/datadrive/fork-repair (default)   QUORUM_HOST=rubix@<quorum-vm>  (auto only)
#       DAPP_ENV=/datadrive/ymca-wellness-dapp/.env (BOOTSTRAP_EMAIL/PASSWORD for verify)
set -u
WORK=${WORK:-/datadrive/fork-repair}
DAPP_ENV=${DAPP_ENV:-/datadrive/ymca-wellness-dapp/.env}
PORTS="8000 8001 8002 8003 8004 8005 8006 8007 8008 8009"
mkdir -p "$WORK" 2>/dev/null || { WORK=/tmp/fork-repair; mkdir -p "$WORK"; }

PG="docker exec ymca-pg psql -U postgres -d ymca_wellness_dapp -At -F |"
npg() { docker exec "node${1}-postgres" psql -U rubix -d rubix -At -F '|' -c "$2"; }
die() { echo "!! $*" >&2; exit 1; }
# yqa is the host running the dApp database container; the quorum VM has no ymca-pg.
is_yqa_vm() { docker ps --format '{{.Names}}' | grep -qx ymca-pg; }
is_quorum_vm() { ! is_yqa_vm; }
nodes_in() { cut -d: -f1 "$1" | tr '\n' ' '; }

# ---------------------------------------------------------------- detect (yqa)
cmd_detect() {
  is_quorum_vm && die "detect runs on the yqa VM, not the quorum VM"
  local OUT="$WORK/forks.txt"; : > "$OUT"
  local J="FROM transfer_status ts JOIN admins a ON a.did=ts.admin_did WHERE ts.kind='reward' AND a.node_port="
  for PORT in $PORTS; do
    local N=$((PORT-7999))
    local LASTOK LASTMM ERR QL EX SC HEAD
    LASTOK=$($PG -c "SELECT coalesce(max(ts.created_at)::text,'-') $J'$PORT' AND ts.status='success'")
    LASTMM=$($PG -c "SELECT coalesce(max(ts.created_at)::text,'-') $J'$PORT' AND ts.error_details LIKE '%chain mismatch%'")
    if [ "$LASTMM" = "-" ]; then echo "node$N: no mismatch ever"; continue; fi
    if [ "$LASTOK" != "-" ] && [[ "$LASTMM" < "$LASTOK" ]]; then echo "node$N: healthy (last ok $LASTOK is after last mismatch)"; continue; fi
    ERR=$($PG -c "SELECT ts.error_details $J'$PORT' AND ts.error_details LIKE '%chain mismatch%' ORDER BY ts.created_at DESC LIMIT 1")
    QL=$(echo "$ERR" | grep -oE 'local latest [0-9a-f]{64}' | awk '{print $3}')
    EX=$(echo "$ERR" | grep -oE 'expected [0-9a-f]{64}' | awk '{print $2}')
    SC=$($PG -c "SELECT ac.contract_hash FROM admin_contracts ac JOIN admins a ON a.did=ac.admin_did WHERE a.node_port='$PORT' AND ac.contract_kind='reward'")
    HEAD=$(npg $N "SELECT transaction_id FROM tokens WHERE token_id='$SC'")
    if [ "$HEAD" = "$EX" ]; then
      echo "node$N: FORKED  owner head ${HEAD:0:12} quorum head ${QL:0:12}  (last mismatch $LASTMM)"
      echo "$N:$QL" >> "$OUT"
    else
      echo "node$N: MISMATCH BUT owner head ${HEAD:0:12} != expected ${EX:0:12} -> investigate by hand, not repairing"
    fi
  done
  echo "forked nodes: $(nodes_in "$OUT")(list in $OUT)"
}

# ---------------------------------------------------------------- export (quorum VM)
cmd_export() {
  local LIST=${1:-}; [ -f "$LIST" ] || die "usage: export <forks.txt>"
  is_quorum_vm || die "export runs on the quorum VM (this host has ymca-pg, so it is yqa)"
  local DIR="$WORK/orphans"; rm -rf "$DIR"; mkdir -p "$DIR"
  while IFS=: read -r N TX; do
    [ -z "$N" ] && continue
    docker exec node${N}-postgres psql -U rubix -d rubix -At -c "COPY (SELECT id, info::text, signature::text FROM transactions WHERE id='$TX') TO STDOUT WITH (FORMAT csv)" > "$DIR/orphan-node${N}.csv"
    local ROW; ROW=$(docker exec node${N}-postgres psql -U rubix -d rubix -At -c "SELECT position||' prev='||left(previous_transaction_id,12) FROM tokenchain WHERE transaction_id='$TX' AND token_id LIKE 'Qm%'")
    echo "node$N: $(wc -l < "$DIR/orphan-node$N.csv") row(s); quorum chain row: $ROW"
  done < "$LIST"
  python3 - "$DIR" <<'PY'
import csv, hashlib, json, glob, sys
bad = 0
for f in sorted(glob.glob(sys.argv[1] + '/orphan-node*.csv')):
    rows = list(csv.reader(open(f)))
    if len(rows) != 1:
        print(f"{f.split('/')[-1]}: !! {len(rows)} rows"); bad += 1; continue
    tid, info, sig = rows[0]
    j = json.loads(info); sc = j['tokens']['smartContract'][0]
    ok = hashlib.sha3_256(info.encode()).hexdigest() == tid
    bad += 0 if ok else 1
    print(f"{f.split('/')[-1]}: hash_ok={ok} initiator={j['initiator'][:22]}.. sc={sc['tokenId'][:14]}.. prev={sc['previousTransactionID'][:12]}..")
sys.exit(1 if bad else 0)
PY
  [ $? -eq 0 ] || die "export verification failed"
  tar czf "$WORK/orphans.tgz" -C "$WORK" orphans && echo "wrote $WORK/orphans.tgz"
}

# ---------------------------------------------------------------- apply (yqa)
cmd_apply() {
  is_quorum_vm && die "apply runs on the yqa VM"
  local MODE=apply END=COMMIT; [ "${1:-}" = "--dry-run" ] && { MODE=dryrun; END=ROLLBACK; }
  local LIST="$WORK/forks.txt"; [ -s "$LIST" ] || die "no $LIST; run detect first"
  [ -f "$WORK/orphans.tgz" ] || die "no $WORK/orphans.tgz; run export on the quorum VM and copy it here"
  rm -rf "$WORK/orphans" && tar xzf "$WORK/orphans.tgz" -C "$WORK"
  echo "MODE=$MODE nodes: $(nodes_in "$LIST")"
  local TS; TS=$(date +%Y%m%d-%H%M)
  for N in $(nodes_in "$LIST"); do
    local CSV="$WORK/orphans/orphan-node${N}.csv"
    echo; echo "################ node$N ($MODE)"
    [ -f "$CSV" ] && [ "$(wc -l < "$CSV")" = 1 ] || { echo "!! $CSV missing or not exactly 1 line; skipping"; continue; }
    if [ $MODE = apply ]; then
      docker exec node${N}-postgres pg_dump -U rubix -d rubix > "$WORK/backup-node${N}-rubix-$TS.sql" && echo "backup $WORK/backup-node${N}-rubix-$TS.sql"
    fi
    docker cp "$CSV" node${N}-postgres:/tmp/orphan.csv
    docker exec -i node${N}-postgres psql -U rubix -d rubix -v ON_ERROR_STOP=1 -q <<SQL
BEGIN;
CREATE TEMP TABLE orphan (id text, info text, signature text);
COPY orphan FROM '/tmp/orphan.csv' WITH (FORMAT csv);
DO \$\$
DECLARE n int; o record; t record;
BEGIN
  SELECT count(*) INTO n FROM orphan;
  IF n <> 1 THEN RAISE EXCEPTION 'orphan rows = %', n; END IF;
  SELECT id, info::json->>'initiator' AS initiator,
         info::json->'tokens'->'smartContract'->0->>'tokenId' AS sc,
         info::json->'tokens'->'smartContract'->0->>'previousTransactionID' AS prev INTO o FROM orphan;
  SELECT token_id, transaction_id, latest_position, token_status INTO t FROM tokens WHERE token_id = o.sc;
  IF t.token_id IS NULL THEN RAISE EXCEPTION 'contract % not on this node', o.sc; END IF;
  IF t.transaction_id <> o.prev THEN RAISE EXCEPTION 'owner head % <> block prev %', t.transaction_id, o.prev; END IF;
  IF t.token_status <> 11 THEN RAISE EXCEPTION 'contract status is %, expected 11 (locked? restart the node first)', t.token_status; END IF;
  IF EXISTS (SELECT 1 FROM tokenchain WHERE transaction_id = o.id) THEN RAISE EXCEPTION 'tx % already on chain', o.id; END IF;
  IF NOT EXISTS (SELECT 1 FROM dids WHERE did = o.initiator) THEN RAISE EXCEPTION 'initiator % unknown here', o.initiator; END IF;
  RAISE NOTICE 'guards ok: sc=% head_pos=% head=% -> new tx=%', o.sc, t.latest_position, left(t.transaction_id,12), left(o.id,12);
END \$\$;
INSERT INTO transactions (id, info, signature, created_at, updated_at)
  SELECT id, info::json, signature::jsonb, NOW(), NOW() FROM orphan ON CONFLICT (id) DO NOTHING;
INSERT INTO transaction_units (transaction_id, did, execution_role, status, created_at, updated_at)
  SELECT id, info::json->>'initiator', 'initiator', 'committed', NOW(), NOW() FROM orphan ON CONFLICT (transaction_id, did) DO NOTHING;
INSERT INTO tokenchain (token_id, transaction_id, previous_transaction_id, role, position, created_at, updated_at)
  SELECT t.token_id, o.id, t.transaction_id, 3, t.latest_position + 1, NOW(), NOW()
  FROM orphan o JOIN tokens t ON t.token_id = o.info::json->'tokens'->'smartContract'->0->>'tokenId';
INSERT INTO tokenchain_index (token_id, index, created_at, updated_at)
  SELECT tc.token_id, array_agg(tc.id ORDER BY tc.position), NOW(), NOW() FROM tokenchain tc
  WHERE tc.token_id = (SELECT info::json->'tokens'->'smartContract'->0->>'tokenId' FROM orphan) GROUP BY tc.token_id
  ON CONFLICT (token_id) DO UPDATE SET index = EXCLUDED.index, updated_at = NOW();
UPDATE tokens t SET transaction_id = o.id, latest_position = t.latest_position + 1, latest_role = 3, token_status = 11,
  did = o.info::json->>'initiator', lock_reference_id = NULL, updated_at = NOW()
  FROM orphan o
  WHERE t.token_id = o.info::json->'tokens'->'smartContract'->0->>'tokenId'
    AND t.transaction_id = o.info::json->'tokens'->'smartContract'->0->>'previousTransactionID';
SELECT 'after: tokenchain='||(SELECT count(*) FROM tokenchain WHERE token_id=t.token_id)
     ||' index_len='||(SELECT array_length(index,1) FROM tokenchain_index WHERE token_id=t.token_id)
     ||' joined='||(SELECT count(*) FROM tokenchain tc JOIN transactions x ON x.id=tc.transaction_id WHERE tc.token_id=t.token_id)
     ||' head_pos='||t.latest_position||' head='||left(t.transaction_id,12)||' status='||t.token_status||'/'||t.latest_role
     ||' served_latest='||left((SELECT transaction_id FROM tokenchain WHERE id=(SELECT index[array_upper(index,1)] FROM tokenchain_index WHERE token_id=t.token_id)),12)
  FROM tokens t WHERE t.token_id = (SELECT info::json->'tokens'->'smartContract'->0->>'tokenId' FROM orphan);
$END;
SQL
    echo "node$N exit=$?"
  done

  echo; echo "################ dApp alignment ($MODE)"
  python3 - "$MODE" "$WORK/orphans" <<'PY'
import csv, json, subprocess, sys, glob
mode, d = sys.argv[1], sys.argv[2]
def psql(sql):
    return subprocess.run(["docker","exec","-i","ymca-pg","psql","-U","postgres","-d","ymca_wellness_dapp","-At","-v","ON_ERROR_STOP=1"],
                          input=sql, capture_output=True, text=True)
for f in sorted(glob.glob(d + '/orphan-node*.csv')):
    tid, info, sig = next(csv.reader(open(f)))
    j = json.loads(info); sc = j['tokens']['smartContract'][0]; data = json.loads(sc['data'])
    arr = "ARRAY[" + ",".join("'%s'" % a.replace("'", "''") for a in data['activity_ids']) + "]::text[]"
    where = (f"kind='reward' AND status='failed' AND admin_did='{j['initiator']}' AND user_did='{data['user_did']}' "
             f"AND activity_ids = {arr} AND error_details LIKE 'sign:%' "
             f"AND created_at BETWEEN to_timestamp({j['epoch']}) - interval '120 seconds' AND to_timestamp({j['epoch']}) + interval '120 seconds'")
    r = psql(f"SELECT request_id||' '||created_at FROM transfer_status WHERE {where} ORDER BY created_at;")
    rows = [x for x in r.stdout.splitlines() if x.strip()]
    tag = f.split('/')[-1]
    if r.returncode != 0 or len(rows) != 1:
        print(f"{tag}: !! expected exactly 1 matching failed row, got {len(rows)} {r.stderr.strip()} {rows} -> align by hand (tx {tid})"); continue
    rid = rows[0].split()[0]
    print(f"{tag}: match request {rid} user={data['user_did']} activities={data['activity_ids']} pts={data['reward_points']} tx={tid[:12]}..  (add to do-not-retry list)")
    if mode == 'apply':
        note = f"replayed {__import__('datetime').date.today()}: owner node crashed after quorum consensus; block {tid} restored from quorum copy"
        u = psql(f"UPDATE transfer_status SET status='success', transaction_id='{tid}', contract_hash='{sc['tokenId']}', "
                 f"reward_points={int(data['reward_points'])}, message='transferred {int(data['reward_points'])} ytoken to {data['user_did']}', "
                 f"error_details='{note}', updated_at=NOW() WHERE request_id='{rid}' AND status='failed';")
        print(f"   {u.stdout.strip() or u.stderr.strip()}")
PY

  if [ $MODE = apply ]; then
    echo; echo "################ restarting repaired owners"
    local R=""; for N in $(nodes_in "$LIST"); do R="$R node${N}-node"; done
    docker restart $R
    sleep 25; for N in $(nodes_in "$LIST"); do docker ps --format '{{.Names}} {{.Status}}' | grep "^node${N}-node "; done
  fi
}

# ---------------------------------------------------------------- verify (yqa)
cmd_verify() {
  is_quorum_vm && die "verify runs on the yqa VM"
  local LIST="$WORK/forks.txt"; [ -s "$LIST" ] || die "no $LIST"
  local EMAIL PASS TOK BASE=http://localhost:9100
  EMAIL=$(grep '^BOOTSTRAP_EMAIL=' "$DAPP_ENV" | cut -d= -f2-); PASS=$(grep '^BOOTSTRAP_PASSWORD=' "$DAPP_ENV" | cut -d= -f2-)
  [ -n "$EMAIL" ] && [ -n "$PASS" ] || die "BOOTSTRAP_EMAIL/PASSWORD not found in $DAPP_ENV"
  TOK=$(curl -s -X POST $BASE/api/auth/token -H 'Content-Type: application/json' -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" | jq -r '.access_token // empty')
  [ -n "$TOK" ] || die "login failed; is the dApp up on :9100?"
  local USER=bafybmie45toyrjyh5qpd5vtpkq6vk3ukes7ieta2ip2irpei3xu2ylz7kq
  for N in $(nodes_in "$LIST"); do
    local PORT=$((7999+N)) A ACT R S
    A=$($PG -c "SELECT did FROM admins WHERE node_port='$PORT'")
    ACT=$($PG -c "SELECT activity_id FROM activities WHERE admin_did='$A' ORDER BY created_at LIMIT 1")
    [ -n "$ACT" ] || { echo "node$N: admin has no activities; cannot verify"; continue; }
    R=$(curl -s -X POST $BASE/admin/payouts -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
        -d "{\"user_did\":\"$USER\",\"admin_did\":\"$A\",\"activity_id\":[\"$ACT\"]}" | jq -r '.result.request_id // empty')
    [ -n "$R" ] || { echo "node$N: payout not accepted"; continue; }
    for i in 1 2 3 4 5 6; do
      sleep 5
      S=$(curl -s $BASE/admin/payouts/status/$R -H "Authorization: Bearer $TOK" | jq -c '{status:.result.status, tx:.result.blockchain_tx_id, err:(.result.error_details|.[0:120])}')
      case "$S" in *'"success"'*|*'"failed"'*) break;; esac
    done
    echo "node$N (activity $ACT, request $R): $S"
  done
}

# ---------------------------------------------------------------- pledges (quorum VM)
cmd_pledges() {
  local LIST=${1:-}; [ -f "$LIST" ] || die "usage: pledges <forks.txt>"
  is_quorum_vm || die "pledges runs on the quorum VM"
  while IFS=: read -r N TX; do
    [ -z "$N" ] && continue
    local SC U H
    SC=$(docker exec node$N-postgres psql -U rubix -d rubix -At -c "SELECT token_id FROM tokenchain WHERE transaction_id='$TX' AND token_id LIKE 'Qm%'")
    U=$(docker exec node$N-postgres psql -U rubix -d rubix -At -c "SELECT count(*) FROM unpledge_sequence_info WHERE tx_id='$TX'")
    H=$(docker exec node$N-postgres psql -U rubix -d rubix -At -c "SELECT position||' '||left(transaction_id,12) FROM tokenchain WHERE token_id='$SC' ORDER BY position DESC LIMIT 1")
    echo "node$N: pending unpledge rows=$U (want 0) ; quorum contract head=$H"
  done < "$LIST"
}

# ---------------------------------------------------------------- auto (yqa, needs ssh to quorum VM)
cmd_auto() {
  is_quorum_vm && die "auto runs on the yqa VM"
  [ -n "${QUORUM_HOST:-}" ] || die "set QUORUM_HOST=rubix@<quorum-vm> (ssh key access required)"
  if systemctl is-active --quiet nginx; then die "nginx is active: stop it first (sudo systemctl stop nginx) so no payouts run during the repair"; fi
  cmd_detect
  [ -s "$WORK/forks.txt" ] || { echo "nothing to repair"; return 0; }
  local SELF; SELF=$(readlink -f "$0")
  ssh "$QUORUM_HOST" "mkdir -p /tmp/fork-repair"
  scp -q "$SELF" "$WORK/forks.txt" "$QUORUM_HOST:/tmp/fork-repair/"
  ssh "$QUORUM_HOST" "WORK=/tmp/fork-repair bash /tmp/fork-repair/fork-repair.sh export /tmp/fork-repair/forks.txt" || die "export failed on quorum VM"
  scp -q "$QUORUM_HOST:/tmp/fork-repair/orphans.tgz" "$WORK/orphans.tgz"
  cmd_apply "${1:-}"
  [ "${1:-}" = "--dry-run" ] && { echo "dry run complete; re-run without --dry-run to apply"; return 0; }
  echo; echo "################ verify"; cmd_verify
  echo; echo "################ pledges (quorum VM)"
  ssh "$QUORUM_HOST" "bash /tmp/fork-repair/fork-repair.sh pledges /tmp/fork-repair/forks.txt"
  echo; echo "Done. Give the testers the request ids printed under 'dApp alignment' as do-not-retry."
}

case "${1:-}" in
  detect)  cmd_detect ;;
  export)  cmd_export "${2:-}" ;;
  apply)   cmd_apply "${2:-}" ;;
  verify)  cmd_verify ;;
  pledges) cmd_pledges "${2:-}" ;;
  auto)    cmd_auto "${2:-}" ;;
  *) sed -n '2,20p' "$0"; exit 2 ;;
esac
