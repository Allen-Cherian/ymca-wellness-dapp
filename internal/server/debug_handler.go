package server

// Operator diagnostics under /api/debug/. Read-only, behind RequireAuth
// like every other /api route. Each endpoint is one of the hand-written
// psql / curl checks from docs/INCIDENT-2026-09-04-chain-fork.md; see
// docs/debug-api.md for the contract. Handlers stay thin: SQL lives in
// internal/database/queries.go, the node call goes through the existing
// rubix client.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/rubix"
)

const (
	debugDefaultWindow   = 24 * time.Hour
	debugHistoryLimit    = 100
	debugHistoryMaxLimit = 1000
	debugFailuresLimit   = 20
	debugFailuresMax     = 200
	// debugNodeTimeout caps one chain fetch from a node so a hung node
	// cannot pin the whole /contracts response to the rubix client
	// timeout (120 s).
	debugNodeTimeout = 15 * time.Second
	// debugNodeParallel bounds concurrent chain fetches across nodes.
	debugNodeParallel = 5
)

// debugStore is the slice of internal/database the debug handlers use.
// It exists so handler tests can run without Postgres.
type debugStore interface {
	PayoutSummary(ctx context.Context, since time.Time, adminDID string) ([]database.PayoutSummaryRow, error)
	PayoutFailures(ctx context.Context, since time.Time, adminDID string, limit int) ([]database.FailureGroup, error)
	PayoutHistory(ctx context.Context, adminDID, status string, since time.Time, limit int) ([]database.TransferStatus, error)
	ListAdminContracts(ctx context.Context, adminDID string) ([]database.AdminContractRow, error)
	LatestRewardMismatch(ctx context.Context, adminDID string) (*database.TransferStatus, error)
	LatestRewardSuccess(ctx context.Context, adminDID string) (*database.TransferStatus, error)
	FirstRewardMismatchAfter(ctx context.Context, adminDID string, t time.Time) (*time.Time, error)
}

// dbDebugStore is the production debugStore: package-level database funcs.
type dbDebugStore struct{}

func (dbDebugStore) PayoutSummary(ctx context.Context, since time.Time, adminDID string) ([]database.PayoutSummaryRow, error) {
	return database.PayoutSummary(ctx, since, adminDID)
}
func (dbDebugStore) PayoutFailures(ctx context.Context, since time.Time, adminDID string, limit int) ([]database.FailureGroup, error) {
	return database.PayoutFailures(ctx, since, adminDID, limit)
}
func (dbDebugStore) PayoutHistory(ctx context.Context, adminDID, status string, since time.Time, limit int) ([]database.TransferStatus, error) {
	return database.PayoutHistory(ctx, adminDID, status, since, limit)
}
func (dbDebugStore) ListAdminContracts(ctx context.Context, adminDID string) ([]database.AdminContractRow, error) {
	return database.ListAdminContracts(ctx, adminDID)
}
func (dbDebugStore) LatestRewardMismatch(ctx context.Context, adminDID string) (*database.TransferStatus, error) {
	return database.LatestRewardMismatch(ctx, adminDID)
}
func (dbDebugStore) LatestRewardSuccess(ctx context.Context, adminDID string) (*database.TransferStatus, error) {
	return database.LatestRewardSuccess(ctx, adminDID)
}
func (dbDebugStore) FirstRewardMismatchAfter(ctx context.Context, adminDID string, t time.Time) (*time.Time, error) {
	return database.FirstRewardMismatchAfter(ctx, adminDID, t)
}

// chainFetcher fetches a contract's chain from its owner's node.
// Production: Server.fetchChainFromNode. Tests substitute a fake.
type chainFetcher func(ctx context.Context, adminDID, contractHash string) ([]rubix.ChainEntry, error)

// fetchChainFromNode calls GET /rubix/v1/smart_contracts/{id}/chain on
// the admin's node (http://localhost:<node_port>).
func (s *Server) fetchChainFromNode(ctx context.Context, adminDID, contractHash string) ([]rubix.ChainEntry, error) {
	c, _, err := s.Svc.ClientFor(adminDID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, debugNodeTimeout)
	defer cancel()
	return c.GetSmartContractChain(ctx, contractHash)
}

// ---------------------------------------------------------------------------
// Query-param helpers
// ---------------------------------------------------------------------------

