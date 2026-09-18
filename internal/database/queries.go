package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned by selectors when no row matched.
var ErrNotFound = errors.New("database: not found")

// ---------------------------------------------------------------------------
// transfer_status
// ---------------------------------------------------------------------------

// CreateTransferStatus inserts a new ledger row. The caller sets RequestID,
// Kind, and any known context fields. Status defaults to queued.
func CreateTransferStatus(ctx context.Context, s *TransferStatus) error {
	if s.Status == "" {
		s.Status = StatusQueued
	}
	s.CreatedAt = time.Now()
	s.UpdatedAt = s.CreatedAt
	_, err := Pool.Exec(ctx, `
		INSERT INTO transfer_status (
			request_id, transaction_id, kind, admin_did, user_did,
			activity_ids, reward_points, contract_hash, status, message,
			error_details, created_at, updated_at
		) VALUES (
			$1, NULLIF($2, ''), $3, NULLIF($4, ''), NULLIF($5, ''),
			$6, $7, NULLIF($8, ''), $9, $10,
			$11, $12, $13
		)
	`,
		s.RequestID, s.TransactionID, s.Kind, s.AdminDID, s.UserDID,
		s.ActivityIDs, s.RewardPoints, s.ContractHash, s.Status, s.Message,
		s.ErrorDetails, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("CreateTransferStatus: %w", err)
	}
	return nil
}

