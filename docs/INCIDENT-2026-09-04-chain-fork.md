# Incident: reward-contract chain forks on yqa (3–4 Sep 2026), repaired 7 Sep 2026

Environment: yqa (https://yqa.rubix.network, VM EXTSTVM01), 10 Rubix nodes, quorums on a
second VM. All commands below were run as `rubix`. Scripts referenced here are checked in
under `scripts/incident-2026-09-04/`.

## Summary

Between 23:40 on 3 Sep and 11:53 on 4 Sep, five of the ten reward contracts (nodes 2, 4,
7, 8, 9) stopped accepting payouts. Every attempt failed at the signing step with:

```
initiateConsensusHandler: transaction info validation failed: ValidateTransaction:
TokenChainIntigrityCheck: token <sc> (smart_contract) chain mismatch after sync from
<ownerDID>: local latest <quorumHead> != expected <ownerHead>
```

Separately, the 4 Sep VM restart killed in-flight transactions on nodes 3 and 10 and left
their reward contracts locked (`token_status=1`) in each node's own Postgres.

Both were repaired on 7 Sep by direct database edits, without redeploying any contract,
and verified with one live payout through every one of the ten admins.

## Root cause

A bug in `rubixgoplatform`, `core/sync.go:548`, `applyTokenChainFromSync`:

```
panic: runtime error: slice bounds out of range [4254:4253]
core.(*Core).applyTokenChainFromSync        core/sync.go:548
core.(*Core).SyncTransactionChainsFromPeer  core/sync.go:229
core.(*Core).ContractCallBack               core/smart_contract.go:349
created by types.(*PubSub).receivePub       types/pubsub.go:164
```

A pubsub chain-sync callback receives a chain one entry shorter than the local chain and
slices past the end. There is no `recover()` around pubsub callbacks, so the whole node
process dies; the container's `entrypoint.sh` relaunches it two seconds later (Docker
reports zero restarts). The owner subscribes to its own contract topic on every execution
and syncs its own contract from itself on every echo, so under the stress-test load a
stale echo one position behind is routine.

When the panic lands between "Initiating consensus with quorum" and the owner's
post-consensus persistence, the quorum commits the block and the owner never records it.
The quorum is then one block ahead of the owner, permanently:

- Quorum validation (`TokenChainIntegrityCheck`, `core/consensus/checks.go:694`) reads
  its own head from `tokenchain_index`, pulls the owner's chain from the owner, and can
  only append. An owner that is behind has nothing the quorum lacks, so the check fails
  on every retry.
- No shipped endpoint lets an owner pull its own chain from its quorum. Recovery from
  fullnodes covers RBT/FT/NFT only, not smart contracts.
- The quorum's pledge tokens for the lost transaction stay `Pledged` until an execution
  arrives whose previous transaction is the lost one.

The five crashes each happened within five minutes of that node's last success, one node
at a time, over a twelve-hour stress run. The 4 Sep restart did not cause the forks; node7
was already failing this way at 13:28, before it.

## State found by the audit (7 Sep, before any change)

| Node | Port | Reward contract | Owner head | Quorum head | Finding |
|---|---|---|---|---|---|
| node1 | 8000 | QmdtEW… | 8 | 8 | clean, 7 payouts ever |
| node2 | 8001 | QmRE3u… | 3906 | 3907 | quorum ahead by 1 |
| node3 | 8002 | QmRT5w… | 4530 | 4530 | locked, lock ref DF99BC78… |
| node4 | 8003 | QmSDKA… | 4186 | 4187 | quorum ahead by 1 |
| node5 | 8004 | Qmc5PE… | 4548 | 4548 | clean |
| node6 | 8005 | QmeTYq… | 0 | none | deployed, never executed |
| node7 | 8006 | Qmanq3… | 4253 | 4254 | quorum ahead by 1 |
| node8 | 8007 | Qme6dP… | 3344 | 3345 | quorum ahead by 1 |
| node9 | 8008 | QmYDJx… | 4150 | 4151 | quorum ahead by 1 |
| node10 | 8009 | QmVxck… | 4451 | 4451 | locked, lock ref 843C483B… |

Other findings that turned out to be harmless:

