// Package nodedb reads a Rubix node's own Postgres (the nodeN-postgres
// containers) for the operator diagnostics under /api/debug/node/. It is
// strictly read-only: every statement is a SELECT and every session is
// opened with default_transaction_read_only=on (see docs/debug-api.md).
//
// Schema: rubixgoplatform core/storage/schema.go. Status and role numbers
// are the platform's constants (constants/constants.go).
package nodedb

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/sha3"
)

// ErrNotConfigured is returned when NODE_DB_PASSWORD is unset.
var ErrNotConfigured = errors.New("nodedb: node database access is not configured (NODE_DB_PASSWORD)")

// ErrNotFound is returned when a row does not exist on the node.
var ErrNotFound = errors.New("nodedb: not found")

// Config is how the dApp reaches each node's Postgres. The database port
// for a node is its API port plus PortOffset (yqa: API 8000 → DB 9000).
type Config struct {
	Host       string
	PortOffset int
	User       string
	Password   string
	DBName     string
	// ConnectTimeout bounds opening a pool and each query.
	ConnectTimeout time.Duration
}

// Enabled reports whether a password was configured.
func (c Config) Enabled() bool { return c.Password != "" }

// DBPort maps a node API port ("8003") to its database port ("9003").
func (c Config) DBPort(nodePort string) (string, error) {
	p, err := strconv.Atoi(nodePort)
	if err != nil {
		return "", fmt.Errorf("nodedb: bad node port %q", nodePort)
	}
	return strconv.Itoa(p + c.PortOffset), nil
}

// Pools lazily opens one small pgxpool per node database.
type Pools struct {
	cfg   Config
	mu    sync.Mutex
	pools map[string]*pgxpool.Pool // key = db port
}

// NewPools builds an empty pool set.
func NewPools(cfg Config) *Pools {
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}
	return &Pools{cfg: cfg, pools: make(map[string]*pgxpool.Pool)}
}

// Config returns the configuration.
func (p *Pools) Config() Config { return p.cfg }

// For returns the pool for the node with the given API port.
func (p *Pools) For(ctx context.Context, nodePort string) (*pgxpool.Pool, error) {
	if !p.cfg.Enabled() {
		return nil, ErrNotConfigured
	}
	dbPort, err := p.cfg.DBPort(nodePort)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if pool, ok := p.pools[dbPort]; ok {
		return pool, nil
	}
	// Fields are set directly rather than through a DSN string so a
	// password with spaces or quotes cannot break parsing.
	pc, err := pgxpool.ParseConfig("sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("nodedb: base config: %w", err)
	}
	port, _ := strconv.Atoi(dbPort)
	pc.ConnConfig.Host = p.cfg.Host
	pc.ConnConfig.Port = uint16(port)
	pc.ConnConfig.User = p.cfg.User
	pc.ConnConfig.Password = p.cfg.Password
	pc.ConnConfig.Database = p.cfg.DBName
	pc.ConnConfig.ConnectTimeout = p.cfg.ConnectTimeout
	pc.MaxConns = 2
	pc.MinConns = 0
	pc.MaxConnIdleTime = 2 * time.Minute
	// Read-only at the session level too, in case the role is not.
	pc.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pc.ConnConfig.RuntimeParams["application_name"] = "ymca-dapp-debug"
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("nodedb: open port %s: %w", dbPort, err)
	}
	p.pools[dbPort] = pool
	return pool, nil
}

// Close releases every pool.
func (p *Pools) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, pool := range p.pools {
		pool.Close()
		delete(p.pools, k)
	}
}

// Querier is what the query helpers need; *pgxpool.Pool satisfies it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ---------------------------------------------------------------------------
// Status names (rubixgoplatform constants/constants.go)
// ---------------------------------------------------------------------------

var statusNames = map[int]string{
	0: "free", 1: "locked", 2: "generated", 3: "fetched", 4: "transferred", 5: "committed",
	6: "pledged", 7: "quorum_pledged", 8: "burnt", 9: "burnt_for_ft", 10: "deployed",
	11: "executed", 12: "pinned_as_service", 13: "orphaned", 14: "chain_sync_issue",
	15: "being_double_spent", 99: "seed",
}