// sinceLayouts are accepted by ?since=, tried in order. RFC3339 first;
// the space-separated forms match what morning-check.sh takes and are
// read as UTC.
var sinceLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseSince parses ?since=. Empty returns def (which may be zero).
func parseSince(raw string, def time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	for _, layout := range sinceLayouts {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, errors.New("since must be RFC3339 (2026-09-08T10:30:00Z) or 'YYYY-MM-DD HH:MM' UTC")
}

// parseLimit parses ?limit= with a default and a ceiling.
func parseLimit(raw string, def, max int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("limit must be a positive integer")
	}
	if n > max {
		n = max
	}
	return n, nil
}

var validHistoryStatus = map[string]bool{
	"":                        true,
	database.StatusQueued:     true,
	database.StatusProcessing: true,
	database.StatusSuccess:    true,
	database.StatusFailed:     true,
}

// ---------------------------------------------------------------------------
// Mismatch error parsing
// ---------------------------------------------------------------------------

// The quorum's TokenChainIntigrityCheck failure, as stored in
// transfer_status.error_details:
//
//	... token <sc> (smart_contract) chain mismatch after sync from <did>:
//	local latest <quorumHead> != expected <ownerHead>
var (
	mismatchHeadsRe    = regexp.MustCompile(`local latest ([0-9a-f]{64}) != expected ([0-9a-f]{64})`)
	mismatchContractRe = regexp.MustCompile(`token (\S+) \(smart_contract\) chain mismatch`)
)

// mismatchInfo is what parseMismatch extracts from one error_details.
type mismatchInfo struct {
	Contract      string // may be empty on an unexpected message shape
	QuorumHead    string // "local latest" on the quorum
	OwnerExpected string // "expected": the owner's head as the quorum saw it
}

// parseMismatch pulls contract and heads out of a chain-mismatch error.
// ok is false when the two heads cannot be found.
func parseMismatch(errorDetails string) (info mismatchInfo, ok bool) {
	m := mismatchHeadsRe.FindStringSubmatch(errorDetails)
	if m == nil {
		return info, false
	}
	info.QuorumHead, info.OwnerExpected = m[1], m[2]
	if c := mismatchContractRe.FindStringSubmatch(errorDetails); c != nil {
		info.Contract = c[1]
	}
	return info, true
}

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

type debugSummaryRow struct {
	AdminDID        string     `json:"admin_did"`
	NodePort        string     `json:"node_port"`
	Total           int        `json:"total"`
	Success         int        `json:"success"`
	Failed          int        `json:"failed"`
	Mismatch        int        `json:"chain_mismatch"`
	InFlight        int        `json:"in_flight"`
	FirstMismatchAt *time.Time `json:"first_mismatch_at"`
	LastMismatchAt  *time.Time `json:"last_mismatch_at"`
	LastSuccessAt   *time.Time `json:"last_success_at"`
	LastSuccessTx   string     `json:"last_success_tx"`
}

type debugFailureGroup struct {
	Count    int       `json:"count"`
	Reason   string    `json:"reason"`
	FirstAt  time.Time `json:"first_at"`
	LatestAt time.Time `json:"latest_at"`
}