- `token_status=11` with a stale `lock_reference_id` (nodes 2, 4, 7, 8, 9): residue of the
  failed-transaction release path, which restores status but leaves the reference. The
  executability check reads status only. Inert.
- `token_status=10, latest_role=4` on add_admin contracts and node6's reward contract:
  deployed, never executed. Status 10 is executable.
- One `token_status=0, latest_role=2` row per node: the node's last free 1.0 RBT token.
- All 20 add_activity/add_admin contracts unaffected.
- Ports 8002/8004/8009 refusing connections and the "unhealthy" containers were cured by
  the `docker restart` at 06:15 on 7 Sep.

## What was changed on 7 Sep

Backups: `pg_dump` of every touched node DB is in `/datadrive/backup-node<N>-rubix-20260907-*.sql`.

### 1. Unlock node3 and node10 (owner DBs)

```sql
UPDATE tokens SET token_status=11, lock_reference_id=NULL, updated_at=now()
 WHERE token_id='QmRT5wwenWCvxg3a8mVS6qb2XL4RJsg4speCemN7xbF5iD'
   AND token_status=1 AND lock_reference_id='DF99BC78-1583-4BA4-BBCA-204AAE83A27D';   -- node3
UPDATE tokens SET token_status=11, lock_reference_id=NULL, updated_at=now()
 WHERE token_id='QmVxckxSZ8JtkPW72mRdUrBRtQp3WKKrhi8wgaGDc2LcPg'
   AND token_status=1 AND lock_reference_id='843C483B-8323-4554-86B5-D185F440598A';   -- node10
```

Then `docker restart node3-node node10-node`. Safe because both chains equalled their
quorum's copy and the interrupted transactions had failed at the POST, before consensus.

### 2. Bring the five drifted owners forward (owner DBs)

Redeploying was rejected: a contract deploy consumes a whole 1.0 RBT token with no change
(`core/transaction_builder.go:337`), each node has exactly one left, and it would strand
the quorum pledges forever.

For each of nodes 2, 4, 7, 8, 9 the quorum's `transactions` row for the lost transaction
was exported (`scripts/incident-2026-09-04/export-orphans.sh`, run on the quorum VM; the
id was verified to equal SHA3-256 of the stored info) and replayed on the owner
(`scripts/incident-2026-09-04/replay-orphans.sh`), reproducing the owner's own
post-consensus writes in one transaction:

1. `transactions` (id, info, signature) copied verbatim from the quorum.
2. `transaction_units` (tx, ownerDID, 'initiator', 'committed').
3. `tokenchain` row: role 3, position = head+1, previous = old head.
4. `tokenchain_index.index` rebuilt as `array_agg(id ORDER BY position)`.
5. `tokens`: transaction_id, latest_position+1, latest_role 3, status 11, lock ref NULL.

Guards: exactly one exported row, contract present at status 11, owner head equals the
block's declared previous transaction, block not already on chain, initiator in `dids`.

Lost transactions replayed:

| Node | Transaction | New position |
|---|---|---|
| node2 | 21eaecee8026796212e8ac74351bafcea7d90e670cf71b1175de01b31fb81c5a | 3907 |
| node4 | 72c187d713ad6c93448e9c9c79da12a2a8fd41a8dcc38e375ba9a9737484b4cd | 4187 |
| node7 | aac44572f5072e846c6003c8d699fd7c832139a4ca5fa0ffc586b9f42ade08bf | 4254 |
| node8 | 63eb8fb3a910a372b041155e7ed1d32db087dee248a53ea96bd29fdc901b6586 | 3345 |
| node9 | ade46223b5a32a64b3495728bd54a57bb29e044496e0dc01e864a758c003d6a4 | 4151 |

Then `docker restart` of the five owner nodes.

### 3. dApp alignment (ymca-pg, `transfer_status`)

The dApp has no ledger; balances are sums over `status='success'` rows. Five requests had
been executed by consensus but recorded as failed. Each was flipped to success with its
real transaction id, contract hash, points from the block, the standard message, and an
`error_details` note beginning `replayed 2026-09-07`.

