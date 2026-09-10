package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"ymca-wellness-dapp/internal/auth"
	"ymca-wellness-dapp/internal/config"
	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/queue"
	"ymca-wellness-dapp/internal/rubix"
	"ymca-wellness-dapp/internal/service"
)

// fakeStore is an in-memory debugStore. Each method returns the canned
// value or error, and records the arguments it was called with.
type fakeStore struct {
	summary   []database.PayoutSummaryRow
	failures  []database.FailureGroup
	history   []database.TransferStatus
	contracts []database.AdminContractRow
	mismatch  *database.TransferStatus
	success   *database.TransferStatus
	firstAt   *time.Time
	err       error

	gotSince  time.Time
	gotLimit  int
	gotStatus string
	gotAdmin  string
	gotUser   string
	gotAfter  time.Time
}

func (f *fakeStore) PayoutSummary(_ context.Context, since time.Time, adminDID, userDID string) ([]database.PayoutSummaryRow, error) {
	f.gotSince, f.gotAdmin, f.gotUser = since, adminDID, userDID
	return f.summary, f.err
}
func (f *fakeStore) PayoutFailures(_ context.Context, since time.Time, adminDID string, limit int) ([]database.FailureGroup, error) {
	f.gotSince, f.gotAdmin, f.gotLimit = since, adminDID, limit
	return f.failures, f.err
}
func (f *fakeStore) PayoutHistory(_ context.Context, adminDID, userDID, status string, since time.Time, limit int) ([]database.TransferStatus, error) {
	f.gotAdmin, f.gotUser, f.gotStatus, f.gotSince, f.gotLimit = adminDID, userDID, status, since, limit
	return f.history, f.err
}
func (f *fakeStore) ListAdminContracts(_ context.Context, adminDID string) ([]database.AdminContractRow, error) {
	f.gotAdmin = adminDID
	var out []database.AdminContractRow
	for _, r := range f.contracts {
		if adminDID == "" || r.AdminDID == adminDID {
			out = append(out, r)
		}
	}
	return out, f.err
}
func (f *fakeStore) LatestRewardMismatch(context.Context, string) (*database.TransferStatus, error) {
	if f.mismatch == nil {
		return nil, database.ErrNotFound
	}
	return f.mismatch, nil
}
func (f *fakeStore) LatestRewardSuccess(context.Context, string) (*database.TransferStatus, error) {
	if f.success == nil {
		return nil, database.ErrNotFound
	}
	return f.success, nil
}
func (f *fakeStore) FirstRewardMismatchAfter(_ context.Context, _ string, after time.Time) (*time.Time, error) {
	f.gotAfter = after
	return f.firstAt, nil
}

var testKeys *auth.Keys

func init() {
	gin.SetMode(gin.TestMode)
	gin.DefaultWriter = io.Discard
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	testKeys = &auth.Keys{Private: priv, Public: &priv.PublicKey}
}

// newTestServer builds a real Server (real router, real auth middleware)
// with the debug store and node fetch replaced.
func newTestServer(t *testing.T, store debugStore, fetch chainFetcher) *Server {
	t.Helper()
	cfg := &config.AppConfig{Env: config.EnvConfig{ServerPort: "0", AccessTokenTTL: time.Minute}}
	svc := service.New(cfg)
	s := New(cfg, svc, queue.NewManager(svc, 1, time.Minute), testKeys)
	s.debug = store
	if fetch != nil {
		s.fetchChain = fetch
	}
	return s
}

func bearer(t *testing.T) string {
	t.Helper()
	tok, _, err := auth.IssueAccess(testKeys, uuid.New(), database.RoleOperator, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + tok
}

// get performs an authenticated GET and decodes the JSON body.
func get(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", bearer(t))
	w := httptest.NewRecorder()
	s.Engine.ServeHTTP(w, req)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: non-JSON body %q: %v", path, w.Body.String(), err)
	}
	return w.Code, body
}

func data(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	d, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data object in %v", body)
	}
	return d
}

const (
	hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hexC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	scID = "QmSDKAcontractcontractcontractcontractcontract"
	did4 = "bafybmid3lona"
)

