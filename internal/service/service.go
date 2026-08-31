// Package service contains the dApp business logic. It sits between the
// HTTP handlers / queue workers and the Rubix client + database layers.
//
// Everything in this package is synchronous from the caller's perspective.
// Async flows (reward transfer) are orchestrated by the queue package,
// which invokes Service methods on worker goroutines.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"ymca-wellness-dapp/internal/config"
	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/rubix"
)

// Service is the dApp's application layer. Callers obtain a pool-backed
// Rubix client via Service.ClientFor(adminDID) and use the high-level
// methods below for common flows.
type Service struct {
	Cfg  *config.AppConfig
	Pool *rubix.Pool
}

// New builds a Service. The Rubix client pool is lazily populated per
// admin base URL.
func New(cfg *config.AppConfig) *Service {
	timeout := time.Duration(cfg.Env.RubixHTTPTimeoutSecond) * time.Second
	return &Service{
		Cfg:  cfg,
		Pool: rubix.NewPool(timeout),
	}
}

// ClientFor returns the Rubix client for the given admin DID, or an error
// if the admin is unknown / has no node config.
func (s *Service) ClientFor(adminDID string) (*rubix.Client, config.AdminConfig, error) {
	a, ok := s.Cfg.AdminByDID(adminDID)
	if !ok {
		return nil, config.AdminConfig{}, fmt.Errorf("unknown admin_did %q", adminDID)
	}
	c, err := s.Pool.ForBaseURL(a.BaseURL())
	if err != nil {
		return nil, a, fmt.Errorf("rubix client for %s: %w", adminDID, err)
	}
	return c, a, nil
}

// ---------------------------------------------------------------------------
// DID / user registration
// ---------------------------------------------------------------------------

// CreateDIDWithPubKey provisions a DID on the admin's node using the
// supplied public key. Password defaults to the admin's password (the
// on-disk key material is held on the admin's node).
func (s *Service) CreateDIDWithPubKey(ctx context.Context, adminDID, pubKey string) (string, error) {
	if pubKey == "" {
		return "", fmt.Errorf("public_key is required")
	}
	c, a, err := s.ClientFor(adminDID)
	if err != nil {
		return "", err
	}
	res, err := c.CreateDID(ctx, rubix.CreateDIDRequest{
		Password:  a.Password,
		PublicKey: pubKey,
	})
	if err != nil {
		return "", fmt.Errorf("create DID: %w", err)
	}
	if res == nil || res.DID == "" {
		return "", fmt.Errorf("create DID: node returned empty did")
	}

	// Best-effort: record the (user_did -> admin_did) mapping. A DB
	// failure here does not fail the request; the DID is already minted
	// on Rubix and the caller has it. They can retry later or query
	// transfer_status to recover the relationship.
	if err := database.UpsertUserAdmin(ctx, res.DID, adminDID); err != nil {
		log.Printf("create-did-with-pubkey: user_admins upsert failed for user=%s admin=%s: %v",
			res.DID, adminDID, err)
	}

	return res.DID, nil
}

// ---------------------------------------------------------------------------
// Contracts
// ---------------------------------------------------------------------------

// DeployContract generates and signs a smart contract for the given admin,
// recording the resulting contract token id under (admin_did, kind).
//
// kind is the caller-chosen label (e.g. "reward", "add_activity",
// "add_admin") stored in admin_contracts so the same admin can have
// multiple independent contracts. wasmPath/rsPath must exist on disk of
// the process running this server (they are uploaded to the node as
// multipart files).
func (s *Service) DeployContract(ctx context.Context, adminDID, kind, wasmPath, rsPath string) (string, error) {
	if kind == "" {
		return "", fmt.Errorf("kind is required")
	}
	c, a, err := s.ClientFor(adminDID)
	if err != nil {
		return "", err
	}

	reqID, err := c.GenerateSmartContract(ctx, adminDID, wasmPath, rsPath)
	if err != nil {
		return "", fmt.Errorf("generate smart contract: %w", err)
	}
	sr, err := c.Sign(ctx, reqID, a.Password)
	if err != nil {
		return "", fmt.Errorf("sign generate: %w", err)
	}
	token, err := rubix.ContractTokenFromSignResult(sr)
	if err != nil {
		return "", fmt.Errorf("extract contract token: %w", err)
	}

	// Subscribe so this node receives chain updates for the contract.
	if err := c.SubscribeSmartContract(ctx, token); err != nil {
		// Non-fatal: chain queries can still work by direct token id;
		// log-only in higher layers. Returning error here would cause
		// spurious deploy failures on networks that haven't fully
		// propagated yet.
		_ = err
	}

	if err := database.UpsertAdminContract(ctx, adminDID, kind, token); err != nil {
		return "", fmt.Errorf("persist admin contract: %w", err)
	}
	return token, nil
}