// UpdateTransferStatus applies a partial update keyed by request_id.
// Allowed keys: status, message, error_details, transaction_id, contract_hash, reward_points.
func UpdateTransferStatus(ctx context.Context, requestID string, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	allowed := map[string]bool{
		"status":         true,
		"message":        true,
		"error_details":  true,
		"transaction_id": true,
		"contract_hash":  true,
		"reward_points":  true,
	}
	sets := make([]string, 0, len(updates)+1)
	args := make([]any, 0, len(updates)+2)
	i := 1
	for k, v := range updates {
		if !allowed[k] {
			return fmt.Errorf("UpdateTransferStatus: unsupported field %q", k)
		}
		sets = append(sets, fmt.Sprintf("%s = $%d", k, i))
		args = append(args, v)
		i++
	}
	sets = append(sets, fmt.Sprintf("updated_at = $%d", i))
	args = append(args, time.Now())
	i++
	args = append(args, requestID)

	q := fmt.Sprintf("UPDATE transfer_status SET %s WHERE request_id = $%d", strings.Join(sets, ", "), i)
	tag, err := Pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("UpdateTransferStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetTransferStatusByRequestID fetches a row by request_id.
func GetTransferStatusByRequestID(ctx context.Context, requestID string) (*TransferStatus, error) {
	return selectTransferStatus(ctx, "request_id = $1", requestID)
}

// GetTransferStatusByTransactionID fetches a row by transaction_id.
func GetTransferStatusByTransactionID(ctx context.Context, txID string) (*TransferStatus, error) {
	return selectTransferStatus(ctx, "transaction_id = $1", txID)
}

// ListSuccessfulRewardsByUserDID returns successful reward transfers
// where user_did matches, newest first.
func ListSuccessfulRewardsByUserDID(ctx context.Context, userDID string) ([]TransferStatus, error) {
	return listTransferStatus(ctx,
		"user_did = $1 AND kind = $2 AND status = $3 ORDER BY created_at DESC",
		userDID, KindReward, StatusSuccess,
	)
}

// ListSuccessfulRewardsByAdminDID returns successful reward transfers
// where admin_did matches, newest first.
func ListSuccessfulRewardsByAdminDID(ctx context.Context, adminDID string) ([]TransferStatus, error) {
	return listTransferStatus(ctx,
		"admin_did = $1 AND kind = $2 AND status = $3 ORDER BY created_at DESC",
		adminDID, KindReward, StatusSuccess,
	)
}

func listTransferStatus(ctx context.Context, where string, args ...any) ([]TransferStatus, error) {
	q := `
		SELECT request_id, COALESCE(transaction_id,''), kind,
		       COALESCE(admin_did,''), COALESCE(user_did,''),
		       COALESCE(activity_ids,'{}'::text[]),
		       COALESCE(reward_points,0), COALESCE(contract_hash,''),
		       status, COALESCE(message,''), COALESCE(error_details,''),
		       created_at, updated_at
		FROM transfer_status
		WHERE ` + where
	rows, err := Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listTransferStatus: %w", err)
	}
	defer rows.Close()

	var out []TransferStatus
	for rows.Next() {
		var s TransferStatus
		if err := rows.Scan(
			&s.RequestID, &s.TransactionID, &s.Kind,
			&s.AdminDID, &s.UserDID,
			&s.ActivityIDs,
			&s.RewardPoints, &s.ContractHash,
			&s.Status, &s.Message, &s.ErrorDetails,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("listTransferStatus scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func selectTransferStatus(ctx context.Context, where string, args ...any) (*TransferStatus, error) {
	q := `
		SELECT request_id, COALESCE(transaction_id,''), kind,
		       COALESCE(admin_did,''), COALESCE(user_did,''),
		       COALESCE(activity_ids,'{}'::text[]),
		       COALESCE(reward_points,0), COALESCE(contract_hash,''),
		       status, COALESCE(message,''), COALESCE(error_details,''),
		       created_at, updated_at
		FROM transfer_status
		WHERE ` + where
	var s TransferStatus
	err := Pool.QueryRow(ctx, q, args...).Scan(
		&s.RequestID, &s.TransactionID, &s.Kind,
		&s.AdminDID, &s.UserDID,
		&s.ActivityIDs,
		&s.RewardPoints, &s.ContractHash,
		&s.Status, &s.Message, &s.ErrorDetails,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("selectTransferStatus: %w", err)
	}
	return &s, nil
}

// ---------------------------------------------------------------------------
// user_admins
// ---------------------------------------------------------------------------

// UpsertUserAdmin records (user_did -> admin_did). Idempotent: a repeat
// call with the same user_did is a no-op (first admin wins).
func UpsertUserAdmin(ctx context.Context, userDID, adminDID string) error {
	_, err := Pool.Exec(ctx, `
		INSERT INTO user_admins (user_did, admin_did)
		VALUES ($1, $2)
		ON CONFLICT (user_did) DO NOTHING
	`, userDID, adminDID)
	if err != nil {
		return fmt.Errorf("UpsertUserAdmin: %w", err)
	}
	return nil
}

// CountUsersForAdmin returns the number of users mapped to admin_did.
// Returns 0 with no error if admin has no users.
func CountUsersForAdmin(ctx context.Context, adminDID string) (int, error) {
	var count int
	err := Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM user_admins WHERE admin_did = $1", adminDID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountUsersForAdmin: %w", err)
	}
	return count, nil
}

// GetAdminForUser returns the admin_did mapped to user_did, or
// ErrNotFound if no mapping exists.
func GetAdminForUser(ctx context.Context, userDID string) (string, error) {
	var adminDID string
	err := Pool.QueryRow(ctx,
		"SELECT admin_did FROM user_admins WHERE user_did = $1", userDID,
	).Scan(&adminDID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("GetAdminForUser: %w", err)
	}
	return adminDID, nil
}

// ---------------------------------------------------------------------------
// admin_contracts
// ---------------------------------------------------------------------------

// UpsertAdminContract records or updates a deployed contract hash for an
// (admin_did, contract_kind) pair.
func UpsertAdminContract(ctx context.Context, adminDID, kind, hash string) error {
	_, err := Pool.Exec(ctx, `
		INSERT INTO admin_contracts (admin_did, contract_kind, contract_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (admin_did, contract_kind)
		DO UPDATE SET contract_hash = EXCLUDED.contract_hash, deployed_at = NOW()
	`, adminDID, kind, hash)
	if err != nil {
		return fmt.Errorf("UpsertAdminContract: %w", err)
	}
	return nil
}

// GetAdminContract returns the hash for an (admin_did, kind) pair.
func GetAdminContract(ctx context.Context, adminDID, kind string) (string, error) {
	var hash string
	err := Pool.QueryRow(ctx,
		`SELECT contract_hash FROM admin_contracts
		 WHERE admin_did = $1 AND contract_kind = $2`,
		adminDID, kind,
	).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("GetAdminContract: %w", err)
	}
	return hash, nil
}

// GetAdminDIDByContract returns the admin_did that owns a contract hash,
// used to route contract-chain queries to the right node.
func GetAdminDIDByContract(ctx context.Context, hash string) (string, error) {
	var did string
	err := Pool.QueryRow(ctx,
		`SELECT admin_did FROM admin_contracts WHERE contract_hash = $1`,
		hash,
	).Scan(&did)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("GetAdminDIDByContract: %w", err)
	}
	return did, nil
}

