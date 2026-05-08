#!/usr/bin/env bash
#
# deploy-contracts.sh — generate + deploy + sign smart contracts for every
# admin in the `admins` table, recording results in `admin_contracts`.
#
# For each (admin, kind) pair the script runs the 3-call Rubix flow:
#   1. POST /rubix/v1/smart_contracts/generate  (multipart, sync)
#   2. POST /rubix/v1/tx                        (returns id needing signature)
#   3. POST /rubix/v1/signature                 (completes deploy)
# Then UPSERTs (admin_did, kind, contract_token) into admin_contracts.
#
# Idempotent: rows already present in admin_contracts are skipped.
# Best-effort: a single (admin, kind) failure does not stop the run.

set -uo pipefail

# ---------------------------------------------------------------- defaults
ARTIFACTS_DIR_DEFAULT="./artifacts"
KINDS=("reward" "add_activity" "add_admin")

# Per-kind .wasm filename. macOS ships Bash 3.2 (no associative arrays),
# so we use a case statement instead of `declare -A`.
# The .rs file is always lib.rs.
RS_FILENAME="lib.rs"

wasm_for_kind() {
  case "$1" in
    reward)       echo "reward_transfer.wasm" ;;
    add_activity) echo "activity_contract.wasm" ;;
    add_admin)    echo "add_admin_contract.wasm" ;;
    *)            echo "" ;;
  esac
}

NODE_HOST="http://localhost"
SIGN_PATH="/rubix/v1/signature"

DB_HOST="${DB_HOST:-localhost}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-postgres}"
DB_PASSWORD="${DB_PASSWORD:-postgres}"
DB_NAME="${DB_NAME:-ymca_wellness_cafe_v2}"

# ---------------------------------------------------------------- args
ARTIFACTS_DIR="$ARTIFACTS_DIR_DEFAULT"

usage() {
  cat <<EOF
Usage: $0 [--artifacts-dir <path>]

Options:
  --artifacts-dir <path>  Directory containing per-kind subdirs.
                          Default: $ARTIFACTS_DIR_DEFAULT

Expected layout:
  <artifacts-dir>/reward/reward_transfer.wasm
  <artifacts-dir>/reward/lib.rs
  <artifacts-dir>/add_activity/activity_contract.wasm
  <artifacts-dir>/add_activity/lib.rs
  <artifacts-dir>/add_admin/add_admin_contract.wasm
  <artifacts-dir>/add_admin/lib.rs

  -h, --help              Show this message.

Env overrides for Postgres connection:
  DB_HOST DB_PORT DB_USER DB_PASSWORD DB_NAME
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --artifacts-dir) ARTIFACTS_DIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

# ---------------------------------------------------------------- preflight
[[ -d "$ARTIFACTS_DIR" ]] || { echo "ERROR: artifacts dir not found: $ARTIFACTS_DIR" >&2; exit 1; }
for kind in "${KINDS[@]}"; do
  wasm="$ARTIFACTS_DIR/$kind/$(wasm_for_kind "$kind")"
  rs="$ARTIFACTS_DIR/$kind/$RS_FILENAME"
  [[ -f "$wasm" ]] || { echo "ERROR: missing $wasm" >&2; exit 1; }
  [[ -f "$rs"   ]] || { echo "ERROR: missing $rs"   >&2; exit 1; }
done

for tool in jq curl psql; do
  command -v "$tool" >/dev/null || { echo "ERROR: $tool not in PATH" >&2; exit 1; }
done

# ---------------------------------------------------------------- logging
mkdir -p ./logs
LOGFILE="./logs/deploy-$(date +%Y-%m-%d-%H%M%S).log"
echo "deploy-contracts.sh starting at $(date)" >"$LOGFILE"
echo "artifacts_dir=$ARTIFACTS_DIR" >>"$LOGFILE"
for kind in "${KINDS[@]}"; do
  echo "  $kind: $ARTIFACTS_DIR/$kind/$(wasm_for_kind "$kind") + $RS_FILENAME" >>"$LOGFILE"
done

logfile_say()  { echo "$@" >>"$LOGFILE"; }
console_say()  { echo "$@"; }
both_say()     { console_say "$@"; logfile_say "$@"; }

# ---------------------------------------------------------------- db helpers
psql_exec() {
  PGPASSWORD="$DB_PASSWORD" psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" \
    -d "$DB_NAME" -tAc "$1"
}

