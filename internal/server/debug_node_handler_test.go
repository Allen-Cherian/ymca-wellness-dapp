package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/nodedb"
	"ymca-wellness-dapp/internal/rubix"
)

// fakeNodeStore answers per node port from canned data.
type fakeNodeStore struct {
	enabled bool
	states  map[string]map[string]nodedb.ContractState // nodePort -> tokenID -> state
	chain   []nodedb.ChainRow
	tx      *nodedb.Transaction
	locks   map[string][]nodedb.LockRow
	inv     *nodedb.Inventory
	quorum  []nodedb.QuorumRow
	errFor  string // nodePort that fails
}

func (f *fakeNodeStore) Enabled() bool { return f.enabled }
func (f *fakeNodeStore) DBPort(nodePort string) (string, error) {
	return nodedb.Config{PortOffset: 1000}.DBPort(nodePort)
}
func (f *fakeNodeStore) fail(nodePort string) error {
	if nodePort == f.errFor {
		return errors.New("nodedb: open port " + nodePort + ": connection refused")
	}
	return nil
}
func (f *fakeNodeStore) ContractStates(_ context.Context, nodePort string, ids []string) (map[string]nodedb.ContractState, error) {
	if err := f.fail(nodePort); err != nil {
		return nil, err
	}
	out := map[string]nodedb.ContractState{}
	for _, id := range ids {
		if st, ok := f.states[nodePort][id]; ok {
			out[id] = st
		}
	}
	return out, nil
}
func (f *fakeNodeStore) ChainTail(_ context.Context, nodePort, _ string, limit int) ([]nodedb.ChainRow, error) {
	if err := f.fail(nodePort); err != nil {
		return nil, err
	}
	if limit < len(f.chain) {
		return f.chain[:limit], nil
	}
	return f.chain, nil
}
func (f *fakeNodeStore) GetTransaction(_ context.Context, nodePort, txID string) (*nodedb.Transaction, error) {
	if err := f.fail(nodePort); err != nil {
		return nil, err
	}
	if f.tx == nil || f.tx.ID != txID {
		return nil, nodedb.ErrNotFound
	}
	return f.tx, nil
}
func (f *fakeNodeStore) LockedTokens(_ context.Context, nodePort string) ([]nodedb.LockRow, error) {
	if err := f.fail(nodePort); err != nil {
		return nil, err
	}
	return f.locks[nodePort], nil
}
func (f *fakeNodeStore) TokenInventory(_ context.Context, nodePort, _ string, _ int) (*nodedb.Inventory, error) {
	if err := f.fail(nodePort); err != nil {
		return nil, err
	}
	return f.inv, nil
}
func (f *fakeNodeStore) QuorumMembers(_ context.Context, nodePort string) ([]nodedb.QuorumRow, error) {
	if err := f.fail(nodePort); err != nil {
		return nil, err
	}
	return f.quorum, nil
}

var nodeContracts = []database.AdminContractRow{
	{AdminDID: did4, NodePort: "8003", ContractKind: "add_activity", ContractHash: "QmAct4"},
	{AdminDID: did4, NodePort: "8003", ContractKind: "reward", ContractHash: scID},
	{AdminDID: "bafybmig3n5xn", NodePort: "8004", ContractKind: "reward", ContractHash: "Qmc5PE"},
}

func TestNodeEndpointsUnavailableWhenUnconfigured(t *testing.T) {
	s := newTestServer(t, &fakeStore{contracts: nodeContracts}, nil) // s.node nil
	for _, p := range []string{
		"/api/debug/node/contracts", "/api/debug/node/chain?admin_did=" + did4,
		"/api/debug/node/transaction?admin_did=" + did4 + "&tx=x", "/api/debug/node/locks",
		"/api/debug/node/tokens?admin_did=" + did4, "/api/debug/node/quorum",
	} {
		if code, _ := get(t, s, p); code != http.StatusServiceUnavailable {
			t.Errorf("%s: got %d, want 503", p, code)
		}
	}
	s.node = &fakeNodeStore{enabled: false}
	if code, _ := get(t, s, "/api/debug/node/contracts"); code != http.StatusServiceUnavailable {
		t.Errorf("disabled store: got %d, want 503", code)
	}
}

