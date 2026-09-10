#!/usr/bin/env bash
# morning-check.sh — READ-ONLY health check for the yqa fleet after the 7 Sep 2026 repair.
#   bash morning-check.sh '2026-09-07 11:30'      # UTC; defaults to 12 hours ago
# A non-zero "mismatch" column or a non-zero panic count means a chain has forked again.
set -u
SINCE="${1:-$(date -u -d '12 hours ago' '+%Y-%m-%d %H:%M')}"
SINCE_ISO=$(date -u -d "$SINCE" +%Y-%m-%dT%H:%M:%SZ)
PG="docker exec ymca-pg psql -U postgres -d ymca_wellness_dapp"
Q() { docker exec "node${1}-postgres" psql -U rubix -d rubix -At -F '|' -c "$2"; }

echo "== window: since $SINCE UTC"
echo "== node containers not reporting healthy"
docker ps -a --filter name=node --format '{{.Names}} {{.Status}}' | sort -V | grep -v healthy || echo "   none, all healthy"

echo "== payouts per node in the window (ok / failed / chain-mismatch / first mismatch / in flight)"
$PG -c "SELECT a.node_port, count(*) FILTER (WHERE ts.status='success') AS ok, count(*) FILTER (WHERE ts.status='failed') AS failed, count(*) FILTER (WHERE ts.error_details LIKE '%chain mismatch%') AS mismatch, min(ts.created_at) FILTER (WHERE ts.error_details LIKE '%chain mismatch%') AS first_mismatch, count(*) FILTER (WHERE ts.status IN ('queued','processing')) AS inflight FROM transfer_status ts JOIN admins a ON a.did=ts.admin_did WHERE ts.kind='reward' AND ts.created_at >= '$SINCE' GROUP BY 1 ORDER BY 1;"

echo "== distinct failure reasons in the window (top 8)"
$PG -c "SELECT count(*) AS n, left(regexp_replace(error_details, '[0-9a-f]{64}|bafybmi[a-z0-9]+|127\.0\.0\.1:[0-9]+', '#', 'g'), 110) AS reason FROM transfer_status WHERE kind='reward' AND status='failed' AND created_at >= '$SINCE' GROUP BY reason ORDER BY n DESC LIMIT 8;"

echo "== node process panics / relaunches in the window"
for N in $(seq 1 10); do
  c=$(docker logs --since "$SINCE_ISO" node${N}-node 2>&1 | grep -c -E '^panic:|Starting Rubix node')
  echo "   node$N: $c"
done

echo "== owner chain consistency per reward contract (tokenchain = index_len = joined; status 11/3 or 10/4)"
$PG -At -F '|' -c "SELECT a.node_port, ac.contract_hash FROM admin_contracts ac JOIN admins a ON a.did=ac.admin_did WHERE ac.contract_kind='reward' ORDER BY 1" | while IFS='|' read -r PORT SC; do
  N=$((PORT-7999))
  echo "   node$N: $(Q $N "SELECT 'tokenchain='||(SELECT count(*) FROM tokenchain WHERE token_id='$SC')||' index_len='||coalesce((SELECT array_length(index,1) FROM tokenchain_index WHERE token_id='$SC'),0)||' joined='||(SELECT count(*) FROM tokenchain tc JOIN transactions t ON t.id=tc.transaction_id WHERE tc.token_id='$SC')||' head_pos='||latest_position||' status='||token_status||'/'||latest_role||' lock='||coalesce(lock_reference_id,'-') FROM tokens WHERE token_id='$SC'")"
done