// ExecuteContract runs an already-deployed contract by submitting a
// unified transaction with a SmartContract leg. input is passed verbatim
// as the SC leg's Data field (opaque to the node).
func (s *Service) ExecuteContract(ctx context.Context, adminDID, contractHash, input string) (*rubix.SignResult, error) {
	c, a, err := s.ClientFor(adminDID)
	if err != nil {
		return nil, err
	}
	reqID, err := c.PostTx(ctx, rubix.TransactionRequest{
		Initiator: adminDID,
		Owner:     adminDID,
		Tokens: rubix.TransactionTokenDetails{
			SmartContract: []rubix.SmartContractInfo{{
				SmartContractId: contractHash,
				Value:           0,
				Data:            input,
			}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("post tx: %w", err)
	}
	sr, err := c.Sign(ctx, reqID, a.Password)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return sr, nil
}

// GetContractChain returns the chain history for the contract registered
// under (admin_did, kind). The query is routed to that admin's node.
func (s *Service) GetContractChain(ctx context.Context, adminDID, kind string) (string, []rubix.ChainEntry, error) {
	hash, err := database.GetAdminContract(ctx, adminDID, kind)
	if err != nil {
		return "", nil, fmt.Errorf("lookup contract: %w", err)
	}
	c, _, err := s.ClientFor(adminDID)
	if err != nil {
		return hash, nil, err
	}
	chain, err := c.GetSmartContractChain(ctx, hash)
	if err != nil {
		return hash, nil, fmt.Errorf("get chain: %w", err)
	}
	return hash, chain, nil
}

// GetContractChainByHash is like GetContractChain but keyed directly by
// contract hash; the admin is resolved from admin_contracts.
func (s *Service) GetContractChainByHash(ctx context.Context, contractHash string) (string, []rubix.ChainEntry, error) {
	adminDID, err := database.GetAdminDIDByContract(ctx, contractHash)
	if err != nil {
		return "", nil, fmt.Errorf("resolve admin for contract %s: %w", contractHash, err)
	}
	c, _, err := s.ClientFor(adminDID)
	if err != nil {
		return adminDID, nil, err
	}
	chain, err := c.GetSmartContractChain(ctx, contractHash)
	if err != nil {
		return adminDID, nil, fmt.Errorf("get chain: %w", err)
	}
	return adminDID, chain, nil
}

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

// AddActivity records an activity locally (activities table) AND writes
// it to the admin's add_activity contract chain so the mirror is
// verifiable on-chain. Returns the terminal sign result.
func (s *Service) AddActivity(ctx context.Context, adminDID, activityID string, rewardPoints int, description string) (*rubix.SignResult, string, error) {
	if activityID == "" {
		return nil, "", fmt.Errorf("activity_id is required")
	}
	if rewardPoints < 0 {
		return nil, "", fmt.Errorf("reward_points must be >= 0")
	}
	contractHash, err := database.GetAdminContract(ctx, adminDID, database.KindAddActivity)
	if err != nil {
		return nil, "", fmt.Errorf("admin has no add_activity contract: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"action":        "add_activity",
		"activity_id":   activityID,
		"reward_points": rewardPoints,
		"description":   description,
	})
	if err != nil {
		return nil, contractHash, fmt.Errorf("marshal payload: %w", err)
	}
	sr, err := s.ExecuteContract(ctx, adminDID, contractHash, string(payload))
	if err != nil {
		return nil, contractHash, err
	}
	// Mirror locally only after on-chain succeeds.
	if err := database.CreateActivity(ctx, &database.Activity{
		AdminDID:      adminDID,
		ActivityID:    activityID,
		RewardPoints:  rewardPoints,
		Description:   description,
		TransactionID: sr.TransactionID,
	}); err != nil {
		return sr, contractHash, fmt.Errorf("persist activity: %w", err)
	}
	return sr, contractHash, nil
}

// ---------------------------------------------------------------------------
// Admin
// ---------------------------------------------------------------------------

// AddAdmin appends new_admin_did to the existing admin's add_admin
// contract chain.
func (s *Service) AddAdmin(ctx context.Context, existingAdminDID, newAdminDID string) (*rubix.SignResult, string, error) {
	if !isPlausibleDID(newAdminDID) {
		return nil, "", fmt.Errorf("new_admin_did %q does not look like a valid Rubix DID", newAdminDID)
	}
	contractHash, err := database.GetAdminContract(ctx, existingAdminDID, database.KindAddAdmin)
	if err != nil {
		return nil, "", fmt.Errorf("existing admin has no add_admin contract: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"action":        "add_admin",
		"new_admin_did": newAdminDID,
	})
	if err != nil {
		return nil, contractHash, fmt.Errorf("marshal payload: %w", err)
	}
	sr, err := s.ExecuteContract(ctx, existingAdminDID, contractHash, string(payload))
	return sr, contractHash, err
}

// ---------------------------------------------------------------------------
// Reward transfer (core of the queue worker path)
// ---------------------------------------------------------------------------

// TransferRewardInput is the input to a reward transfer (queued flow).
type TransferRewardInput struct {
	AdminDID    string
	UserDID     string
	ActivityIDs []string
}

// TransferRewardResult is what the worker stores on success.
type TransferRewardResult struct {
	TransactionID string
	ContractHash  string
	RewardPoints  int
	Message       string
}

// ValidateTransferReward checks inputs that must be correct before the
// request is even enqueued. Heavy checks (contract lookup, points sum)
// happen in the worker so the API can respond fast.
func (s *Service) ValidateTransferReward(in TransferRewardInput) error {
	if !isPlausibleDID(in.AdminDID) {
		return fmt.Errorf("admin_did %q does not look valid", in.AdminDID)
	}
	if !isPlausibleDID(in.UserDID) {
		return fmt.Errorf("user_did %q does not look valid", in.UserDID)
	}
	if _, ok := s.Cfg.AdminByDID(in.AdminDID); !ok {
		return fmt.Errorf("admin_did %q is not registered in config", in.AdminDID)
	}
	if len(in.ActivityIDs) == 0 {
		return fmt.Errorf("activity_id must be a non-empty array")
	}
	for i, id := range in.ActivityIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("activity_id[%d] is empty", i)
		}
	}
	return nil
}

// ProcessTransferReward is the blocking happy-path executed by the admin
// worker goroutine. Performs: points lookup -> PostTx(FT+SC) -> Sign ->
// return result. The caller (worker) is responsible for updating DB
// state around this call.
func (s *Service) ProcessTransferReward(ctx context.Context, in TransferRewardInput) (*TransferRewardResult, error) {
	contractHash, err := database.GetAdminContract(ctx, in.AdminDID, database.KindReward)
	if err != nil {
		return nil, fmt.Errorf("admin has no reward contract: %w", err)
	}

	points, err := database.SumRewardPointsForActivities(ctx, in.AdminDID, in.ActivityIDs)
	if err != nil {
		return nil, fmt.Errorf("sum reward points: %w", err)
	}
	if points <= 0 {
		return nil, fmt.Errorf("activities resolved to zero reward points")
	}
	// Fail here rather than sending an empty creatorDID to the node, which
	// reports it as an opaque FT-not-found rather than a config problem.
	if s.Cfg.Env.FTCreatorDID == "" {
		return nil, fmt.Errorf("FT_CREATOR_DID is not set; reward transfers cannot name the minting DID")
	}

	payload, err := json.Marshal(map[string]any{
		"action":        "transfer",
		"user_did":      in.UserDID,
		"activity_ids":  in.ActivityIDs,
		"reward_points": points,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	c, a, err := s.ClientFor(in.AdminDID)
	if err != nil {
		return nil, err
	}

	reqID, err := c.PostTx(ctx, rubix.TransactionRequest{
		Initiator: in.AdminDID,
		Owner:     in.UserDID,
		Tokens: rubix.TransactionTokenDetails{
			// The FT leg moves the reward itself; the SC leg below records
			// the audit entry. Both settle in one unified transaction, so a
			// successful Sign means the tokens moved and the chain recorded
			// why.
			//
			// CreatorDID is the central minter, not the paying admin: Rubix
			// keys a fungible token on (name, creator), and every admin
			// spends FTs distributed from that one mint. Naming the admin
			// here would make the node look for FTs it minted itself and
			// find none.
			FT: []rubix.FTInfo{{
				FTName:      s.Cfg.Env.FTName,
				NumberOfFts: float64(points),
				CreatorDID:  s.Cfg.Env.FTCreatorDID,
			}},
			SmartContract: []rubix.SmartContractInfo{{
				SmartContractId: contractHash,
				Value:           0,
				Data:            string(payload),
			}},
		},
		Memo: fmt.Sprintf("reward to %s (activities=%d points=%d)", in.UserDID, len(in.ActivityIDs), points),
	})
	if err != nil {
		return nil, fmt.Errorf("post tx: %w", err)
	}
	sr, err := c.Sign(ctx, reqID, a.Password)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	return &TransferRewardResult{
		TransactionID: sr.TransactionID,
		ContractHash:  contractHash,
		RewardPoints:  points,
		Message:       sr.Message,
	}, nil
}

// ---------------------------------------------------------------------------
// Local helpers
// ---------------------------------------------------------------------------

// isPlausibleDID applies a cheap shape check. The real test happens on
// the Rubix node; this just rejects obvious garbage before we bother
// making an HTTP call.
func isPlausibleDID(did string) bool {
	if len(did) < 20 {
		return false
	}
	return strings.HasPrefix(did, "bafyb") || strings.HasPrefix(did, "Qm")
}