func TestNodeContracts(t *testing.T) {
	upd := time.Date(2026, 9, 8, 9, 10, 9, 0, time.UTC)
	ns := &fakeNodeStore{enabled: true, errFor: "8004", states: map[string]map[string]nodedb.ContractState{
		"8003": {
			scID: {TokenID: scID, TokenType: "smart_contract", Status: 11, StatusName: "executed", LatestPosition: 4220,
				LatestRole: 3, LatestRoleName: "initiator", HeadTx: hexA, LockReferenceID: "STALE-REF", OwnerDID: did4, UpdatedAt: upd,
				TokenchainRows: 4221, IndexLen: 4221, JoinedRows: 4221, MaxPosition: 4220},
			// add_activity contract missing from tokens: found=false
		},
	}}
	s := newTestServer(t, &fakeStore{contracts: nodeContracts}, nil)
	s.node = ns

	code, body := get(t, s, "/api/debug/node/contracts")
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	admins := data(t, body)["admins"].([]any)
	if len(admins) != 2 {
		t.Fatalf("admins = %d", len(admins))
	}
	a0 := admins[0].(map[string]any)
	if a0["node_port"] != "8003" || a0["db_port"] != "9003" || a0["error"] != nil {
		t.Errorf("admin0 = %v", a0)
	}
	cs := a0["contracts"].([]any)
	act, rew := cs[0].(map[string]any), cs[1].(map[string]any)
	if act["found"] != false || act["kind"] != "add_activity" {
		t.Errorf("add_activity = %v", act)
	}
	if rew["found"] != true || rew["status_name"] != "executed" || rew["latest_position"].(float64) != 4220 ||
		rew["head_tx"] != hexA || rew["consistent"] != true || rew["locked"] != false || rew["stale_lock_ref"] != true {
		t.Errorf("reward = %v", rew)
	}
	a1 := admins[1].(map[string]any)
	if !strings.Contains(a1["error"].(string), "connection refused") || a1["contracts"].([]any)[0].(map[string]any)["found"] != false {
		t.Errorf("unreachable node must report error, got %v", a1)
	}
}

func TestNodeChainAndTransaction(t *testing.T) {
	ns := &fakeNodeStore{enabled: true,
		chain: []nodedb.ChainRow{
			{Position: 4220, TransactionID: hexA, PreviousTx: hexC, Role: 3, RoleName: "initiator", HasTransaction: true},
			{Position: 4219, TransactionID: hexC, PreviousTx: "p", Role: 3, RoleName: "initiator", HasTransaction: false},
		},
		tx: &nodedb.Transaction{ID: hexA, InfoHash: hexA, Info: []byte(`{"initiator":"x"}`), Signature: []byte(`{}`),
			Units:     []nodedb.TxUnit{{DID: did4, ExecutionRole: "initiator", Status: "committed"}},
			ChainRefs: []nodedb.TxChainRef{{TokenID: scID, Position: 4220, Role: 3}}},
	}
	s := newTestServer(t, &fakeStore{contracts: nodeContracts}, nil)
	s.node = ns

	code, body := get(t, s, "/api/debug/node/chain?admin_did="+did4+"&limit=1")
	if code != http.StatusOK {
		t.Fatalf("chain status %d: %v", code, body)
	}
	d := data(t, body)
	rows := d["rows"].([]any)
	if d["contract_hash"] != scID || d["kind"] != "reward" || len(rows) != 1 || rows[0].(map[string]any)["position"].(float64) != 4220 {
		t.Errorf("chain = %v", d)
	}
	if code, _ := get(t, s, "/api/debug/node/chain?admin_did="+did4+"&kind=nft"); code != http.StatusNotFound {
		t.Errorf("unknown kind: got %d, want 404", code)
	}
	if code, _ := get(t, s, "/api/debug/node/chain?admin_did=nobody"); code != http.StatusNotFound {
		t.Errorf("unknown admin: got %d, want 404", code)
	}
	if code, _ := get(t, s, "/api/debug/node/chain"); code != http.StatusBadRequest {
		t.Errorf("missing admin: got %d, want 400", code)
	}

	code, body = get(t, s, "/api/debug/node/transaction?admin_did="+did4+"&tx="+hexA)
	d = data(t, body)
	if code != http.StatusOK || d["found"] != true || d["info_hash_matches_id"] != true ||
		d["info"].(map[string]any)["initiator"] != "x" || len(d["units"].([]any)) != 1 || len(d["chain_refs"].([]any)) != 1 {
		t.Errorf("transaction = %d %v", code, d)
	}
	code, body = get(t, s, "/api/debug/node/transaction?admin_did="+did4+"&tx="+hexB)
	if d := data(t, body); code != http.StatusOK || d["found"] != false {
		t.Errorf("missing tx: %d %v", code, d)
	}
	if code, _ := get(t, s, "/api/debug/node/transaction?admin_did="+did4); code != http.StatusBadRequest {
		t.Errorf("missing tx param: got %d, want 400", code)
	}
}