// The error text exactly as the quorum's TokenChainIntigrityCheck stores
// it in transfer_status.error_details.
func mismatchText(quorum, owner string) string {
	return "initiateConsensusHandler: transaction info validation failed: ValidateTransaction: " +
		"TokenChainIntigrityCheck: token " + scID + " (smart_contract) chain mismatch after sync from " +
		did4 + ": local latest " + quorum + " != expected " + owner
}

func TestDebugRequiresAuth(t *testing.T) {
	s := newTestServer(t, &fakeStore{}, nil)
	for _, p := range []string{
		"/api/debug/payouts/summary", "/api/debug/payouts/failures",
		"/api/debug/payouts/history?admin_did=x", "/api/debug/contracts", "/api/debug/fork-check?admin_did=x",
	} {
		w := httptest.NewRecorder()
		s.Engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without token: got %d, want 401", p, w.Code)
		}
	}
}

func TestParseSince(t *testing.T) {
	def := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 8, 10, 30, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":                          def,
		"2026-09-08T10:30:00Z":      want,
		"2026-09-08T12:30:00+02:00": want,
		"2026-09-08 10:30":          want,
		"2026-09-08 10:30:00":       want,
		"2026-09-08":                time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC),
	}
	for in, exp := range cases {
		got, err := parseSince(in, def)
		if err != nil || !got.Equal(exp) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, exp)
		}
	}
	if _, err := parseSince("yesterday", def); err == nil {
		t.Error("parseSince(\"yesterday\") should fail")
	}
}

func TestParseMismatch(t *testing.T) {
	info, ok := parseMismatch(mismatchText(hexA, hexB))
	if !ok || info.QuorumHead != hexA || info.OwnerExpected != hexB || info.Contract != scID {
		t.Fatalf("parseMismatch = %+v, %v", info, ok)
	}
	if _, ok := parseMismatch("SC lock failed"); ok {
		t.Error("unrelated error must not parse")
	}
}

func TestDebugSummary(t *testing.T) {
	first := time.Date(2026, 9, 8, 9, 10, 9, 0, time.UTC)
	store := &fakeStore{summary: []database.PayoutSummaryRow{{
		AdminDID: did4, NodePort: "8003", Total: 10, Success: 7, Failed: 3, Mismatch: 3,
		FirstMismatchAt: &first, LastSuccessTx: hexC,
	}}}
	s := newTestServer(t, store, nil)

	code, body := get(t, s, "/api/debug/payouts/summary?since=2026-09-08T10:30:00Z&admin_did="+did4)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if !store.gotSince.Equal(time.Date(2026, 9, 8, 10, 30, 0, 0, time.UTC)) || store.gotAdmin != did4 {
		t.Errorf("store called with since=%v admin=%q", store.gotSince, store.gotAdmin)
	}
	admins := data(t, body)["admins"].([]any)
	if len(admins) != 1 {
		t.Fatalf("admins = %v", admins)
	}
	row := admins[0].(map[string]any)
	if row["node_port"] != "8003" || row["chain_mismatch"].(float64) != 3 || row["last_success_tx"] != hexC {
		t.Errorf("row = %v", row)
	}
	if row["first_mismatch_at"] != "2026-09-08T09:10:09Z" {
		t.Errorf("first_mismatch_at = %v, want UTC RFC3339", row["first_mismatch_at"])
	}
	if row["last_success_at"] != nil {
		t.Errorf("last_success_at should be null, got %v", row["last_success_at"])
	}

	// user_did filter is passed through.
	get(t, s, "/api/debug/payouts/summary?user_did=bafybmiuser")
	if store.gotUser != "bafybmiuser" || store.gotAdmin != "" {
		t.Errorf("user filter: admin=%q user=%q", store.gotAdmin, store.gotUser)
	}

	// Default window: since is about 24h ago.
	get(t, s, "/api/debug/payouts/summary")
	if d := time.Since(store.gotSince); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("default since is %v ago, want ~24h", d)
	}

	code, _ = get(t, s, "/api/debug/payouts/summary?since=notatime")
	if code != http.StatusBadRequest {
		t.Errorf("bad since: got %d, want 400", code)
	}

	store.err = errors.New("pg down")
	code, _ = get(t, s, "/api/debug/payouts/summary")
	if code != http.StatusInternalServerError {
		t.Errorf("store error: got %d, want 500", code)
	}
}

