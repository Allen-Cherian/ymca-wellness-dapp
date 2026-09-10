package server

// Operator diagnostics that read each Rubix node's own Postgres, under
// /api/debug/node/. Read-only; see docs/debug-api.md. Disabled (503)
// until NODE_DB_PASSWORD is set. SQL lives in internal/nodedb.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/nodedb"
)

const (
	debugChainTailLimit    = 20
	debugChainTailMaxLimit = 500
	debugUnpledgeLimit     = 20
	// debugNodeDBTimeout caps one node database call.
	debugNodeDBTimeout = 10 * time.Second
)

// nodeStore is the per-node database access the handlers use, keyed by
// the node's API port. Production: nodedbStore over nodedb.Pools; tests
// substitute a fake.
type nodeStore interface {
	Enabled() bool
	DBPort(nodePort string) (string, error)
	ContractStates(ctx context.Context, nodePort string, tokenIDs []string) (map[string]nodedb.ContractState, error)
	ChainTail(ctx context.Context, nodePort, tokenID string, limit int) ([]nodedb.ChainRow, error)
	GetTransaction(ctx context.Context, nodePort, txID string) (*nodedb.Transaction, error)
	LockedTokens(ctx context.Context, nodePort string) ([]nodedb.LockRow, error)
	TokenInventory(ctx context.Context, nodePort, did string, unpledgeLimit int) (*nodedb.Inventory, error)
	QuorumMembers(ctx context.Context, nodePort string) ([]nodedb.QuorumRow, error)
}

// nodedbStore is the production nodeStore.
type nodedbStore struct{ pools *nodedb.Pools }

func (s nodedbStore) Enabled() bool { return s.pools != nil && s.pools.Config().Enabled() }
func (s nodedbStore) DBPort(nodePort string) (string, error) {
	if s.pools == nil {
		return "", nodedb.ErrNotConfigured
	}
	return s.pools.Config().DBPort(nodePort)
}
func (s nodedbStore) pool(ctx context.Context, nodePort string) (nodedb.Querier, context.Context, context.CancelFunc, error) {
	if s.pools == nil {
		return nil, nil, nil, nodedb.ErrNotConfigured
	}
	p, err := s.pools.For(ctx, nodePort)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, debugNodeDBTimeout)
	return p, ctx, cancel, nil
}
func (s nodedbStore) ContractStates(ctx context.Context, nodePort string, ids []string) (map[string]nodedb.ContractState, error) {
	q, ctx, cancel, err := s.pool(ctx, nodePort)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return nodedb.ContractStates(ctx, q, ids)
}
func (s nodedbStore) ChainTail(ctx context.Context, nodePort, tokenID string, limit int) ([]nodedb.ChainRow, error) {
	q, ctx, cancel, err := s.pool(ctx, nodePort)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return nodedb.ChainTail(ctx, q, tokenID, limit)
}
func (s nodedbStore) GetTransaction(ctx context.Context, nodePort, txID string) (*nodedb.Transaction, error) {
	q, ctx, cancel, err := s.pool(ctx, nodePort)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return nodedb.GetTransaction(ctx, q, txID)
}
func (s nodedbStore) LockedTokens(ctx context.Context, nodePort string) ([]nodedb.LockRow, error) {
	q, ctx, cancel, err := s.pool(ctx, nodePort)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return nodedb.LockedTokens(ctx, q)
}
func (s nodedbStore) TokenInventory(ctx context.Context, nodePort, did string, limit int) (*nodedb.Inventory, error) {
	q, ctx, cancel, err := s.pool(ctx, nodePort)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return nodedb.TokenInventory(ctx, q, did, limit)
}
func (s nodedbStore) QuorumMembers(ctx context.Context, nodePort string) ([]nodedb.QuorumRow, error) {
	q, ctx, cancel, err := s.pool(ctx, nodePort)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return nodedb.QuorumMembers(ctx, q)
}