func TestNodeLocksTokensQuorum(t *testing.T) {
	since := time.Now().Add(-90 * time.Minute)
	ns := &fakeNodeStore{enabled: true, errFor: "8004",
		locks: map[string][]nodedb.LockRow{"8003": {
			{TokenID: scID, TokenType: "smart_contract", OwnerDID: did4, LockReferenceID: "REF-1", LatestPosition: 4220, UpdatedAt: since},
		}},
		inv: &nodedb.Inventory{
			Buckets:      []nodedb.TokenBucket{{TokenType: "rbt", Status: 0, StatusName: "free", Count: 1, Value: "1"}},
			Denoms:       []nodedb.DenomRow{{Denom: "1.000", Count: 1}},
			PendingCount: 0,
		},
		quorum: []nodedb.QuorumRow{{DID: "bafybmiquorum4"}},
	}
	s := newTestServer(t, &fakeStore{contracts: nodeContracts}, nil)
	s.node = ns

	code, body := get(t, s, "/api/debug/node/locks")
	d := data(t, body)
	if code != http.StatusOK || d["count"].(float64) != 1 {
		t.Fatalf("locks = %d %v", code, d)
	}
	l := d["locks"].([]any)[0].(map[string]any)
	if l["node_port"] != "8003" || l["kind"] != "reward" || l["lock_reference_id"] != "REF-1" || !strings.HasPrefix(l["locked_for"].(string), "1h") {
		t.Errorf("lock row = %v", l)
	}
	nodes := d["nodes"].([]any)
	if len(nodes) != 2 || nodes[1].(map[string]any)["error"] == nil {
		t.Errorf("per-node results = %v", nodes)
	}

	code, body = get(t, s, "/api/debug/node/tokens?admin_did="+did4)
	d = data(t, body)
	if code != http.StatusOK || len(d["buckets"].([]any)) != 1 || len(d["denoms"].([]any)) != 1 || d["pending_unpledge_count"].(float64) != 0 {
		t.Errorf("tokens = %d %v", code, d)
	}
	if code, _ := get(t, s, "/api/debug/node/tokens?admin_did=bafybmig3n5xn"); code != http.StatusBadGateway {
		t.Errorf("unreachable node on single-node endpoint: got %d, want 502", code)
	}

	code, body = get(t, s, "/api/debug/node/quorum?admin_did="+did4)
	d = data(t, body)
	q := d["admins"].([]any)[0].(map[string]any)
	if code != http.StatusOK || len(d["admins"].([]any)) != 1 || q["quorum"].([]any)[0] != "bafybmiquorum4" {
		t.Errorf("quorum = %d %v", code, d)
	}
}

func TestForkCheckIncludesOwnerDBHead(t *testing.T) {
	store := &fakeStore{
		contracts: []database.AdminContractRow{{AdminDID: did4, NodePort: "8003", ContractKind: "reward", ContractHash: scID}},
		mismatch:  &database.TransferStatus{RequestID: "req-mm", ErrorDetails: mismatchText(hexA, hexB), CreatedAt: time.Now()},
	}
	fetch := func(context.Context, string, string) ([]rubix.ChainEntry, error) { return nil, errors.New("api down") }
	s := newTestServer(t, store, nil)
	s.fetchChain = fetch
	s.node = &fakeNodeStore{enabled: true, states: map[string]map[string]nodedb.ContractState{"8003": {
		scID: {TokenID: scID, HeadTx: hexB, LatestPosition: 4220, StatusName: "executed", LockReferenceID: ""},
	}}}
	_, body := get(t, s, "/api/debug/fork-check?admin_did="+did4)
	d := data(t, body)
	if d["node_error"] == nil || d["owner_db_head"] != hexB || d["owner_db_position"].(float64) != 4220 || d["owner_db_status"] != "executed" {
		t.Errorf("fork-check with API down must still show DB head: %v", d)
	}
}