| Port | request_id | Points |
|---|---|---|
| 8001 | 16501900-644c-489f-99c0-f28efcb2472d | 2 |
| 8003 | b3642711-17f2-4af0-8df3-8d9c5af29430 | 2 |
| 8006 | dc185f95-9e14-4742-8f24-edab119712b2 | 1 |
| 8007 | f112bcb7-2146-4a3d-a0a8-70d18e29ebcc | 3 |
| 8008 | 55c1ffbe-b31f-4980-a045-311ec635df56 | 3 |

Node8's row differed from the others: the quorum committed on the first attempt, the node
retried, and the dApp stored the retry's chain-mismatch error rather than a signature EOF.

Nothing else in the dApp was changed. The ~110k chain-mismatch failures, the
connection-refused failures around each crash, and the node3/node10 interrupted requests
are all correctly failed.

### 4. Verification

Ten live payouts (activity `1`, or `yoga-001` for node1) to user
`bafybmie45toyrjyh5qpd5vtpkq6vk3ukes7ieta2ip2irpei3xu2ylz7kq`, one per admin, all
`success` with a transaction id. On the quorum VM, every `unpledge_sequence_info` row for
the five lost transactions was deleted by that first execution and every quorum head now
equals the owner head (`scripts/incident-2026-09-04/check-pledges.sh`).

## Recurrence: 8 Sep 2026

nginx was reopened to the testers at about 11:35 UTC on 7 Sep on the unpatched binary.
Within seconds (11:39:41–11:39:42) nodes 3, 5 and 9 each panicked once mid-consensus
under the retry burst. Same shape as before: quorum one block ahead, owner's head equal to
its last success, contract left locked by the crash until the locks were released on the
morning of 8 Sep, after which every payout failed with the chain mismatch.

Repaired 8 Sep with the same scripts (owner DB backups in `/datadrive/backup-node{3,5,9}-*`):

| Node | Lost transaction | Replayed at | dApp request flipped | Verified by |
|---|---|---|---|---|
| node3 | 20f6f709c76c4b908ac0ca87e758e02eca2608ff6fdd01157494087efc34030e | 4535 | 9baef1f6-e9fb-46b4-99fa-ee43ac57b2ea (1 pt) | 30ac85c5… @4536 |
| node5 | 4d58b9bce05e3750a2c1c69bb77624602e3a48bf48f88c5f89b50b350bae793d | 4552 | 92b00e07-d9f8-4ac4-a13e-6833a8fc1079 (3 pts) | 9145b30a… @4553 |
| node9 | e8b7c9225579234b5ca37430b98e5fbffe1af60ed6d7a1c69f4212fa739e2618 | 4158 | 284bf0b3-2b40-441a-b3d0-8af4aa2c76f9 (1 pt) | 82a881aa… @4159 |

Quorum pledges released on the first post-repair execution.

What the overnight failures were: roughly 3,000–6,500 "SC lock failed" per contract on
all eight active contracts (nodes 2–5, 7–10), i.e. every contract sat locked after a
crash until the morning. Allen rolled a platform build with a **startup unlock** (locked
smart contracts are released when the node starts) at about 06:35 UTC on 8 Sep; the
restarts released all locks, which is when the three forked nodes began reporting the
mismatch. That build does NOT yet contain the sync-panic guard.

Also checked on 8 Sep before reopening: nodes 2, 4, 7, 8 had panicked again that morning
(3, 2, 3, 1 times) but their owner heads equalled their quorum heads (3953, 4220, 4278,
3579), so those crashes fell outside consensus and nothing forked. nginx reopened at
about 10:30 UTC on 8 Sep by Allen's decision, on the binary without the sync guard.

Do-not-retry list for the testers (users already credited by the replayed blocks):
`16501900-644c…`, `b3642711-17f2…`, `dc185f95-9e14…`, `f112bcb7-2146…`, `55c1ffbe-b31f…`,
`9baef1f6-e9fb…`, `92b00e07-d9f8…`, `284bf0b3-2b40…`.

Conclusion: a crash inside consensus forks the chain; the startup unlock only cures the
lock half. Until the sync guard is deployed, run `morning-check.sh` daily and repair
forks with the scripted procedure.

## Still open