// UseNodeDB enables /api/debug/node/* over the given pools. Call once
// after New; without it (or with an unconfigured Config) the routes
// answer 503.
func (s *Server) UseNodeDB(p *nodedb.Pools) { s.node = nodedbStore{pools: p} }

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

type nodeContractState struct {
	Kind            string    `json:"kind"`
	ContractHash    string    `json:"contract_hash"`
	Found           bool      `json:"found"`
	TokenType       string    `json:"token_type,omitempty"`
	Status          int       `json:"status"`
	StatusName      string    `json:"status_name"`
	LatestPosition  int64     `json:"latest_position"`
	LatestRole      int       `json:"latest_role"`
	LatestRoleName  string    `json:"latest_role_name"`
	HeadTx          string    `json:"head_tx"`
	LockReferenceID string    `json:"lock_reference_id"`
	OwnerDID        string    `json:"owner_did"`
	UpdatedAt       time.Time `json:"updated_at"`
	TokenchainRows  int64     `json:"tokenchain_rows"`
	IndexLen        int64     `json:"index_len"`
	JoinedRows      int64     `json:"joined_rows"`
	MaxPosition     int64     `json:"max_position"`
	Locked          bool      `json:"locked"`
	StaleLockRef    bool      `json:"stale_lock_ref"`
	Consistent      bool      `json:"consistent"`
}

type nodeAdminContracts struct {
	AdminDID  string              `json:"admin_did"`
	NodePort  string              `json:"node_port"`
	DBPort    string              `json:"db_port"`
	Error     string              `json:"error,omitempty"`
	Contracts []nodeContractState `json:"contracts"`
}

type nodeChainRow struct {
	Position       int64     `json:"position"`
	TransactionID  string    `json:"transaction_id"`
	PreviousTx     string    `json:"previous_tx"`
	Role           int       `json:"role"`
	RoleName       string    `json:"role_name"`
	CreatedAt      time.Time `json:"created_at"`
	HasTransaction bool      `json:"has_transaction"`
}

type nodeLockRow struct {
	AdminDID        string    `json:"admin_did"`
	NodePort        string    `json:"node_port"`
	TokenID         string    `json:"token_id"`
	TokenType       string    `json:"token_type"`
	OwnerDID        string    `json:"owner_did"`
	LockReferenceID string    `json:"lock_reference_id"`
	LatestPosition  int64     `json:"latest_position"`
	LockedSince     time.Time `json:"locked_since"`
	LockedFor       string    `json:"locked_for"`
	Kind            string    `json:"kind,omitempty"` // from admin_contracts when the token is a registered contract
}