func TestDebugFailures(t *testing.T) {
	store := &fakeStore{failures: []database.FailureGroup{{Count: 42, Reason: "chain mismatch # != #"}}}
	s := newTestServer(t, store, nil)

	code, body := get(t, s, "/api/debug/payouts/failures?limit=5000")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if store.gotLimit != debugFailuresMax {
		t.Errorf("limit clamped to %d, want %d", store.gotLimit, debugFailuresMax)
	}
	f := data(t, body)["failures"].([]any)[0].(map[string]any)
	if f["count"].(float64) != 42 {
		t.Errorf("failure = %v", f)
	}

	get(t, s, "/api/debug/payouts/failures")
	if store.gotLimit != debugFailuresLimit {
		t.Errorf("default limit %d, want %d", store.gotLimit, debugFailuresLimit)
	}
	code, _ = get(t, s, "/api/debug/payouts/failures?limit=-1")
	if code != http.StatusBadRequest {
		t.Errorf("negative limit: got %d, want 400", code)
	}
}

func TestDebugHistory(t *testing.T) {
	now := time.Date(2026, 9, 8, 9, 10, 9, 40_000_000, time.UTC)
	store := &fakeStore{history: []database.TransferStatus{{
		RequestID: "req-1", TransactionID: hexA, Kind: "reward", AdminDID: did4, UserDID: "bafybmiuser",
		ActivityIDs: []string{"1"}, RewardPoints: 2, ContractHash: scID, Status: "success",
		Message: "transferred 2 ytoken", CreatedAt: now, UpdatedAt: now,
	}, {
		RequestID: "req-0", Kind: "reward", AdminDID: did4, Status: "failed",
		ErrorDetails: mismatchText(hexA, hexB), CreatedAt: now.Add(-time.Second), UpdatedAt: now,
	}}}
	s := newTestServer(t, store, nil)

	code, body := get(t, s, "/api/debug/payouts/history?admin_did="+did4+"&status=failed&since=2026-09-08%2009:00&limit=50")
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if store.gotAdmin != did4 || store.gotStatus != "failed" || store.gotLimit != 50 ||
		!store.gotSince.Equal(time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("store called with admin=%q status=%q since=%v limit=%d", store.gotAdmin, store.gotStatus, store.gotSince, store.gotLimit)
	}
	d := data(t, body)
	rows := d["rows"].([]any)
	if d["count"].(float64) != 2 || len(rows) != 2 {
		t.Fatalf("count/rows = %v / %d", d["count"], len(rows))
	}
	r0 := rows[0].(map[string]any)
	for _, k := range []string{"request_id", "transaction_id", "activity_ids", "reward_points", "contract_hash", "error_details", "message", "created_at", "updated_at", "user_did", "status", "kind", "admin_did"} {
		if _, ok := r0[k]; !ok {
			t.Errorf("row missing column %q", k)
		}
	}
	if r1 := rows[1].(map[string]any); r1["activity_ids"] == nil || !strings.Contains(r1["error_details"].(string), "chain mismatch") {
		t.Errorf("row 1 = %v", r1)
	}

	code, _ = get(t, s, "/api/debug/payouts/history")
	if code != http.StatusBadRequest {
		t.Errorf("missing admin_did and user_did: got %d, want 400", code)
	}
	code, _ = get(t, s, "/api/debug/payouts/history?user_did=bafybmiuser&status=failed")
	if code != http.StatusOK || store.gotUser != "bafybmiuser" || store.gotAdmin != "" || store.gotStatus != "failed" {
		t.Errorf("user-only history: code=%d admin=%q user=%q status=%q", code, store.gotAdmin, store.gotUser, store.gotStatus)
	}
	code, _ = get(t, s, "/api/debug/payouts/history?admin_did=x&status=done")
	if code != http.StatusBadRequest {
		t.Errorf("bad status: got %d, want 400", code)
	}
	// No since -> zero time passed through (no filter).
	get(t, s, "/api/debug/payouts/history?admin_did=x")
	if !store.gotSince.IsZero() || store.gotLimit != debugHistoryLimit {
		t.Errorf("defaults: since=%v limit=%d", store.gotSince, store.gotLimit)
	}
}