contract_exists() {
  local did=$1 kind=$2
  local row
  row=$(psql_exec "SELECT contract_hash FROM admin_contracts
                   WHERE admin_did='$did' AND contract_kind='$kind'") || return 1
  [[ -n "$row" ]] && echo "$row"
}

upsert_contract() {
  local did=$1 kind=$2 hash=$3
  psql_exec "INSERT INTO admin_contracts (admin_did, contract_kind, contract_hash)
             VALUES ('$did', '$kind', '$hash')
             ON CONFLICT (admin_did, contract_kind)
             DO UPDATE SET contract_hash = EXCLUDED.contract_hash, deployed_at = NOW();"
}

# ---------------------------------------------------------------- file output
# All admins + their contracts go into a single file. The file is updated
# incrementally after each successful (admin, kind) deploy so it stays
# consistent with the DB even if the run is interrupted.
CONTRACTS_FILE="./contracts.json"

# Ensure the file exists with the expected top-level shape.
if [[ ! -f "$CONTRACTS_FILE" ]]; then
  echo '{"admins": {}, "updated_at": null}' > "$CONTRACTS_FILE"
fi

# write_contract_file sets admins[<did>].contracts[<kind>] = <hash>,
# preserving any other admins/kinds already in the file.
write_contract_file() {
  local did=$1 kind=$2 hash=$3
  local now
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  jq --arg did "$did" --arg kind "$kind" --arg hash "$hash" --arg now "$now" '
       .admins[$did] //= {admin_did: $did, contracts: {}}
     | .admins[$did].contracts[$kind] = $hash
     | .admins[$did].updated_at = $now
     | .updated_at = $now
  ' "$CONTRACTS_FILE" > "${CONTRACTS_FILE}.tmp" && mv "${CONTRACTS_FILE}.tmp" "$CONTRACTS_FILE"
}

# ---------------------------------------------------------------- rubix calls
# All three call helpers print the response to LOGFILE and either return 0
# with the desired field on stdout, or return non-zero with empty stdout.

rubix_generate() {
  local did=$1 port=$2 kind=$3
  local url="${NODE_HOST}:${port}/rubix/v1/smart_contracts/generate"
  local wasm="${ARTIFACTS_DIR}/${kind}/$(wasm_for_kind "$kind")"
  local rs="${ARTIFACTS_DIR}/${kind}/${RS_FILENAME}"
  local resp
  resp=$(curl -s -X POST "$url" \
    -F "did=${did}" \
    -F "binaryCodePath=@${wasm}" \
    -F "rawCodePath=@${rs}")
  logfile_say "[$did/$kind] generate response: $resp"
  local token
  token=$(echo "$resp" | jq -r 'if .status == true then (.result // empty) else empty end')
  [[ -n "$token" ]] || return 1
  echo "$token"
}

rubix_deploy_tx() {
  local did=$1 port=$2 token=$3 kind=$4
  local url="${NODE_HOST}:${port}/rubix/v1/tx"
  local body
  body=$(jq -nc \
    --arg did "$did" --arg token "$token" --arg memo "deploy ${kind}" \
    '{
      initiator: $did,
      owner:     $did,
      tokens: {
        rbt: 0,
        smartContract: [{
          smartContractId: $token,
          value: 1,
          data:  "deploy"
        }]
      },
      memo: $memo
    }')
  local resp
  resp=$(curl -s -X POST "$url" -H "Content-Type: application/json" -d "$body")
  logfile_say "[$did/$kind] tx response: $resp"
  local req_id
  req_id=$(echo "$resp" | jq -r '.result.id // .result.request_id // empty')
  [[ -n "$req_id" ]] || return 1
  echo "$req_id"
}

rubix_sign() {
  local port=$1 req_id=$2 password=$3 did=$4 kind=$5
  local url="${NODE_HOST}:${port}${SIGN_PATH}"
  local body
  body=$(jq -nc --arg id "$req_id" --arg pw "$password" \
    '{id: $id, password: $pw}')
  local resp
  resp=$(curl -s -X POST "$url" -H "Content-Type: application/json" -d "$body")
  logfile_say "[$did/$kind] sign response: $resp"
  local ok
  ok=$(echo "$resp" | jq -r '.status // false')
  [[ "$ok" == "true" ]] || return 1
}

# ---------------------------------------------------------------- main loop
deployed=0
skipped=0
failed=0
requested=0

# Read admins from DB.
ADMIN_ROWS=$(psql_exec "SELECT did || '|' || node_port || '|' || password
                        FROM admins ORDER BY created_at")

if [[ -z "$ADMIN_ROWS" ]]; then
  echo "no admins in DB. provision via POST /api/admins/setup first." >&2
  exit 1
fi

# Count admins for the per-admin progress prefix.
total_admins=$(echo "$ADMIN_ROWS" | grep -c .)
admin_idx=0

while IFS='|' read -r did port password; do
  [[ -z "$did" ]] && continue
  admin_idx=$((admin_idx + 1))
  both_say ""
  both_say "[admin $admin_idx/$total_admins: $did port=$port]"

  for kind in "${KINDS[@]}"; do
    requested=$((requested + 1))
    existing=$(contract_exists "$did" "$kind" || true)
    if [[ -n "$existing" ]]; then
      both_say "  [$kind] already deployed ($existing) — skipped"
      skipped=$((skipped + 1))
      continue
    fi

    console_say "  [$kind] generating..."
    token=$(rubix_generate "$did" "$port" "$kind") || {
      both_say "  [$kind] FAILED at generate (see $LOGFILE)"
      failed=$((failed + 1))
      continue
    }
    console_say "  [$kind]   token=$token"

    console_say "  [$kind] deploying..."
    req_id=$(rubix_deploy_tx "$did" "$port" "$token" "$kind") || {
      both_say "  [$kind] FAILED at tx (see $LOGFILE)"
      failed=$((failed + 1))
      continue
    }

    console_say "  [$kind] signing..."
    rubix_sign "$port" "$req_id" "$password" "$did" "$kind" || {
      both_say "  [$kind] FAILED at sign (see $LOGFILE)"
      failed=$((failed + 1))
      continue
    }

    upsert_contract "$did" "$kind" "$token" >/dev/null || {
      both_say "  [$kind] FAILED at db upsert (see $LOGFILE)"
      failed=$((failed + 1))
      continue
    }

    write_contract_file "$did" "$kind" "$token" || {
      logfile_say "  [$kind] file write failed (DB row still persisted)"
    }

    both_say "  [$kind] done. token=$token"
    deployed=$((deployed + 1))
  done
done <<< "$ADMIN_ROWS"

# ---------------------------------------------------------------- summary
both_say ""
both_say "Summary: requested=$requested deployed=$deployed skipped=$skipped failed=$failed"
both_say "Logs: $LOGFILE"

if [[ $failed -gt 0 ]]; then
  exit 1
fi