// ---------------------------------------------------------------------------
// activities
// ---------------------------------------------------------------------------

// CreateActivity inserts a new activity record for an admin. On
// conflict (re-adding an existing activity_id), reward_points,
// description, and transaction_id are all updated.
func CreateActivity(ctx context.Context, a *Activity) error {
	a.CreatedAt = time.Now()
	_, err := Pool.Exec(ctx, `
		INSERT INTO activities (admin_did, activity_id, reward_points, description, transaction_id, created_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)
		ON CONFLICT (admin_did, activity_id)
		DO UPDATE SET reward_points  = EXCLUDED.reward_points,
		              description    = EXCLUDED.description,
		              transaction_id = EXCLUDED.transaction_id
	`, a.AdminDID, a.ActivityID, a.RewardPoints, a.Description, a.TransactionID, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("CreateActivity: %w", err)
	}
	return nil
}

// GetActivity returns the activity for an (admin_did, activity_id) pair.
func GetActivity(ctx context.Context, adminDID, activityID string) (*Activity, error) {
	var a Activity
	err := Pool.QueryRow(ctx, `
		SELECT admin_did, activity_id, reward_points,
		       COALESCE(description,''), COALESCE(transaction_id,''), created_at
		FROM activities WHERE admin_did = $1 AND activity_id = $2
	`, adminDID, activityID).Scan(
		&a.AdminDID, &a.ActivityID, &a.RewardPoints, &a.Description, &a.TransactionID, &a.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("GetActivity: %w", err)
	}
	return &a, nil
}