type debugTransferRow struct {
	RequestID     string    `json:"request_id"`
	TransactionID string    `json:"transaction_id"`
	Kind          string    `json:"kind"`
	AdminDID      string    `json:"admin_did"`
	UserDID       string    `json:"user_did"`
	ActivityIDs   []string  `json:"activity_ids"`
	RewardPoints  int       `json:"reward_points"`
	ContractHash  string    `json:"contract_hash"`
	Status        string    `json:"status"`
	Message       string    `json:"message"`
	ErrorDetails  string    `json:"error_details"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toDebugTransferRow(t database.TransferStatus) debugTransferRow {
	ids := t.ActivityIDs
	if ids == nil {
		ids = []string{}
	}
	return debugTransferRow{
		RequestID: t.RequestID, TransactionID: t.TransactionID, Kind: t.Kind,
		AdminDID: t.AdminDID, UserDID: t.UserDID, ActivityIDs: ids,
		RewardPoints: t.RewardPoints, ContractHash: t.ContractHash,
		Status: t.Status, Message: t.Message, ErrorDetails: t.ErrorDetails,
		CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(),
	}
}

type debugContract struct {
	Kind         string `json:"kind"`
	ContractHash string `json:"contract_hash"`
	// ChainLength counts every block including the deploy block, so the
	// head is at position ChainLength-1 (= tokens.latest_position).
	ChainLength  int    `json:"chain_length"`
	HeadPosition int    `json:"head_position"`
	HeadTx       string `json:"head_tx"`
	HeadEpoch    int64  `json:"head_epoch"`
	Error        string `json:"error,omitempty"`
}

type debugAdminContracts struct {
	AdminDID  string          `json:"admin_did"`
	NodePort  string          `json:"node_port"`
	Contracts []debugContract `json:"contracts"`
}

type debugForkCheck struct {
	AdminDID         string     `json:"admin_did"`
	NodePort         string     `json:"node_port"`
	Contract         string     `json:"contract"`
	QuorumHead       string     `json:"quorum_head"`
	OwnerExpected    string     `json:"owner_expected"`
	OwnerCurrentHead string     `json:"owner_current_head"`
	OwnerChainLength int        `json:"owner_chain_length"`
	Forked           bool       `json:"forked"`
	MismatchSeenAt   *time.Time `json:"mismatch_seen_at"`
	MismatchRequest  string     `json:"mismatch_request_id"`
	FirstMismatchAt  *time.Time `json:"first_mismatch_at"`
	LastSuccessTx    string     `json:"last_success_tx"`
	LastSuccessAt    *time.Time `json:"last_success_at"`
	NodeError        string     `json:"node_error,omitempty"`
	Note             string     `json:"note,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// GET /api/debug/payouts/summary?since=&admin_did=
func (s *Server) handleDebugPayoutSummary(c *gin.Context) {
	since, err := parseSince(c.Query("since"), time.Now().Add(-debugDefaultWindow))
	if err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	adminDID := c.Query("admin_did")
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	rows, err := s.debug.PayoutSummary(ctx, since, adminDID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}
	out := make([]debugSummaryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, debugSummaryRow{
			AdminDID: r.AdminDID, NodePort: r.NodePort, Total: r.Total, Success: r.Success,
			Failed: r.Failed, Mismatch: r.Mismatch, InFlight: r.InFlight,
			FirstMismatchAt: utcPtr(r.FirstMismatchAt), LastMismatchAt: utcPtr(r.LastMismatchAt),
			LastSuccessAt: utcPtr(r.LastSuccessAt), LastSuccessTx: r.LastSuccessTx,
		})
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"since":  since.UTC(),
		"admins": out,
	}})
}

// GET /api/debug/payouts/failures?since=&admin_did=&limit=
func (s *Server) handleDebugPayoutFailures(c *gin.Context) {
	since, err := parseSince(c.Query("since"), time.Now().Add(-debugDefaultWindow))
	if err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	limit, err := parseLimit(c.Query("limit"), debugFailuresLimit, debugFailuresMax)
	if err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	adminDID := c.Query("admin_did")
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	groups, err := s.debug.PayoutFailures(ctx, since, adminDID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}
	out := make([]debugFailureGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, debugFailureGroup{Count: g.Count, Reason: g.Reason, FirstAt: g.FirstAt.UTC(), LatestAt: g.LatestAt.UTC()})
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"since":    since.UTC(),
		"limit":    limit,
		"failures": out,
	}})
}

// GET /api/debug/payouts/history?admin_did=&status=&since=&limit=
func (s *Server) handleDebugPayoutHistory(c *gin.Context) {
	adminDID := c.Query("admin_did")
	if adminDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: "admin_did is required"})
		return
	}
	status := c.Query("status")
	if !validHistoryStatus[status] {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: "status must be one of queued, processing, success, failed"})
		return
	}
	since, err := parseSince(c.Query("since"), time.Time{})
	if err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	limit, err := parseLimit(c.Query("limit"), debugHistoryLimit, debugHistoryMaxLimit)
	if err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	rows, err := s.debug.PayoutHistory(ctx, adminDID, status, since, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}
	out := make([]debugTransferRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toDebugTransferRow(r))
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did": adminDID,
		"status":    status,
		"limit":     limit,
		"count":     len(out),
		"rows":      out,
	}})
}