func chainOf(n int, headTx string) []rubix.ChainEntry {
	out := make([]rubix.ChainEntry, n)
	for i := range out {
		out[i] = rubix.ChainEntry{TransactionID: "tx" + string(rune('a'+i)), Epoch: int64(1000 + i)}
	}
	if n > 0 {
		out[n-1].TransactionID = headTx
	}
	return out
}

func TestDebugContracts(t *testing.T) {
	store := &fakeStore{contracts: []database.AdminContractRow{
		{AdminDID: did4, NodePort: "8003", ContractKind: "add_activity", ContractHash: "QmAct"},
		{AdminDID: did4, NodePort: "8003", ContractKind: "reward", ContractHash: scID},
		{AdminDID: "bafybmig3n5xn", NodePort: "8004", ContractKind: "reward", ContractHash: "QmDown"},
	}}
	fetch := func(_ context.Context, adminDID, hash string) ([]rubix.ChainEntry, error) {
		switch hash {
		case scID:
			return chainOf(4221, hexA), nil // head_position 4220
		case "QmAct":
			return chainOf(1, "deployTx"), nil
		default:
			return nil, errors.New("rubix call /rubix/v1/smart_contracts/QmDown/chain: connection refused")
		}
	}
	s := newTestServer(t, store, fetch)

	code, body := get(t, s, "/api/debug/contracts")
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	admins := data(t, body)["admins"].([]any)
	if len(admins) != 2 {
		t.Fatalf("admins = %d, want 2", len(admins))
	}
	a0 := admins[0].(map[string]any)
	if a0["admin_did"] != did4 || a0["node_port"] != "8003" {
		t.Errorf("admin 0 = %v", a0)
	}
	cs := a0["contracts"].([]any)
	reward := cs[1].(map[string]any)
	if reward["kind"] != "reward" || reward["chain_length"].(float64) != 4221 ||
		reward["head_position"].(float64) != 4220 || reward["head_tx"] != hexA || reward["error"] != nil {
		t.Errorf("reward contract = %v", reward)
	}
	a1 := admins[1].(map[string]any)
	down := a1["contracts"].([]any)[0].(map[string]any)
	if !strings.Contains(down["error"].(string), "connection refused") || down["head_tx"] != "" {
		t.Errorf("unreachable node must report error per contract, got %v", down)
	}

	// Filter to one admin.
	_, body = get(t, s, "/api/debug/contracts?admin_did="+did4)
	if n := len(data(t, body)["admins"].([]any)); n != 1 {
		t.Errorf("filtered admins = %d, want 1", n)
	}
}

