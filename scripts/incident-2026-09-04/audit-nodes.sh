#!/usr/bin/env bash
# audit-nodes.sh — READ-ONLY sweep of the 10 Rubix nodes. Makes no changes.
set -u
PG="docker exec ymca-pg psql -U postgres -d ymca_wellness_dapp -At -F |"
EXPECTED=$($PG -c "SELECT a.node_port, ac.contract_kind, ac.contract_hash FROM admin_contracts ac JOIN admins a ON a.did=ac.admin_did ORDER BY 1,2")
echo "== dApp admin_contracts: $(echo "$EXPECTED" | grep -c .) rows expected across $(echo "$EXPECTED" | cut -d'|' -f1 | sort -u | wc -l) ports"
echo "== tokens table columns (node1)"
docker exec node1-postgres psql -U rubix -d rubix -At -c "SELECT string_agg(column_name||':'||data_type, ', ' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_name='tokens'"

for N in $(seq 1 10); do
  PORT=$((7999+N))
  echo; echo "################ node$N  (host port $PORT)"
  docker ps -a --filter "name=^node${N}-" --format '  {{.Names}}  {{.Status}}  {{.Ports}}'
  code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://localhost:$PORT/api/get-all-quorum")
  [ "$code" = "000" ] && code="REFUSED/TIMEOUT"
  echo "  API :$PORT -> $code ; host listeners on :$PORT = $(ss -ltn "sport = :$PORT" | tail -n +2 | wc -l)"
  NPG="docker exec node${N}-postgres psql -U rubix -d rubix -At -F |"
  if ! $NPG -c "SELECT 1" >/dev/null 2>&1; then echo "  !! node${N}-postgres NOT QUERYABLE"; continue; fi
  echo "  -- histogram: token_status|latest_role|rows|rows_with_lock"
  $NPG -c "SELECT token_status, latest_role, count(*), count(lock_reference_id) FROM tokens GROUP BY 1,2 ORDER BY 1,2" | sed 's/^/     /'
  echo "  -- expected contracts: kind|status|lock_ref|role|value|updated_at"
  echo "$EXPECTED" | awk -F'|' -v p="$PORT" '$1==p' | while IFS='|' read -r _ kind hash; do
    row=$($NPG -c "SELECT token_status, coalesce(lock_reference_id,'-'), latest_role, token_value, updated_at FROM tokens WHERE token_id='$hash'")
    echo "     $kind|${hash:0:14}..|${row:-MISSING}"
  done
  echo "  -- locked or non-11 rows (max 25): token_id|status|lock_ref|role|value|updated_at"
  $NPG -c "SELECT token_id, token_status, coalesce(lock_reference_id,'-'), latest_role, token_value, updated_at FROM tokens WHERE lock_reference_id IS NOT NULL OR token_status <> 11 ORDER BY updated_at DESC LIMIT 25" | sed 's/^/     /'
done