1. **Platform fix not yet deployed.** Branch `fix/sync-panic-and-self-echo` in
   rubixgoplatform: guard the slice at `core/sync.go:548` and `:888`, `recover()` in
   `types/pubsub.go` `receivePub`, skip self-echo in `ContractCallBack`/`NFTCallBack`,
   stop re-subscribing on every execution. Until it is on all ten nodes, any burst of
   payouts can fork a chain again. Repair is the procedure above.
2. **Testers' retry mechanism** must exclude the five request ids in section 3, or those
   users are credited twice.
3. **Separate platform bug to file:** SC deploys do not decrement `token_denom`
   (`post_consensus_persistence.go:148-152`).
4. `docs/RESTART.md` quorum-check commands use `/api/get-all-quorum`, which is not a route
   on this build (404). Quorum config is in each node's `quorum_manager` table.
5. The operator password in older notes (`rubix@123`) is wrong; use `BOOTSTRAP_PASSWORD`
   from `/datadrive/ymca-wellness-dapp/.env`.

## Rounds 3 and 4: 8 Sep 2026, ~10:00 and ~11:40 UTC

After reopening on the binary without the sync guard, nodes 4 and 5 forked again within
the hour (round 3, lost tx 21cad7a6…, 21b6cb8d…), and by 11:40 nodes 3 and 7 had
joined them (round 4, lost tx 8884d15b…, e90f03e1…); nodes 2, 8 and 9 crashed the same
morning but outside consensus and recovered on their own. All four were repaired in one
pass with `fork-repair.sh auto` (first use), verified with payouts ca903927…, 094138e8…,
0870f4ca…, 42ebf749…, pledges released. Do-not-retry additions: `cad2f0c5-62f6…`,
`03044ed3-922b…`, `dc692c05-1887…`, `abfb7fa0-c8b0…`. nginx left stopped pending the dApp
per-admin gap (PAYOUT_MIN_INTERVAL_MS, feat/payout-spacing) and the platform fix.

## Repeatable repair: `scripts/incident-2026-09-04/fork-repair.sh`

All of the above is now one script with subcommands. Get it onto the VMs via git or scp,
never by pasting (it contains heredocs). Stop nginx first so no payouts run mid-repair.

```
yqa VM:     fork-repair.sh detect            # writes /datadrive/fork-repair/forks.txt
quorum VM:  fork-repair.sh export forks.txt  # writes orphans.tgz; scp it to yqa /datadrive/fork-repair/
yqa VM:     fork-repair.sh apply --dry-run   # guards only, rolls back
yqa VM:     fork-repair.sh apply             # pg_dump, replay, align dApp rows, restart nodes
yqa VM:     fork-repair.sh verify            # one real payout per repaired admin
quorum VM:  fork-repair.sh pledges forks.txt # pledges released, heads advanced
```

With ssh key access from yqa to the quorum VM, `QUORUM_HOST=rubix@<quorum-vm>
fork-repair.sh auto [--dry-run]` runs the whole chain. `detect` only lists nodes whose
newest mismatch is newer than their last success and whose owner head equals the
head the mismatch error expected; anything else is printed for manual inspection.
The "dApp alignment" lines print the request ids to add to the testers' do-not-retry list.

## Morning check

Run `scripts/incident-2026-09-04/morning-check.sh` on the yqa VM. It reports, since a
given time: payout success/failure counts per node, the first chain-mismatch failure if
any, any panic in any node log, and owner chain consistency (tokenchain rows = index
length = joined rows). A non-zero mismatch count or a panic line means a re-fork; compare
owner vs quorum heads and repeat sections 2–3 for that node.

## Operational gotchas learned

- Same container names on both VMs. Guard quorum-side scripts with
  `docker ps --format '{{.Names}}' | grep -qx rubix_a`.
- Pasting multi-line blocks into the VM adds two leading spaces to every line after the
  first and hard-wraps very long lines. Use `nano -I`, then
  `sed -i '2,$ s/^  //' <file>`, then `bash -n`, and keep lines under ~160 characters.
- `docker exec … psql` with a heredoc needs `\$\$` for PL/pgSQL blocks.
- Quorum-VM nodeN is the quorum for yqa nodeN. The quorum keeps its copy of the owner's
  contract chain in its own `tokenchain`, not in the `fullnode_*` tables.