func TestDebugForkCheck(t *testing.T) {
	lastOK := time.Date(2026, 9, 8, 9, 10, 9, 0, time.UTC)
	firstMM := lastOK.Add(3 * time.Second)
	seen := lastOK.Add(50 * time.Minute)
	store := &fakeStore{
		contracts: []database.AdminContractRow{{AdminDID: did4, NodePort: "8003", ContractKind: "reward", ContractHash: scID}},
		success:   &database.TransferStatus{TransactionID: hexC, CreatedAt: lastOK},
		mismatch:  &database.TransferStatus{RequestID: "req-mm", ErrorDetails: mismatchText(hexA, hexB), CreatedAt: seen},
		firstAt:   &firstMM,
	}
	ownerHead := hexB // owner still at the head the quorum said it expected: forked
	fetch := func(_ context.Context, _ string, hash string) ([]rubix.ChainEntry, error) {
		if hash != scID {
			t.Errorf("fetched %q, want the reward contract", hash)
		}
		return chainOf(4221, ownerHead), nil
	}
	s := newTestServer(t, store, fetch)

	code, body := get(t, s, "/api/debug/fork-check?admin_did="+did4)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	d := data(t, body)
	want := map[string]any{
		"contract": scID, "node_port": "8003", "quorum_head": hexA, "owner_expected": hexB,
		"owner_current_head": hexB, "owner_chain_length": float64(4221), "forked": true,
		"owner_has_quorum_head": false, "mismatch_stale": false,
		"last_success_tx": hexC, "last_success_at": "2026-09-08T09:10:09Z",
		"first_mismatch_at": "2026-09-08T09:10:12Z", "mismatch_request_id": "req-mm",
	}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("%s = %v, want %v", k, d[k], v)
		}
	}
	if !store.gotAfter.Equal(lastOK) {
		t.Errorf("first mismatch searched after %v, want last success %v", store.gotAfter, lastOK)
	}

	// After the replay the owner's head equals the quorum's: not forked.
	ownerHead = hexA
	_, body = get(t, s, "/api/debug/fork-check?admin_did="+did4)
	if d := data(t, body); d["forked"] != false || d["owner_current_head"] != hexA || d["owner_has_quorum_head"] != true {
		t.Errorf("after repair: %v", d)
	}

	// Repaired and moved on: the quorum's block is deep in the chain and
	// newer successes exist. Must NOT report a fork.
	ownerHead = "newerHead"
	s.fetchChain = func(context.Context, string, string) ([]rubix.ChainEntry, error) {
		ch := chainOf(4230, ownerHead)
		ch[4220].TransactionID = hexA
		return ch, nil
	}
	store.success = &database.TransferStatus{TransactionID: "newerHead", CreatedAt: seen.Add(time.Hour)}
	store.firstAt = nil
	_, body = get(t, s, "/api/debug/fork-check?admin_did="+did4)
	if d := data(t, body); d["forked"] != false || d["owner_has_quorum_head"] != true || d["mismatch_stale"] != true || d["first_mismatch_at"] != nil {
		t.Errorf("repaired and moved on: %v", d)
	}

	// Stale mismatch but the quorum block never made it onto the owner
	// (e.g. the owner was re-deployed): still not forked, with a note.
	s.fetchChain = func(context.Context, string, string) ([]rubix.ChainEntry, error) { return chainOf(5, "fresh"), nil }
	_, body = get(t, s, "/api/debug/fork-check?admin_did="+did4)
	if d := data(t, body); d["forked"] != false || d["mismatch_stale"] != true || d["owner_has_quorum_head"] != false || d["note"] == nil {
		t.Errorf("stale without block: %v", d)
	}

	// Empty chain from the node is an error, not a fork.
	s.fetchChain = func(context.Context, string, string) ([]rubix.ChainEntry, error) { return nil, nil }
	_, body = get(t, s, "/api/debug/fork-check?admin_did="+did4)
	if d := data(t, body); d["forked"] != false || d["node_error"] == nil {
		t.Errorf("empty chain: %v", d)
	}
	store.success = &database.TransferStatus{TransactionID: hexC, CreatedAt: lastOK}
	store.firstAt = &firstMM
	s.fetchChain = fetch
	ownerHead = hexA

	// Node unreachable: 200 with node_error, forked stays false.
	s.fetchChain = func(context.Context, string, string) ([]rubix.ChainEntry, error) {
		return nil, errors.New("connection refused")
	}
	code, body = get(t, s, "/api/debug/fork-check?admin_did="+did4)
	if d := data(t, body); code != http.StatusOK || d["node_error"] == nil || d["forked"] != false {
		t.Errorf("node down: code=%d data=%v", code, d)
	}

	// No mismatch rows at all.
	store.mismatch = nil
	s.fetchChain = fetch
	_, body = get(t, s, "/api/debug/fork-check?admin_did="+did4)
	if d := data(t, body); d["forked"] != false || d["quorum_head"] != "" || d["note"] == nil || d["owner_current_head"] != hexA {
		t.Errorf("no mismatch: %v", d)
	}

	code, _ = get(t, s, "/api/debug/fork-check")
	if code != http.StatusBadRequest {
		t.Errorf("missing admin_did: got %d, want 400", code)
	}
}
