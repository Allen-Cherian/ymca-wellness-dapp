#!/usr/bin/env bash
# export-orphans.sh — READ-ONLY, run on the QUORUM VM.
# Copies the quorum's `transactions` row for each committed-but-lost transaction to a CSV,
# then bundle with:  tar czf /tmp/orphans.tgz -C /tmp orphans
# and verify with:   python3 check-orphans.py
# Edit the node/tx list below for a new incident (node number, lost transaction id).
docker ps --format '{{.Names}}' | grep -qx rubix_a || { echo "NOT the quorum VM ($(hostname)) - stop"; exit 1; }
set -u
OUT=/tmp/orphans; mkdir -p $OUT
while read -r N TX; do
  [ -z "$N" ] && continue
  docker exec node${N}-postgres psql -U rubix -d rubix -At -c "COPY (SELECT id, info::text, signature::text FROM transactions WHERE id='$TX') TO STDOUT WITH (FORMAT csv)" > $OUT/orphan-node${N}.csv
  ROW=$(docker exec node${N}-postgres psql -U rubix -d rubix -At -c "SELECT position||' prev='||left(previous_transaction_id,12) FROM tokenchain WHERE transaction_id='$TX' AND token_id LIKE 'Qm%'")
  echo "node$N: $(wc -l < $OUT/orphan-node$N.csv) row(s), $(wc -c < $OUT/orphan-node$N.csv) bytes; quorum chain row: $ROW"
done <<'LIST'
2 21eaecee8026796212e8ac74351bafcea7d90e670cf71b1175de01b31fb81c5a
4 72c187d713ad6c93448e9c9c79da12a2a8fd41a8dcc38e375ba9a9737484b4cd
7 aac44572f5072e846c6003c8d699fd7c832139a4ca5fa0ffc586b9f42ade08bf
8 63eb8fb3a910a372b041155e7ed1d32db087dee248a53ea96bd29fdc901b6586
9 ade46223b5a32a64b3495728bd54a57bb29e044496e0dc01e864a758c003d6a4
LIST