// ListActivities returns all activities, optionally filtered by admin.
// If adminDID is empty, returns activities across every admin.
func ListActivities(ctx context.Context, adminDID string) ([]Activity, error) {
	var rows pgx.Rows
	var err error
	if adminDID == "" {
		rows, err = Pool.Query(ctx,
			`SELECT admin_did, activity_id, reward_points,
			        COALESCE(description,''), COALESCE(transaction_id,''), created_at
			 FROM activities ORDER BY created_at ASC`)
	} else {
		rows, err = Pool.Query(ctx,
			`SELECT admin_did, activity_id, reward_points,
			        COALESCE(description,''), COALESCE(transaction_id,''), created_at
			 FROM activities WHERE admin_did = $1 ORDER BY created_at ASC`, adminDID)
	}
	if err != nil {
		return nil, fmt.Errorf("ListActivities: %w", err)
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.AdminDID, &a.ActivityID, &a.RewardPoints,
			&a.Description, &a.TransactionID, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("ListActivities scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// FTInfoEntry is one row in /users/{user_did}/payouts: total reward
// points received from a single creator (admin) DID.
type FTInfoEntry struct {
	CreatorDID string
	TotalCount int
}

// FTInfoForUser returns aggregate reward totals received by user_did,
// grouped by the admin who sent them. Only counts successful reward
// transfers.
func FTInfoForUser(ctx context.Context, userDID string) ([]FTInfoEntry, error) {
	rows, err := Pool.Query(ctx, `
		SELECT admin_did, COALESCE(SUM(reward_points), 0)
		FROM transfer_status
		WHERE user_did = $1
		  AND kind = $2
		  AND status = $3
		GROUP BY admin_did
		ORDER BY admin_did
	`, userDID, KindReward, StatusSuccess)
	if err != nil {
		return nil, fmt.Errorf("FTInfoForUser: %w", err)
	}
	defer rows.Close()
	var out []FTInfoEntry
	for rows.Next() {
		var e FTInfoEntry
		if err := rows.Scan(&e.CreatorDID, &e.TotalCount); err != nil {
			return nil, fmt.Errorf("FTInfoForUser scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SumRewardPointsForActivities returns the sum of reward_points across the
// given activity ids for an admin. Missing activities contribute 0.
func SumRewardPointsForActivities(ctx context.Context, adminDID string, activityIDs []string) (int, error) {
	if len(activityIDs) == 0 {
		return 0, nil
	}
	var sum int
	err := Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(reward_points), 0)
		FROM activities
		WHERE admin_did = $1 AND activity_id = ANY($2)
	`, adminDID, activityIDs).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("SumRewardPointsForActivities: %w", err)
	}
	return sum, nil
}

// ---------------------------------------------------------------------------
// Operator diagnostics (GET /api/debug/*). Read-only. These are the psql
// queries used during docs/INCIDENT-2026-09-04-chain-fork.md, turned into
// helpers so the next incident does not need a shell on the VM.
// ---------------------------------------------------------------------------

// mismatchNeedle is the substring in error_details that identifies a
// quorum/owner chain fork (TokenChainIntigrityCheck on the quorum).
const mismatchNeedle = "%chain mismatch%"

// PayoutSummaryRow is one admin's reward-transfer counts in a window.
type PayoutSummaryRow struct {
	AdminDID        string
	NodePort        string
	Total           int
	Success         int
	Failed          int
	Mismatch        int // failed rows whose error_details mention "chain mismatch"
	InFlight        int // queued + processing
	FirstMismatchAt *time.Time
	LastMismatchAt  *time.Time
	LastSuccessAt   *time.Time
	LastSuccessTx   string
}

// PayoutSummary aggregates reward transfers created at or after since,
// per admin, joined with admins for node_port. adminDID narrows to one
// admin when non-empty. Admins with no rows in the window are omitted.
func PayoutSummary(ctx context.Context, since time.Time, adminDID string) ([]PayoutSummaryRow, error) {
	q := `
		SELECT ts.admin_did, COALESCE(a.node_port, ''),
		       count(*),
		       count(*) FILTER (WHERE ts.status = 'success'),
		       count(*) FILTER (WHERE ts.status = 'failed'),
		       count(*) FILTER (WHERE ts.status = 'failed' AND ts.error_details LIKE $2),
		       count(*) FILTER (WHERE ts.status IN ('queued', 'processing')),
		       min(ts.created_at) FILTER (WHERE ts.status = 'failed' AND ts.error_details LIKE $2),
		       max(ts.created_at) FILTER (WHERE ts.status = 'failed' AND ts.error_details LIKE $2),
		       max(ts.created_at) FILTER (WHERE ts.status = 'success'),
		       COALESCE((SELECT t2.transaction_id FROM transfer_status t2
		                 WHERE t2.admin_did = ts.admin_did AND t2.kind = 'reward' AND t2.status = 'success'
		                 ORDER BY t2.created_at DESC LIMIT 1), '')
		FROM transfer_status ts
		LEFT JOIN admins a ON a.did = ts.admin_did
		WHERE ts.kind = 'reward' AND ts.created_at >= $1
		  AND ($3 = '' OR ts.admin_did = $3)
		GROUP BY ts.admin_did, a.node_port
		ORDER BY a.node_port NULLS LAST, ts.admin_did`
	rows, err := Pool.Query(ctx, q, since, mismatchNeedle, adminDID)
	if err != nil {
		return nil, fmt.Errorf("PayoutSummary: %w", err)
	}
	defer rows.Close()
	var out []PayoutSummaryRow
	for rows.Next() {
		var r PayoutSummaryRow
		if err := rows.Scan(&r.AdminDID, &r.NodePort, &r.Total, &r.Success, &r.Failed, &r.Mismatch,
			&r.InFlight, &r.FirstMismatchAt, &r.LastMismatchAt, &r.LastSuccessAt, &r.LastSuccessTx); err != nil {
			return nil, fmt.Errorf("PayoutSummary scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FailureGroup is one normalised failure reason and how often it occurred.
type FailureGroup struct {
	Count    int
	Reason   string // error_details with hashes/DIDs/ports replaced by '#'
	FirstAt  time.Time
	LatestAt time.Time
}

// failureReasonExpr normalises error_details before grouping: 64-hex
// transaction/contract ids, bafybmi… DIDs and 127.0.0.1:<port> become '#'.
// Same expression as scripts/incident-2026-09-04/morning-check.sh.
const failureReasonExpr = `left(regexp_replace(error_details, '[0-9a-f]{64}|bafybmi[a-z0-9]+|127\.0\.0\.1:[0-9]+', '#', 'g'), 200)`

// PayoutFailures groups failed reward transfers created at or after
// since by normalised reason, most frequent first. adminDID narrows to
// one admin when non-empty.
func PayoutFailures(ctx context.Context, since time.Time, adminDID string, limit int) ([]FailureGroup, error) {
	q := `
		SELECT count(*), ` + failureReasonExpr + ` AS reason, min(created_at), max(created_at)
		FROM transfer_status
		WHERE kind = 'reward' AND status = 'failed' AND created_at >= $1
		  AND ($2 = '' OR admin_did = $2)
		GROUP BY reason
		ORDER BY count(*) DESC, max(created_at) DESC
		LIMIT $3`
	rows, err := Pool.Query(ctx, q, since, adminDID, limit)
	if err != nil {
		return nil, fmt.Errorf("PayoutFailures: %w", err)
	}
	defer rows.Close()
	var out []FailureGroup
	for rows.Next() {
		var g FailureGroup
		if err := rows.Scan(&g.Count, &g.Reason, &g.FirstAt, &g.LatestAt); err != nil {
			return nil, fmt.Errorf("PayoutFailures scan: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// PayoutHistory returns one admin's reward transfers, newest first, with
// every column. status filters when non-empty; since filters when
// non-zero.
func PayoutHistory(ctx context.Context, adminDID, status string, since time.Time, limit int) ([]TransferStatus, error) {
	where := "admin_did = $1 AND kind = $2"
	args := []any{adminDID, KindReward}
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if !since.IsZero() {
		args = append(args, since)
		where += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	args = append(args, limit)
	where += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))
	return listTransferStatus(ctx, where, args...)
}

// AdminContractRow is admin_contracts joined with admins.node_port.
type AdminContractRow struct {
	AdminDID     string
	NodePort     string
	ContractKind string
	ContractHash string
	DeployedAt   time.Time
}

// ListAdminContracts returns every registered contract with its owner's
// node port, ordered by node port then kind. adminDID narrows to one
// admin when non-empty.
func ListAdminContracts(ctx context.Context, adminDID string) ([]AdminContractRow, error) {
	rows, err := Pool.Query(ctx, `
		SELECT ac.admin_did, COALESCE(a.node_port, ''), ac.contract_kind, ac.contract_hash, ac.deployed_at
		FROM admin_contracts ac
		LEFT JOIN admins a ON a.did = ac.admin_did
		WHERE ($1 = '' OR ac.admin_did = $1)
		ORDER BY a.node_port NULLS LAST, ac.admin_did, ac.contract_kind`, adminDID)
	if err != nil {
		return nil, fmt.Errorf("ListAdminContracts: %w", err)
	}
	defer rows.Close()
	var out []AdminContractRow
	for rows.Next() {
		var r AdminContractRow
		if err := rows.Scan(&r.AdminDID, &r.NodePort, &r.ContractKind, &r.ContractHash, &r.DeployedAt); err != nil {
			return nil, fmt.Errorf("ListAdminContracts scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestRewardMismatch returns the newest failed reward transfer for the
// admin whose error_details mention "chain mismatch", or ErrNotFound.
func LatestRewardMismatch(ctx context.Context, adminDID string) (*TransferStatus, error) {
	return selectTransferStatus(ctx,
		"admin_did = $1 AND kind = $2 AND status = $3 AND error_details LIKE $4 ORDER BY created_at DESC LIMIT 1",
		adminDID, KindReward, StatusFailed, mismatchNeedle)
}

// LatestRewardSuccess returns the admin's newest successful reward
// transfer, or ErrNotFound.
func LatestRewardSuccess(ctx context.Context, adminDID string) (*TransferStatus, error) {
	return selectTransferStatus(ctx,
		"admin_did = $1 AND kind = $2 AND status = $3 ORDER BY created_at DESC LIMIT 1",
		adminDID, KindReward, StatusSuccess)
}

// FirstRewardMismatchAfter returns the created_at of the admin's oldest
// "chain mismatch" failure created after t (all of them when t is zero),
// or nil when there is none.
func FirstRewardMismatchAfter(ctx context.Context, adminDID string, t time.Time) (*time.Time, error) {
	var first *time.Time
	err := Pool.QueryRow(ctx, `
		SELECT min(created_at) FROM transfer_status
		WHERE admin_did = $1 AND kind = $2 AND status = $3 AND error_details LIKE $4 AND created_at > $5`,
		adminDID, KindReward, StatusFailed, mismatchNeedle, t).Scan(&first)
	if err != nil {
		return nil, fmt.Errorf("FirstRewardMismatchAfter: %w", err)
	}
	return first, nil
}
