#!/usr/bin/env bash
# check-pledges.sh — READ-ONLY, run on the QUORUM VM after the owners have executed once
# post-repair. For each replayed transaction: the pending unpledge row should be gone (0)
# and the quorum's contract head should be one position above the replayed block.
docker ps --format '{{.Names}}' | grep -qx rubix_a || { echo "NOT the quorum VM ($(hostname)) - stop"; exit 1; }
LIST="2:21eaecee8026796212e8ac74351bafcea7d90e670cf71b1175de01b31fb81c5a
4:72c187d713ad6c93448e9c9c79da12a2a8fd41a8dcc38e375ba9a9737484b4cd
7:aac44572f5072e846c6003c8d699fd7c832139a4ca5fa0ffc586b9f42ade08bf
8:63eb8fb3a910a372b041155e7ed1d32db087dee248a53ea96bd29fdc901b6586
9:ade46223b5a32a64b3495728bd54a57bb29e044496e0dc01e864a758c003d6a4"
for P in $LIST; do
  N=${P%%:*}; TX=${P##*:}
  SC=$(docker exec node$N-postgres psql -U rubix -d rubix -At -c "SELECT token_id FROM tokenchain WHERE transaction_id='$TX' AND token_id LIKE 'Qm%'")
  U=$(docker exec node$N-postgres psql -U rubix -d rubix -At -c "SELECT count(*) FROM unpledge_sequence_info WHERE tx_id='$TX'")
  H=$(docker exec node$N-postgres psql -U rubix -d rubix -At -c "SELECT position||' '||left(transaction_id,12) FROM tokenchain WHERE token_id='$SC' ORDER BY position DESC LIMIT 1")
  echo "node$N: pending unpledge rows=$U ; contract head=$H"
done
