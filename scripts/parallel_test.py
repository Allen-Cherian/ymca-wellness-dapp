#!/usr/bin/env python3
"""
parallel_test.py — concurrency test for the v1 /admin/payouts endpoint.

What it does:

  1. Reads admins, activities, and user DIDs from the dApp's Postgres.
  2. Submits N transfers per admin concurrently via /admin/payouts.
     Default 5 per admin × 6 admins = 30 transfers, all fired at once.
  3. Polls /admin/payouts/status/:request_id until every transfer
     reaches a terminal state (success or failed).
  4. Prints a summary: total time, throughput, per-admin breakdown,
     same-admin FIFO check, cross-admin parallelism speedup.
  5. Saves full per-request results to logs/parallel-test-<ts>.jsonl.

Exit code: 0 if all transfers reach success; 1 otherwise.

Usage:

    python3 scripts/parallel_test.py                 # 5 per admin
    python3 scripts/parallel_test.py --per-admin 10
    python3 scripts/parallel_test.py --concurrency 5 # cap submitter pool
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone
from pathlib import Path
from urllib import request as urlreq, error as urlerr

# psycopg is more common, but to keep this script dependency-free we use psql
# via subprocess for the DB read-only setup queries.
import subprocess

BASE = os.environ.get("BASE_URL", "http://localhost:9000")

DB_HOST = os.environ.get("DB_HOST", "localhost")
DB_PORT = os.environ.get("DB_PORT", "5432")
DB_USER = os.environ.get("DB_USER", "postgres")
DB_PASS = os.environ.get("DB_PASSWORD", "postgres")
DB_NAME = os.environ.get("DB_NAME", "ymca_wellness_cafe_v2")

POLL_TIMEOUT_SECS = 600   # 10 minutes total budget for poll phase
POLL_INTERVAL_SECS = 1    # poll each request_id this often
PROGRESS_INTERVAL_SECS = 2  # how often to print live counts

TERMINAL_STATUSES = {"success", "failed"}


# ---------------------------------------------------------------- DB helpers

def psql(query: str) -> str:
    """Run a read-only psql query, return tab-separated rows."""
    env = os.environ.copy()
    env["PGPASSWORD"] = DB_PASS
    cp = subprocess.run(
        ["psql", "-h", DB_HOST, "-p", DB_PORT, "-U", DB_USER, "-d", DB_NAME,
         "-tAF\t", "-c", query],
        capture_output=True, text=True, env=env, check=True,
    )
    return cp.stdout.strip()


def load_admins() -> list[str]:
    out = psql("SELECT did FROM admins ORDER BY created_at;")
    return [line for line in out.splitlines() if line]


def load_activities_for(admin: str) -> list[str]:
    out = psql(
        f"SELECT activity_id FROM activities "
        f"WHERE admin_did = '{admin}' ORDER BY activity_id;"
    )
    return [line for line in out.splitlines() if line]


def load_user_for_admin(admin_did: str) -> str | None:
    """Return one user mapped to admin_did, or None if there are none."""
    out = psql(
        f"SELECT user_did FROM user_admins WHERE admin_did = '{admin_did}' "
        f"ORDER BY created_at LIMIT 1;"
    )
    return out.strip() or None


def generate_secp256k1_pubkey() -> str:
    """Shell out to openssl to produce a 130-char uncompressed secp256k1
    pubkey (04 + 64 bytes of X + 64 bytes of Y, hex-encoded)."""
    import tempfile
    with tempfile.NamedTemporaryFile(suffix=".pem", delete=False) as priv:
        priv_path = priv.name
    try:
        subprocess.run(
            ["openssl", "ecparam", "-name", "secp256k1", "-genkey", "-noout",
             "-out", priv_path],
            check=True, capture_output=True,
        )
        cp = subprocess.run(
            ["openssl", "ec", "-in", priv_path, "-pubout",
             "-conv_form", "uncompressed", "-text", "-noout"],
            check=True, capture_output=True, text=True,
        )
        # openssl writes the -text dump to stdout; -pubout sends the PEM
        # block to stdout too but we ignore it since we want the hex.
        in_pub = False
        hex_lines = []
        for line in cp.stdout.splitlines():
            stripped = line.strip()
            if stripped.startswith("pub:"):
                in_pub = True
                continue
            if in_pub:
                if stripped.startswith("ASN1 OID") or not stripped or not any(
                        c in "0123456789abcdef:" for c in stripped):
                    break
                hex_lines.append(stripped)
        pubkey = "".join(hex_lines).replace(":", "").replace(" ", "")
        if len(pubkey) != 130:
            raise RuntimeError(f"unexpected pubkey length {len(pubkey)}: {pubkey!r}")
        return pubkey
    finally:
        try:
            os.unlink(priv_path)
        except OSError:
            pass


def ensure_user_for_admin(admin_did: str) -> str:
    """Return a user_did mapped to admin_did. Provisions a fresh one
    via /api/create-did-with-pubkey if none exists."""
    existing = load_user_for_admin(admin_did)
    if existing:
        return existing
    print(f"  provisioning new user for {admin_did[:18]}.. (none mapped)")
    pubkey = generate_secp256k1_pubkey()
    code, body = http_post("/api/create-did-with-pubkey", {
        "admin_did": admin_did,
        "public_key": pubkey,
    }, timeout=60.0)
    if code != 200 or not isinstance(body, dict):
        raise RuntimeError(
            f"failed to provision user for {admin_did}: http={code} body={body}"
        )
    new_did = (body.get("data") or {}).get("did")
    if not new_did:
        raise RuntimeError(
            f"create-did-with-pubkey returned no DID: body={body}"
        )
    return new_did


# ---------------------------------------------------------------- HTTP helpers

def http_post(path: str, body: dict, timeout: float = 30.0) -> tuple[int, dict]:
    data = json.dumps(body).encode()
    req = urlreq.Request(
        f"{BASE}{path}", data=data, method="POST",
        headers={"Content-Type": "application/json"},
    )
    try:
        with urlreq.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urlerr.HTTPError as e:
        return e.code, json.loads((e.read() or b"{}").decode() or "{}")
    except Exception as e:
        return -1, {"error": str(e)}


def http_get(path: str, timeout: float = 30.0) -> tuple[int, dict]:
    req = urlreq.Request(f"{BASE}{path}", method="GET")
    try:
        with urlreq.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urlerr.HTTPError as e:
        return e.code, json.loads((e.read() or b"{}").decode() or "{}")
    except Exception as e:
        return -1, {"error": str(e)}


# ---------------------------------------------------------------- transfer flow

def submit_one(admin: str, user_did: str, activity_ids: list[str]) -> dict:
    """Submit one /admin/payouts request. Returns a record."""
    t0 = time.time()
    code, body = http_post("/admin/payouts", {
        "admin_did": admin,
        "user_did": user_did,
        "activity_id": activity_ids,
    })
    submit_secs = time.time() - t0
    request_id = None
    if isinstance(body, dict):
        result = body.get("result")
        if isinstance(result, dict):
            request_id = result.get("request_id")
    return {
        "admin_did": admin,
        "submit_http_status": code,
        "submit_secs": submit_secs,
        "submit_response": body,
        "request_id": request_id,
        "submitted_at": datetime.now(timezone.utc).isoformat(),
    }


def poll_one(request_id: str) -> dict:
    """Poll /admin/payouts/status/:id once."""
    code, body = http_get(f"/admin/payouts/status/{request_id}")
    if isinstance(body, dict):
        result = body.get("result")
        if isinstance(result, dict):
            return {"http_status": code, "result": result}
    return {"http_status": code, "result": None}


# ---------------------------------------------------------------- orchestration

def run_test(per_admin: int, concurrency: int | None, skip_admins: set[str]) -> int:
    print(f"== Loading prereqs from {DB_NAME} ==")
    admins = load_admins()
    if not admins:
        print("ERROR: no admins in DB. Run /api/admins/setup first.", file=sys.stderr)
        return 1
    if skip_admins:
        before = len(admins)
        admins = [a for a in admins if a not in skip_admins]
        skipped = before - len(admins)
        print(f"  skipping {skipped} admin(s): {', '.join(sorted(skip_admins))}")
        if not admins:
            print("ERROR: all admins skipped — nothing to test.", file=sys.stderr)
            return 1

    activities_by_admin: dict[str, list[str]] = {}
    for a in admins:
        acts = load_activities_for(a)
        if not acts:
            print(f"ERROR: admin {a} has no activities. Run /api/activity/add for it.",
                  file=sys.stderr)
            return 1
        activities_by_admin[a] = acts

    # Per-admin user. Reuse an existing mapped user when present; provision
    # a fresh one otherwise. The test sends each admin's transfers to its
    # own user (matches dApp's user_admins ownership intent).
    print("  resolving per-admin users...")
    users_by_admin: dict[str, str] = {}
    for a in admins:
        try:
            users_by_admin[a] = ensure_user_for_admin(a)
        except Exception as e:
            print(f"ERROR: could not get/create user for admin {a}: {e}",
                  file=sys.stderr)
            return 1

    total = len(admins) * per_admin
    if concurrency is None:
        concurrency = total
    print(f"  admins:    {len(admins)}")
    print(f"  per-admin: {per_admin}")
    print(f"  total tx:  {total}")
    print(f"  pool size: {concurrency}")
    print()
    print("  admin -> user mapping:")
    for a in admins:
        print(f"    {a[:18]}..  ->  {users_by_admin[a][:18]}..")
    print()

    # Build the list of (admin, user, activities) jobs in submission order:
    # all of admin A's, then all of admin B's, etc. Easier FIFO assertion.
    jobs: list[tuple[int, str, str, list[str]]] = []
    seq = 0
    for admin in admins:
        for _ in range(per_admin):
            jobs.append((seq, admin, users_by_admin[admin], activities_by_admin[admin]))
            seq += 1

    # ---- Submit phase ----
    print("== Submit phase ==")
    submit_t0 = time.time()
    records: list[dict] = [None] * len(jobs)
    with ThreadPoolExecutor(max_workers=concurrency) as ex:
        future_to_idx = {
            ex.submit(submit_one, admin, user, acts): seq
            for (seq, admin, user, acts) in jobs
        }
        for fut in as_completed(future_to_idx):
            idx = future_to_idx[fut]
            rec = fut.result()
            rec["submit_seq"] = idx
            records[idx] = rec
    submit_total = time.time() - submit_t0

    accepted = sum(1 for r in records if r["request_id"])
    rejected = total - accepted
    print(f"  submitted in {submit_total:.2f}s")
    print(f"  accepted (202): {accepted}")
    print(f"  rejected (non-202): {rejected}")
    if rejected:
        for r in records:
            if not r["request_id"]:
                print(f"    seq={r['submit_seq']} admin={r['admin_did'][:14]}.. "
                      f"http={r['submit_http_status']} body={r['submit_response']}")
    print()

    if accepted == 0:
        print("All requests rejected. Aborting.", file=sys.stderr)
        return 1

    # ---- Poll phase ----
    print("== Poll phase (terminal=success|failed; timeout {}s) ==".format(POLL_TIMEOUT_SECS))
    poll_t0 = time.time()
    last_progress = poll_t0
    pending = {r["request_id"]: r for r in records if r["request_id"]}
    final_by_id: dict[str, dict] = {}

    while pending and time.time() - poll_t0 < POLL_TIMEOUT_SECS:
        # Poll each pending in parallel.
        with ThreadPoolExecutor(max_workers=min(concurrency, 32)) as ex:
            results = {rid: ex.submit(poll_one, rid) for rid in list(pending)}
            for rid, fut in results.items():
                resp = fut.result()
                result = resp["result"]
                if result and result.get("status") in TERMINAL_STATUSES:
                    final_by_id[rid] = result
                    pending.pop(rid, None)

        # Live progress.
        now = time.time()
        if now - last_progress >= PROGRESS_INTERVAL_SECS:
            done = len(final_by_id)
            success = sum(1 for r in final_by_id.values() if r.get("status") == "success")
            failed = sum(1 for r in final_by_id.values() if r.get("status") == "failed")
            queue_resp = http_get("/api/queue/metrics")
            qd = queue_resp[1].get("data", {}) if isinstance(queue_resp[1], dict) else {}
            qsize = qd.get("total_queued", "?")
            print(f"  t={int(now - poll_t0):3d}s  done: {done}/{accepted}  "
                  f"success: {success}  failed: {failed}  pending: {len(pending)}  "
                  f"queue_depth: {qsize}")
            last_progress = now

        if pending:
            time.sleep(POLL_INTERVAL_SECS)

    poll_total = time.time() - poll_t0
    if pending:
        print(f"  TIMED OUT — {len(pending)} requests still pending after {POLL_TIMEOUT_SECS}s",
              file=sys.stderr)
    print(f"  poll phase: {poll_total:.2f}s")
    print()

    # Merge poll results into records.
    for r in records:
        rid = r["request_id"]
        if rid and rid in final_by_id:
            r["final"] = final_by_id[rid]
        else:
            r["final"] = None

    # ---- Summary ----
    wall_time = time.time() - submit_t0
    print("== Summary ==")
    print(f"  wall time:  {wall_time:.2f}s")
    print(f"  throughput: {accepted / wall_time:.2f} tx/s")
    success_count = sum(1 for r in records if r.get("final") and r["final"].get("status") == "success")
    failed_count = sum(1 for r in records if r.get("final") and r["final"].get("status") == "failed")
    pending_count = sum(1 for r in records if r["request_id"] and not r.get("final"))
    print(f"  success:    {success_count}")
    print(f"  failed:     {failed_count}")
    print(f"  pending:    {pending_count}")
    print(f"  rejected:   {rejected}")
    print()

    # Per-admin breakdown.
    print("== Per-admin ==")
    by_admin: dict[str, list[dict]] = {}
    for r in records:
        by_admin.setdefault(r["admin_did"], []).append(r)
    for admin, rows in by_admin.items():
        n = len(rows)
        ok = sum(1 for r in rows if r.get("final") and r["final"].get("status") == "success")
        # Estimate per-tx time using the spread between earliest created_at and
        # latest updated_at, divided by count. Rough but illustrative.
        created_ats = [r["final"]["created_at"] for r in rows if r.get("final")]
        updated_ats = [r["final"]["updated_at"] for r in rows if r.get("final")]
        if created_ats and updated_ats:
            t_min = min(created_ats)
            t_max = max(updated_ats)
            try:
                d = (datetime.fromisoformat(t_max.replace("Z", "+00:00"))
                     - datetime.fromisoformat(t_min.replace("Z", "+00:00")))
                span = d.total_seconds()
                avg = span / max(n, 1)
                print(f"  {admin[:18]}..  {ok}/{n}  span={span:.2f}s  avg={avg:.2f}s")
            except Exception:
                print(f"  {admin[:18]}..  {ok}/{n}  (timing parse failed)")
        else:
            print(f"  {admin[:18]}..  {ok}/{n}  (no completion data)")
    print()

    # Cross-admin parallelism check.
    if success_count > 0:
        admin_spans = []
        for admin, rows in by_admin.items():
            cs = [r["final"]["created_at"] for r in rows if r.get("final")]
            us = [r["final"]["updated_at"] for r in rows if r.get("final")]
            if cs and us:
                d = (datetime.fromisoformat(max(us).replace("Z", "+00:00"))
                     - datetime.fromisoformat(min(cs).replace("Z", "+00:00")))
                admin_spans.append(d.total_seconds())
        if admin_spans:
            sequential = sum(admin_spans)
            speedup = sequential / wall_time if wall_time > 0 else 0
            print("== Parallelism check ==")
            print(f"  sequential estimate (sum of per-admin spans): {sequential:.2f}s")
            print(f"  actual wall time:                              {wall_time:.2f}s")
            print(f"  speedup:                                       {speedup:.2f}x  "
                  f"(target > {0.7 * len(by_admin):.1f}x)")
            print()

    # Same-admin FIFO check.
    print("== Same-admin FIFO ==")
    fifo_pass = True
    for admin, rows in by_admin.items():
        ok_rows = [r for r in rows if r.get("final") and r["final"].get("status") == "success"]
        # Sort by submit_seq (the order we intended to send).
        ok_rows.sort(key=lambda r: r["submit_seq"])
        # Compare to the order they completed (updated_at).
        prev = None
        violated = False
        for r in ok_rows:
            ua = r["final"]["updated_at"]
            if prev and ua < prev:
                violated = True
                break
            prev = ua
        status = "PASS" if not violated else "FAIL"
        if violated:
            fifo_pass = False
        print(f"  {admin[:18]}..  {status}  (n={len(ok_rows)})")
    print()

    # ---- Save log ----
    Path("logs").mkdir(exist_ok=True)
    ts = datetime.now().strftime("%Y-%m-%d-%H%M%S")
    log_path = f"logs/parallel-test-{ts}.jsonl"
    with open(log_path, "w") as f:
        for r in records:
            f.write(json.dumps(r) + "\n")
    print(f"Full log: {log_path}")

    # Exit code.
    if pending_count or failed_count or rejected:
        return 1
    if not fifo_pass:
        return 1
    return 0


def main():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--per-admin", type=int, default=5,
                   help="Number of transfers per admin (default: 5)")
    p.add_argument("--concurrency", type=int, default=None,
                   help="Max parallel HTTP submitters (default: total tx count)")
    p.add_argument("--skip-admin", action="append", default=[],
                   metavar="DID",
                   help="Admin DID to exclude (repeatable)")
    args = p.parse_args()
    sys.exit(run_test(args.per_admin, args.concurrency, set(args.skip_admin)))


if __name__ == "__main__":
    main()