type nodeLocksResult struct {
	AdminDID string `json:"admin_did"`
	NodePort string `json:"node_port"`
	Error    string `json:"error,omitempty"`
	Count    int    `json:"count"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// nodeDBReady answers 503 when the node database is not configured.
func (s *Server) nodeDBReady(c *gin.Context) bool {
	if s.node == nil || !s.node.Enabled() {
		c.JSON(http.StatusServiceUnavailable, errResponse{Error: "Node database access not configured", Message: nodedb.ErrNotConfigured.Error()})
		return false
	}
	return true
}

// adminNode is one admin's node port and registered contracts.
type adminNode struct {
	AdminDID  string
	NodePort  string
	Contracts []database.AdminContractRow // kind, hash
}

// adminNodes groups admin_contracts rows per admin, in node-port order.
// When adminDID is set and has no contracts, the admin is still returned
// (node port from config) so node-level queries work.
func (s *Server) adminNodes(ctx context.Context, adminDID string) ([]adminNode, error) {
	rows, err := s.debug.ListAdminContracts(ctx, adminDID)
	if err != nil {
		return nil, err
	}
	var out []adminNode
	idx := map[string]int{}
	for _, r := range rows {
		i, ok := idx[r.AdminDID]
		if !ok {
			i = len(out)
			idx[r.AdminDID] = i
			out = append(out, adminNode{AdminDID: r.AdminDID, NodePort: r.NodePort})
		}
		out[i].Contracts = append(out[i].Contracts, r)
	}
	if adminDID != "" && len(out) == 0 {
		if a, ok := s.Cfg.AdminByDID(adminDID); ok {
			out = append(out, adminNode{AdminDID: adminDID, NodePort: a.NodePort})
		}
	}
	return out, nil
}

// requireAdminNode resolves ?admin_did= to its node, or answers 400/404.
func (s *Server) requireAdminNode(c *gin.Context, ctx context.Context) (adminNode, bool) {
	adminDID := c.Query("admin_did")
	if adminDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: "admin_did is required"})
		return adminNode{}, false
	}
	nodes, err := s.adminNodes(ctx, adminDID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return adminNode{}, false
	}
	if len(nodes) == 0 || nodes[0].NodePort == "" {
		c.JSON(http.StatusNotFound, errResponse{Error: "Unknown admin_did", Message: "no node port registered for " + adminDID})
		return adminNode{}, false
	}
	return nodes[0], true
}

// forEachNode runs fn for every admin node with bounded parallelism.
func forEachNode(nodes []adminNode, fn func(n adminNode)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, debugNodeParallel)
	for _, n := range nodes {
		wg.Add(1)
		go func(n adminNode) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(n)
		}(n)
	}
	wg.Wait()
}

func (s *Server) dbPortOf(nodePort string) string {
	p, err := s.node.DBPort(nodePort)
	if err != nil {
		return ""
	}
	return p
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// GET /api/debug/node/contracts?admin_did=
func (s *Server) handleDebugNodeContracts(c *gin.Context) {
	if !s.nodeDBReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()
	nodes, err := s.adminNodes(ctx, c.Query("admin_did"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}

	results := make([]nodeAdminContracts, len(nodes))
	var mu sync.Mutex
	forEachNode(nodes, func(n adminNode) {
		res := nodeAdminContracts{AdminDID: n.AdminDID, NodePort: n.NodePort, DBPort: s.dbPortOf(n.NodePort), Contracts: []nodeContractState{}}
		ids := make([]string, 0, len(n.Contracts))
		for _, ct := range n.Contracts {
			ids = append(ids, ct.ContractHash)
		}
		states, err := s.node.ContractStates(ctx, n.NodePort, ids)
		if err != nil {
			res.Error = err.Error()
		}
		for _, ct := range n.Contracts {
			row := nodeContractState{Kind: ct.ContractKind, ContractHash: ct.ContractHash}
			if st, ok := states[ct.ContractHash]; ok {
				row.Found = true
				row.TokenType, row.Status, row.StatusName = st.TokenType, st.Status, st.StatusName
				row.LatestPosition, row.LatestRole, row.LatestRoleName = st.LatestPosition, st.LatestRole, st.LatestRoleName
				row.HeadTx, row.LockReferenceID, row.OwnerDID, row.UpdatedAt = st.HeadTx, st.LockReferenceID, st.OwnerDID, st.UpdatedAt.UTC()
				row.TokenchainRows, row.IndexLen, row.JoinedRows, row.MaxPosition = st.TokenchainRows, st.IndexLen, st.JoinedRows, st.MaxPosition
				row.Locked, row.StaleLockRef, row.Consistent = st.Locked(), st.StaleLockRef(), st.Consistent()
			}
			res.Contracts = append(res.Contracts, row)
		}
		mu.Lock()
		for i := range nodes {
			if nodes[i].AdminDID == n.AdminDID {
				results[i] = res
			}
		}
		mu.Unlock()
	})
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{"admin_did": c.Query("admin_did"), "admins": results}})
}

// GET /api/debug/node/chain?admin_did=&kind=reward&limit=
func (s *Server) handleDebugNodeChain(c *gin.Context) {
	if !s.nodeDBReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()
	n, ok := s.requireAdminNode(c, ctx)
	if !ok {
		return
	}
	kind := c.DefaultQuery("kind", "reward")
	limit, err := parseLimit(c.Query("limit"), debugChainTailLimit, debugChainTailMaxLimit)
	if err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	var hash string
	for _, ct := range n.Contracts {
		if ct.ContractKind == kind {
			hash = ct.ContractHash
		}
	}
	if hash == "" {
		c.JSON(http.StatusNotFound, errResponse{Error: "Contract not found", Message: "no " + kind + " contract registered for " + n.AdminDID})
		return
	}
	rows, err := s.node.ChainTail(ctx, n.NodePort, hash, limit)
	if err != nil {
		c.JSON(http.StatusBadGateway, errResponse{Error: "Node database call failed", Message: err.Error()})
		return
	}
	out := make([]nodeChainRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, nodeChainRow{Position: r.Position, TransactionID: r.TransactionID, PreviousTx: r.PreviousTx,
			Role: r.Role, RoleName: r.RoleName, CreatedAt: r.CreatedAt.UTC(), HasTransaction: r.HasTransaction})
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did": n.AdminDID, "node_port": n.NodePort, "db_port": s.dbPortOf(n.NodePort),
		"kind": kind, "contract_hash": hash, "limit": limit, "count": len(out), "rows": out,
	}})
}

// GET /api/debug/node/transaction?admin_did=&tx=
func (s *Server) handleDebugNodeTransaction(c *gin.Context) {
	if !s.nodeDBReady(c) {
		return
	}
	txID := c.Query("tx")
	if txID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: "tx is required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()
	n, ok := s.requireAdminNode(c, ctx)
	if !ok {
		return
	}
	t, err := s.node.GetTransaction(ctx, n.NodePort, txID)
	if errors.Is(err, nodedb.ErrNotFound) {
		c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
			"admin_did": n.AdminDID, "node_port": n.NodePort, "db_port": s.dbPortOf(n.NodePort),
			"tx": txID, "found": false,
		}})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, errResponse{Error: "Node database call failed", Message: err.Error()})
		return
	}
	units := make([]gin.H, 0, len(t.Units))
	for _, u := range t.Units {
		units = append(units, gin.H{"did": u.DID, "execution_role": u.ExecutionRole, "status": u.Status, "created_at": u.CreatedAt.UTC()})
	}
	refs := make([]gin.H, 0, len(t.ChainRefs))
	for _, r := range t.ChainRefs {
		refs = append(refs, gin.H{"token_id": r.TokenID, "position": r.Position, "role": r.Role})
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did": n.AdminDID, "node_port": n.NodePort, "db_port": s.dbPortOf(n.NodePort),
		"tx": t.ID, "found": true, "created_at": t.CreatedAt.UTC(),
		"info_hash": t.InfoHash, "info_hash_matches_id": t.InfoHash == t.ID,
		"info": t.Info, "signature": t.Signature,
		"units": units, "chain_refs": refs,
	}})
}

// GET /api/debug/node/locks?admin_did=
func (s *Server) handleDebugNodeLocks(c *gin.Context) {
	if !s.nodeDBReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()
	nodes, err := s.adminNodes(ctx, c.Query("admin_did"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}
	now := time.Now()
	var mu sync.Mutex
	var locks []nodeLockRow
	perNode := make([]nodeLocksResult, 0, len(nodes))
	forEachNode(nodes, func(n adminNode) {
		kinds := map[string]string{}
		for _, ct := range n.Contracts {
			kinds[ct.ContractHash] = ct.ContractKind
		}
		rows, err := s.node.LockedTokens(ctx, n.NodePort)
		res := nodeLocksResult{AdminDID: n.AdminDID, NodePort: n.NodePort, Count: len(rows)}
		if err != nil {
			res.Error = err.Error()
		}
		mu.Lock()
		defer mu.Unlock()
		perNode = append(perNode, res)
		for _, r := range rows {
			locks = append(locks, nodeLockRow{
				AdminDID: n.AdminDID, NodePort: n.NodePort, TokenID: r.TokenID, TokenType: r.TokenType,
				OwnerDID: r.OwnerDID, LockReferenceID: r.LockReferenceID, LatestPosition: r.LatestPosition,
				LockedSince: r.UpdatedAt.UTC(), LockedFor: now.Sub(r.UpdatedAt).Truncate(time.Second).String(),
				Kind: kinds[r.TokenID],
			})
		}
	})
	sort.Slice(perNode, func(i, j int) bool { return perNode[i].NodePort < perNode[j].NodePort })
	sort.Slice(locks, func(i, j int) bool {
		if locks[i].NodePort != locks[j].NodePort {
			return locks[i].NodePort < locks[j].NodePort
		}
		return locks[i].LockedSince.Before(locks[j].LockedSince)
	})
	if locks == nil {
		locks = []nodeLockRow{}
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did": c.Query("admin_did"), "nodes": perNode, "count": len(locks), "locks": locks,
	}})
}

// GET /api/debug/node/tokens?admin_did=
func (s *Server) handleDebugNodeTokens(c *gin.Context) {
	if !s.nodeDBReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()
	n, ok := s.requireAdminNode(c, ctx)
	if !ok {
		return
	}
	inv, err := s.node.TokenInventory(ctx, n.NodePort, n.AdminDID, debugUnpledgeLimit)
	if err != nil {
		c.JSON(http.StatusBadGateway, errResponse{Error: "Node database call failed", Message: err.Error()})
		return
	}
	buckets := make([]gin.H, 0, len(inv.Buckets))
	for _, b := range inv.Buckets {
		buckets = append(buckets, gin.H{"token_type": b.TokenType, "status": b.Status, "status_name": b.StatusName, "count": b.Count, "value": b.Value})
	}
	denoms := make([]gin.H, 0, len(inv.Denoms))
	for _, d := range inv.Denoms {
		denoms = append(denoms, gin.H{"denom": d.Denom, "count": d.Count})
	}
	pending := make([]gin.H, 0, len(inv.PendingUnpledge))
	for _, u := range inv.PendingUnpledge {
		pending = append(pending, gin.H{"tx_id": u.TxID, "quorum_did": u.QuorumDID, "pledge_tokens": u.PledgeTokens, "epoch": u.Epoch, "created_at": u.CreatedAt.UTC()})
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did": n.AdminDID, "node_port": n.NodePort, "db_port": s.dbPortOf(n.NodePort),
		"buckets": buckets, "denoms": denoms,
		"pending_unpledge_count": inv.PendingCount, "pending_unpledge": pending,
	}})
}

// GET /api/debug/node/quorum?admin_did=
func (s *Server) handleDebugNodeQuorum(c *gin.Context) {
	if !s.nodeDBReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()
	nodes, err := s.adminNodes(ctx, c.Query("admin_did"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}
	type result struct {
		AdminDID string   `json:"admin_did"`
		NodePort string   `json:"node_port"`
		Error    string   `json:"error,omitempty"`
		Quorum   []string `json:"quorum"`
	}
	results := make([]result, len(nodes))
	var mu sync.Mutex
	forEachNode(nodes, func(n adminNode) {
		res := result{AdminDID: n.AdminDID, NodePort: n.NodePort, Quorum: []string{}}
		rows, err := s.node.QuorumMembers(ctx, n.NodePort)
		if err != nil {
			res.Error = err.Error()
		}
		for _, r := range rows {
			res.Quorum = append(res.Quorum, r.DID)
		}
		mu.Lock()
		for i := range nodes {
			if nodes[i].AdminDID == n.AdminDID {
				results[i] = res
			}
		}
		mu.Unlock()
	})
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{"admin_did": c.Query("admin_did"), "admins": results}})
}
