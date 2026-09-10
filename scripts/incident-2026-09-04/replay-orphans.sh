#!/usr/bin/env bash
# replay-orphans.sh — bring the 5 drifted owners forward by the block their quorum
# committed, then align the 5 matching dApp rows.
#   bash replay-orphans.sh            dry run: every guard runs, everything rolls back
#   bash replay-orphans.sh --apply    commit, then restart the 5 owner nodes
set -u
MODE=dryrun; [ "${1:-}" = "--apply" ] && MODE=apply
END=ROLLBACK; [ $MODE = apply ] && END=COMMIT
echo "MODE=$MODE"
[ -f /datadrive/orphans.tgz ] || { echo "missing /datadrive/orphans.tgz"; exit 1; }
rm -rf /datadrive/orphans && tar xzf /datadrive/orphans.tgz -C /datadrive && ls /datadrive/orphans

for N in 2 4 7 8 9; do
  CSV=/datadrive/orphans/orphan-node${N}.csv
  echo; echo "################ node$N  ($MODE)"
  [ "$(wc -l < "$CSV")" = 1 ] || { echo "!! $CSV must have exactly 1 line"; continue; }
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
  SELECT token_id, transaction_id, latest_position, token_status, latest_role INTO t FROM tokens WHERE token_id = o.sc;
  IF t.token_id IS NULL THEN RAISE EXCEPTION 'contract % not on this node', o.sc; END IF;
  IF t.transaction_id <> o.prev THEN RAISE EXCEPTION 'owner head % <> block prev %', t.transaction_id, o.prev; END IF;
  IF t.token_status <> 11 THEN RAISE EXCEPTION 'contract status is %, expected 11', t.token_status; END IF;
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
python3 - "$MODE" <<'PY'
import csv, json, subprocess, sys, glob
mode = sys.argv[1]
def psql(sql):
    return subprocess.run(["docker","exec","-i","ymca-pg","psql","-U","postgres","-d","ymca_wellness_dapp","-At","-v","ON_ERROR_STOP=1"],
                          input=sql, capture_output=True, text=True)
for f in sorted(glob.glob('/datadrive/orphans/orphan-node*.csv')):
    tid, info, sig = next(csv.reader(open(f)))
    j = json.loads(info); sc = j['tokens']['smartContract'][0]; d = json.loads(sc['data'])
    arr = "ARRAY[" + ",".join("'%s'" % a.replace("'", "''") for a in d['activity_ids']) + "]::text[]"
    where = (f"kind='reward' AND status='failed' AND admin_did='{j['initiator']}' AND user_did='{d['user_did']}' "
             f"AND activity_ids = {arr} AND error_details LIKE 'sign:%EOF%' "
             f"AND created_at BETWEEN to_timestamp({j['epoch']}) - interval '20 seconds' AND to_timestamp({j['epoch']}) + interval '20 seconds'")
    r = psql(f"SELECT request_id||' '||created_at||' pts='||reward_points FROM transfer_status WHERE {where};")
    rows = [x for x in r.stdout.splitlines() if x.strip()]
    tag = f.split('/')[-1]
    if r.returncode != 0 or len(rows) != 1:
        print(f"{tag}: !! expected exactly 1 matching failed row, got {len(rows)} {r.stderr.strip()} {rows}"); continue
    rid = rows[0].split()[0]
    print(f"{tag}: match {rows[0]} user={d['user_did']} activities={d['activity_ids']} pts={d['reward_points']} tx={tid[:12]}..")
    if mode == 'apply':
        note = f"replayed 2026-09-07: owner node crashed after quorum consensus on 4 Sep; block {tid} restored from quorum copy"
        u = psql(f"UPDATE transfer_status SET status='success', transaction_id='{tid}', contract_hash='{sc['tokenId']}', "
                 f"reward_points={int(d['reward_points'])}, message='transferred {int(d['reward_points'])} ytoken to {d['user_did']}', "
                 f"error_details='{note}', updated_at=NOW() WHERE request_id='{rid}' AND status='failed';")
        print(f"   {u.stdout.strip() or u.stderr.strip()}")
PY

if [ $MODE = apply ]; then
  echo; echo "################ restarting the five owners"
  docker restart node2-node node4-node node7-node node8-node node9-node
  sleep 25; docker ps --format '{{.Names}} {{.Status}}' | grep -E '^node(2|4|7|8|9)-node'
fi