// GET /api/debug/contracts?admin_did=
func (s *Server) handleDebugContracts(c *gin.Context) {
	adminDID := c.Query("admin_did")
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	rows, err := s.debug.ListAdminContracts(ctx, adminDID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}

	// Group by admin, preserving the query's node_port order.
	var admins []*debugAdminContracts
	byAdmin := map[string]*debugAdminContracts{}
	type slot struct {
		admin string
		hash  string
		dst   *debugContract
	}
	var slots []slot
	for _, r := range rows {
		a, ok := byAdmin[r.AdminDID]
		if !ok {
			a = &debugAdminContracts{AdminDID: r.AdminDID, NodePort: r.NodePort, Contracts: []debugContract{}}
			byAdmin[r.AdminDID] = a
			admins = append(admins, a)
		}
		a.Contracts = append(a.Contracts, debugContract{Kind: r.ContractKind, ContractHash: r.ContractHash})
	}
	for _, a := range admins {
		for i := range a.Contracts {
			slots = append(slots, slot{admin: a.AdminDID, hash: a.Contracts[i].ContractHash, dst: &a.Contracts[i]})
		}
	}

	// Fetch heads from the nodes, a few at a time. A node failure is
	// reported on that contract, never as a 500.
	var wg sync.WaitGroup
	sem := make(chan struct{}, debugNodeParallel)
	for _, sl := range slots {
		wg.Add(1)
		go func(sl slot) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fillHead(ctx, s.fetchChain, sl.admin, sl.hash, sl.dst)
		}(sl)
	}
	wg.Wait()

	if admins == nil {
		admins = []*debugAdminContracts{}
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did": adminDID,
		"admins":    admins,
	}})
}

// fillHead populates dst's chain fields from the owner node, or dst.Error.
func fillHead(ctx context.Context, fetch chainFetcher, adminDID, hash string, dst *debugContract) {
	chain, err := fetch(ctx, adminDID, hash)
	if err != nil {
		dst.Error = err.Error()
		return
	}
	dst.ChainLength = len(chain)
	dst.HeadPosition = len(chain) - 1
	if len(chain) > 0 {
		head := chain[len(chain)-1]
		dst.HeadTx = head.TransactionID
		dst.HeadEpoch = head.Epoch
	}
}

// GET /api/debug/fork-check?admin_did=
func (s *Server) handleDebugForkCheck(c *gin.Context) {
	adminDID := c.Query("admin_did")
	if adminDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: "admin_did is required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	out := debugForkCheck{AdminDID: adminDID}

	// Reward contract and node port from the registry.
	contracts, err := s.debug.ListAdminContracts(ctx, adminDID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}
	for _, r := range contracts {
		out.NodePort = r.NodePort
		if r.ContractKind == "reward" {
			out.Contract = r.ContractHash
		}
	}

	// Last success, always reported.
	if last, err := s.debug.LatestRewardSuccess(ctx, adminDID); err == nil {
		out.LastSuccessTx = last.TransactionID
		out.LastSuccessAt = utcPtr(&last.CreatedAt)
	} else if !errors.Is(err, database.ErrNotFound) {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}

	// Newest chain-mismatch failure.
	mm, err := s.debug.LatestRewardMismatch(ctx, adminDID)
	if errors.Is(err, database.ErrNotFound) {
		out.Note = "no chain-mismatch failures recorded for this admin"
		s.fillOwnerHead(ctx, &out)
		c.JSON(http.StatusOK, okResponse{Status: true, Data: out})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}
	out.MismatchSeenAt = utcPtr(&mm.CreatedAt)
	out.MismatchRequest = mm.RequestID
	info, ok := parseMismatch(mm.ErrorDetails)
	if !ok {
		out.Note = "newest chain-mismatch row did not contain 'local latest <hash> != expected <hash>'"
	} else {
		out.QuorumHead = info.QuorumHead
		out.OwnerExpected = info.OwnerExpected
		if info.Contract != "" {
			out.Contract = info.Contract
		}
	}

	// First mismatch after the last success = when the fork happened.
	var after time.Time
	if out.LastSuccessAt != nil {
		after = *out.LastSuccessAt
	}
	first, err := s.debug.FirstRewardMismatchAfter(ctx, adminDID, after)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Query failed", Message: err.Error()})
		return
	}
	out.FirstMismatchAt = utcPtr(first)
	if first == nil && out.Note == "" {
		out.Note = "newest mismatch predates the last success: already repaired"
	}

	s.fillOwnerHead(ctx, &out)
	if out.QuorumHead != "" && out.NodeError == "" {
		out.Forked = out.QuorumHead != out.OwnerCurrentHead
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: out})
}

// fillOwnerHead fetches the owner's current head for out.Contract.
func (s *Server) fillOwnerHead(ctx context.Context, out *debugForkCheck) {
	if out.Contract == "" {
		out.NodeError = "no reward contract registered for this admin"
		return
	}
	var dc debugContract
	fillHead(ctx, s.fetchChain, out.AdminDID, out.Contract, &dc)
	if dc.Error != "" {
		out.NodeError = dc.Error
		return
	}
	out.OwnerCurrentHead = dc.HeadTx
	out.OwnerChainLength = dc.ChainLength
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
