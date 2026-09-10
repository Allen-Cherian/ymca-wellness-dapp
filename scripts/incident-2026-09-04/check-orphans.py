import csv, hashlib, json, glob
# Verifies each exported quorum transaction: id == SHA3-256(info), and prints what it paid.
for f in sorted(glob.glob('/tmp/orphans/orphan-node*.csv')):
    for tid, info, sig in csv.reader(open(f)):
        j = json.loads(info); sc = j['tokens']['smartContract'][0]
        ok = hashlib.sha3_256(info.encode()).hexdigest() == tid
        print(f"{f.split('/')[-1]}: hash_ok={ok} initiator={j['initiator'][:22]}.. sc={sc['tokenId'][:14]}.. prev={sc['previousTransactionID'][:12]}.. sig_fields={sorted(json.loads(sig).keys())}")