// StatusName returns the platform's name for a token_status value.
func StatusName(s int) string {
	if n, ok := statusNames[s]; ok {
		return n
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// Contract state and chain consistency (morning-check.sh, last block)
// ---------------------------------------------------------------------------

// ContractState is one tokens row plus the chain-consistency triple.
type ContractState struct {
	TokenID         string
	TokenType       string
	Status          int
	StatusName      string
	LatestPosition  int64
	LatestRole      int
	LatestRoleName  string
	HeadTx          string // tokens.transaction_id
	LockReferenceID string
	OwnerDID        string
	UpdatedAt       time.Time
	// Consistency: all three should equal LatestPosition+1.
	TokenchainRows int64
	IndexLen       int64
	JoinedRows     int64
	MaxPosition    int64
}

// Locked reports token_status = 1.
func (c ContractState) Locked() bool { return c.Status == 1 }

// StaleLockRef reports a lock reference left behind on a non-locked token
// (residue of the failed-transaction release path; harmless).
func (c ContractState) StaleLockRef() bool { return c.Status != 1 && c.LockReferenceID != "" }

// Consistent reports tokenchain rows = index length = joined rows =
// latest_position + 1.
func (c ContractState) Consistent() bool {
	want := c.LatestPosition + 1
	return c.TokenchainRows == want && c.IndexLen == want && c.JoinedRows == want && c.MaxPosition == c.LatestPosition
}

// ContractStates returns the tokens row for each id, keyed by token id.
// Ids missing from the node are absent from the map.
func ContractStates(ctx context.Context, q Querier, tokenIDs []string) (map[string]ContractState, error) {
	rows, err := q.Query(ctx, `
		SELECT t.token_id, COALESCE(tt.name, ''), t.token_status, t.latest_position,
		       COALESCE(t.latest_role, 0), COALESCE(tr.name, ''), t.transaction_id,
		       COALESCE(t.lock_reference_id, ''), t.did, COALESCE(t.updated_at, now()),
		       (SELECT count(*) FROM tokenchain tc WHERE tc.token_id = t.token_id),
		       COALESCE((SELECT array_length(index, 1) FROM tokenchain_index i WHERE i.token_id = t.token_id), 0),
		       (SELECT count(*) FROM tokenchain tc JOIN transactions x ON x.id = tc.transaction_id WHERE tc.token_id = t.token_id),
		       COALESCE((SELECT max(position) FROM tokenchain tc WHERE tc.token_id = t.token_id), -1)
		FROM tokens t
		LEFT JOIN token_type tt ON tt.id = t.token_type
		LEFT JOIN token_role tr ON tr.id = t.latest_role
		WHERE t.token_id = ANY($1::text[])`, tokenIDs)
	if err != nil {
		return nil, fmt.Errorf("nodedb.ContractStates: %w", err)
	}
	defer rows.Close()
	out := make(map[string]ContractState, len(tokenIDs))
	for rows.Next() {
		var c ContractState
		if err := rows.Scan(&c.TokenID, &c.TokenType, &c.Status, &c.LatestPosition, &c.LatestRole, &c.LatestRoleName,
			&c.HeadTx, &c.LockReferenceID, &c.OwnerDID, &c.UpdatedAt,
			&c.TokenchainRows, &c.IndexLen, &c.JoinedRows, &c.MaxPosition); err != nil {
			return nil, fmt.Errorf("nodedb.ContractStates scan: %w", err)
		}
		c.StatusName = StatusName(c.Status)
		out[c.TokenID] = c
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Chain tail
// ---------------------------------------------------------------------------

// ChainRow is one tokenchain row.
type ChainRow struct {
	Position      int64
	TransactionID string
	PreviousTx    string
	Role          int
	RoleName      string
	CreatedAt     time.Time
	// HasTransaction is false for an orphaned tokenchain row whose
	// transactions row is missing.
	HasTransaction bool
}

// ChainTail returns the newest limit tokenchain rows for a token,
// highest position first.
func ChainTail(ctx context.Context, q Querier, tokenID string, limit int) ([]ChainRow, error) {
	rows, err := q.Query(ctx, `
		SELECT tc.position, tc.transaction_id, COALESCE(tc.previous_transaction_id, ''), tc.role,
		       COALESCE(tr.name, ''), COALESCE(tc.created_at, now()), (x.id IS NOT NULL)
		FROM tokenchain tc
		LEFT JOIN token_role tr ON tr.id = tc.role
		LEFT JOIN transactions x ON x.id = tc.transaction_id
		WHERE tc.token_id = $1
		ORDER BY tc.position DESC
		LIMIT $2`, tokenID, limit)
	if err != nil {
		return nil, fmt.Errorf("nodedb.ChainTail: %w", err)
	}
	defer rows.Close()
	var out []ChainRow
	for rows.Next() {
		var r ChainRow
		if err := rows.Scan(&r.Position, &r.TransactionID, &r.PreviousTx, &r.Role, &r.RoleName, &r.CreatedAt, &r.HasTransaction); err != nil {
			return nil, fmt.Errorf("nodedb.ChainTail scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// One transaction
// ---------------------------------------------------------------------------

// TxUnit is one transaction_units row.
type TxUnit struct {
	DID           string
	ExecutionRole string
	Status        string
	CreatedAt     time.Time
}

// TxChainRef is one tokenchain row that references the transaction.
type TxChainRef struct {
	TokenID  string
	Position int64
	Role     int
}

// Transaction is a transactions row with its units and chain references.
type Transaction struct {
	ID        string
	Info      json.RawMessage
	Signature json.RawMessage
	CreatedAt time.Time
	// InfoHash is SHA3-256 of the stored info text; a valid block has
	// InfoHash == ID (check-orphans.py did the same check).
	InfoHash  string
	Units     []TxUnit
	ChainRefs []TxChainRef
}

// GetTransaction loads one transaction by id, or ErrNotFound.
func GetTransaction(ctx context.Context, q Querier, txID string) (*Transaction, error) {
	var t Transaction
	var info, sig string
	err := q.QueryRow(ctx,
		`SELECT id, info::text, signature::text, COALESCE(created_at, now()) FROM transactions WHERE id = $1`, txID,
	).Scan(&t.ID, &info, &sig, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("nodedb.GetTransaction: %w", err)
	}
	t.Info = json.RawMessage(info)
	t.Signature = json.RawMessage(sig)
	sum := sha3.Sum256([]byte(info))
	t.InfoHash = hex.EncodeToString(sum[:])

	rows, err := q.Query(ctx,
		`SELECT did, execution_role, status, COALESCE(created_at, now()) FROM transaction_units WHERE transaction_id = $1 ORDER BY execution_role, did`, txID)
	if err != nil {
		return nil, fmt.Errorf("nodedb.GetTransaction units: %w", err)
	}
	for rows.Next() {
		var u TxUnit
		if err := rows.Scan(&u.DID, &u.ExecutionRole, &u.Status, &u.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("nodedb.GetTransaction units scan: %w", err)
		}
		t.Units = append(t.Units, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `SELECT token_id, position, role FROM tokenchain WHERE transaction_id = $1 ORDER BY token_id`, txID)
	if err != nil {
		return nil, fmt.Errorf("nodedb.GetTransaction chain refs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r TxChainRef
		if err := rows.Scan(&r.TokenID, &r.Position, &r.Role); err != nil {
			return nil, fmt.Errorf("nodedb.GetTransaction chain refs scan: %w", err)
		}
		t.ChainRefs = append(t.ChainRefs, r)
	}
	return &t, rows.Err()
}

// ---------------------------------------------------------------------------
// Locks
// ---------------------------------------------------------------------------

// LockRow is a token with token_status = 1.
type LockRow struct {
	TokenID         string
	TokenType       string
	OwnerDID        string
	LockReferenceID string
	LatestPosition  int64
	UpdatedAt       time.Time
}

// LockedTokens returns every token currently locked on the node, oldest
// lock first.
func LockedTokens(ctx context.Context, q Querier) ([]LockRow, error) {
	rows, err := q.Query(ctx, `
		SELECT t.token_id, COALESCE(tt.name, ''), t.did, COALESCE(t.lock_reference_id, ''), t.latest_position, COALESCE(t.updated_at, now())
		FROM tokens t LEFT JOIN token_type tt ON tt.id = t.token_type
		WHERE t.token_status = 1
		ORDER BY t.updated_at`)
	if err != nil {
		return nil, fmt.Errorf("nodedb.LockedTokens: %w", err)
	}
	defer rows.Close()
	var out []LockRow
	for rows.Next() {
		var r LockRow
		if err := rows.Scan(&r.TokenID, &r.TokenType, &r.OwnerDID, &r.LockReferenceID, &r.LatestPosition, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("nodedb.LockedTokens scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Token inventory
// ---------------------------------------------------------------------------

// TokenBucket is a count of a DID's tokens by type and status.
type TokenBucket struct {
	TokenType  string
	Status     int
	StatusName string
	Count      int64
	Value      string // sum(token_value) as text
}

// DenomRow is one token_denom row.
type DenomRow struct {
	Denom string
	Count int64
}

// UnpledgeRow is one unpledge_sequence_info row (pledges awaiting release).
type UnpledgeRow struct {
	TxID         string
	QuorumDID    string
	PledgeTokens []string
	Epoch        int64
	CreatedAt    time.Time
}

// Inventory is a DID's token buckets, denominations and the node's
// pending unpledges.
type Inventory struct {
	Buckets         []TokenBucket
	Denoms          []DenomRow
	PendingUnpledge []UnpledgeRow
	PendingCount    int64
}

// TokenInventory summarises the tokens a DID holds on this node.
func TokenInventory(ctx context.Context, q Querier, did string, unpledgeLimit int) (*Inventory, error) {
	inv := &Inventory{}
	rows, err := q.Query(ctx, `
		SELECT COALESCE(tt.name, ''), t.token_status, count(*), COALESCE(sum(t.token_value), 0)::text
		FROM tokens t LEFT JOIN token_type tt ON tt.id = t.token_type
		WHERE t.did = $1
		GROUP BY 1, 2 ORDER BY 1, 2`, did)
	if err != nil {
		return nil, fmt.Errorf("nodedb.TokenInventory: %w", err)
	}
	for rows.Next() {
		var b TokenBucket
		if err := rows.Scan(&b.TokenType, &b.Status, &b.Count, &b.Value); err != nil {
			rows.Close()
			return nil, fmt.Errorf("nodedb.TokenInventory scan: %w", err)
		}
		b.StatusName = StatusName(b.Status)
		inv.Buckets = append(inv.Buckets, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `SELECT denom::text, count FROM token_denom WHERE did = $1 ORDER BY denom`, did)
	if err != nil {
		return nil, fmt.Errorf("nodedb.TokenInventory denom: %w", err)
	}
	for rows.Next() {
		var d DenomRow
		if err := rows.Scan(&d.Denom, &d.Count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("nodedb.TokenInventory denom scan: %w", err)
		}
		inv.Denoms = append(inv.Denoms, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := q.QueryRow(ctx, `SELECT count(*) FROM unpledge_sequence_info`).Scan(&inv.PendingCount); err != nil {
		return nil, fmt.Errorf("nodedb.TokenInventory unpledge count: %w", err)
	}
	rows, err = q.Query(ctx, `
		SELECT tx_id, COALESCE(quorum_did, ''), COALESCE(pledge_tokens, '{}'::text[]), COALESCE(epoch, 0), COALESCE(created_at, now())
		FROM unpledge_sequence_info ORDER BY created_at LIMIT $1`, unpledgeLimit)
	if err != nil {
		return nil, fmt.Errorf("nodedb.TokenInventory unpledge: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u UnpledgeRow
		if err := rows.Scan(&u.TxID, &u.QuorumDID, &u.PledgeTokens, &u.Epoch, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("nodedb.TokenInventory unpledge scan: %w", err)
		}
		inv.PendingUnpledge = append(inv.PendingUnpledge, u)
	}
	return inv, rows.Err()
}

// ---------------------------------------------------------------------------
// Quorum config
// ---------------------------------------------------------------------------

// QuorumRow is one quorum_manager row.
type QuorumRow struct {
	DID       string
	CreatedAt time.Time
}

// QuorumMembers returns the node's configured quorum DIDs.
func QuorumMembers(ctx context.Context, q Querier) ([]QuorumRow, error) {
	rows, err := q.Query(ctx, `SELECT did, COALESCE(created_at, now()) FROM quorum_manager ORDER BY created_at, did`)
	if err != nil {
		return nil, fmt.Errorf("nodedb.QuorumMembers: %w", err)
	}
	defer rows.Close()
	var out []QuorumRow
	for rows.Next() {
		var r QuorumRow
		if err := rows.Scan(&r.DID, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("nodedb.QuorumMembers scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
