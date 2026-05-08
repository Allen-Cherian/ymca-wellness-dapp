package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/queue"
	"ymca-wellness-dapp/internal/rubix"
	"ymca-wellness-dapp/internal/service"
)

// defaultHandlerTimeout caps any single handler's downstream work unless
// the caller supplies a longer context. Rubix Sign() can take tens of
// seconds on testnet so we're generous here.
const defaultHandlerTimeout = 3 * time.Minute

// -----------------------------------------------------------------------
// Reward transfer (async)
// -----------------------------------------------------------------------

func (s *Server) handleTransferReward(c *gin.Context) {
	var req TransferRewardRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}

	in := service.TransferRewardInput{
		AdminDID:    req.AdminDID,
		UserDID:     req.UserDID,
		ActivityIDs: req.ActivityID,
	}
	if err := s.Svc.ValidateTransferReward(in); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}

	requestID := uuid.NewString()
	// Persist queued row up front so the status endpoint works immediately.
	if err := database.CreateTransferStatus(c.Request.Context(), &database.TransferStatus{
		RequestID:    requestID,
		Kind:         database.KindReward,
		AdminDID:     req.AdminDID,
		UserDID:      req.UserDID,
		ActivityIDs:  req.ActivityID,
		RewardPoints: 0, // actual sum is looked up in the worker
		Status:       database.StatusQueued,
		Message:      "queued for processing",
	}); err != nil {
		log.Printf("transfer-reward: persist queued row: %v", err)
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Persistence error", Message: err.Error()})
		return
	}

	if err := s.Queue.Enqueue(&queue.TransferJob{
		RequestID:  requestID,
		Input:      in,
		EnqueuedAt: time.Now(),
	}); err != nil {
		// Queue full -> mark the row failed and return 503.
		_ = database.UpdateTransferStatus(c.Request.Context(), requestID, map[string]any{
			"status":        database.StatusFailed,
			"message":       "queue full, not enqueued",
			"error_details": err.Error(),
		})
		if errors.Is(err, queue.ErrQueueFull) {
			c.JSON(http.StatusServiceUnavailable, errResponse{
				Status:  false,
				Error:   err.Error(),
				Message: "System at capacity, please retry in a few minutes",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Enqueue error", Message: err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, TransferRewardResponse{
		Status:  true,
		Message: "Transfer request queued for processing",
		Data:    TransferRewardAcceptedData{RequestID: requestID},
	})
}

func (s *Server) handleTransferStatus(c *gin.Context) {
	requestID := c.Param("request_id")
	if requestID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "missing request_id"})
		return
	}
	ts, err := database.GetTransferStatusByRequestID(c.Request.Context(), requestID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, errResponse{Status: false, Error: "Transfer not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: transferStatusView(ts)})
}

func (s *Server) handleQueueMetrics(c *gin.Context) {
	total, perAdmin := s.Queue.Snapshot()
	rows := make([]gin.H, 0, len(perAdmin))
	for did, depth := range perAdmin {
		rows = append(rows, gin.H{"admin_did": did, "queue_size": depth})
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"total_admins": len(perAdmin),
		"total_queued": total,
		"admin_queues": rows,
	}})
}

// rewardHistoryEntry is one row in the reward history responses (per-user
// or per-admin). Only fields useful for a reward audit are exposed.
func rewardHistoryEntry(t *database.TransferStatus) gin.H {
	return gin.H{
		"date":           t.CreatedAt.UTC().Format(time.RFC3339),
		"activity_ids":   t.ActivityIDs,
		"reward_points":  t.RewardPoints,
		"transaction_id": t.TransactionID,
		"user_did":       t.UserDID,
		"admin_did":      t.AdminDID,
	}
}

func (s *Server) handleRewardsByUser(c *gin.Context) {
	userDID := c.Param("user_did")
	if userDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "user_did is required"})
		return
	}
	rows, err := database.ListSuccessfulRewardsByUserDID(c.Request.Context(), userDID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}
	rewards := make([]gin.H, 0, len(rows))
	for i := range rows {
		rewards = append(rewards, rewardHistoryEntry(&rows[i]))
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"user_did": userDID,
		"count":    len(rewards),
		"rewards":  rewards,
	}})
}

func (s *Server) handleAdminUserCount(c *gin.Context) {
	adminDID := c.Param("admin_did")
	if adminDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "admin_did is required"})
		return
	}
	if _, err := database.GetAdminByDID(c.Request.Context(), adminDID); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, errResponse{Status: false, Error: "admin not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}
	count, err := database.CountUsersForAdmin(c.Request.Context(), adminDID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "count failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did":  adminDID,
		"user_count": count,
	}})
}

func (s *Server) handleUserAdmin(c *gin.Context) {
	userDID := c.Param("user_did")
	if userDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "user_did is required"})
		return
	}
	adminDID, err := database.GetAdminForUser(c.Request.Context(), userDID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, errResponse{Status: false, Error: "no admin mapping for user"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"user_did":  userDID,
		"admin_did": adminDID,
	}})
}

func (s *Server) handleRewardsByAdmin(c *gin.Context) {
	adminDID := c.Param("admin_did")
	if adminDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "admin_did is required"})
		return
	}
	rows, err := database.ListSuccessfulRewardsByAdminDID(c.Request.Context(), adminDID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}
	rewards := make([]gin.H, 0, len(rows))
	for i := range rows {
		rewards = append(rewards, rewardHistoryEntry(&rows[i]))
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did": adminDID,
		"count":     len(rewards),
		"rewards":   rewards,
	}})
}

// transferStatusView shapes a TransferStatus row for JSON output.
func transferStatusView(t *database.TransferStatus) gin.H {
	return gin.H{
		"request_id":     t.RequestID,
		"transaction_id": t.TransactionID,
		"kind":           t.Kind,
		"admin_did":      t.AdminDID,
		"user_did":       t.UserDID,
		"activity_ids":   t.ActivityIDs,
		"reward_points":  t.RewardPoints,
		"contract_hash":  t.ContractHash,
		"status":         t.Status,
		"message":        t.Message,
		"error_details":  t.ErrorDetails,
		"created_at":     t.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":     t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// -----------------------------------------------------------------------
// Activity / admin / create-did / deploy / execute (sync)
// -----------------------------------------------------------------------

func (s *Server) handleAddActivity(c *gin.Context) {
	var req AddActivityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	if req.ActivityID == "" || req.AdminDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "activity_id and admin_did are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	sr, hash, err := s.Svc.AddActivity(ctx, req.AdminDID, req.ActivityID, req.RewardPoints, req.Description)
	if err != nil {
		c.JSON(rubixErrorStatus(err), errResponse{Error: "add activity failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "Activity added to smart contract tokenchain",
		"data": gin.H{
			"transaction_id": sr.TransactionID,
			"contract_hash":  hash,
			"message":        sr.Message,
		},
	})
}

func (s *Server) handleAddAdmin(c *gin.Context) {
	var req AddAdminRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	if req.NewAdminDID == "" || req.ExistingAdminDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "new_admin_did and existing_admin_did are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	sr, hash, err := s.Svc.AddAdmin(ctx, req.ExistingAdminDID, req.NewAdminDID)
	if err != nil {
		c.JSON(rubixErrorStatus(err), errResponse{Error: "add admin failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "Admin added to smart contract tokenchain",
		"data": gin.H{
			"transaction_id": sr.TransactionID,
			"contract_hash":  hash,
			"message":        sr.Message,
		},
	})
}

func (s *Server) handleCreateDIDWithPubKey(c *gin.Context) {
	var req CreateDIDWithPubKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	if req.AdminDID == "" || req.PublicKey == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "admin_did and public_key are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	did, err := s.Svc.CreateDIDWithPubKey(ctx, req.AdminDID, req.PublicKey)
	if err != nil {
		c.JSON(rubixErrorStatus(err), errResponse{Error: "create did failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{"did": did}})
}

func (s *Server) handleDeployContract(c *gin.Context) {
	var req DeployContractRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	if req.DeployerDID == "" || req.WASMPath == "" || req.LibPath == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "deployer_did, wasm_path and lib_path are required"})
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = database.KindReward
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	token, err := s.Svc.DeployContract(ctx, req.DeployerDID, kind, req.WASMPath, req.LibPath)
	if err != nil {
		c.JSON(rubixErrorStatus(err), errResponse{Error: "deploy failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "Contract Deployed Successfully",
		"data": gin.H{
			"ContractHash": token,
			"Kind":         kind,
			"Success":      true,
			"Message":      "Contract deployed successfully",
		},
	})
}

func (s *Server) handleExecuteContract(c *gin.Context) {
	var req ExecuteContractRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	if req.ContractHash == "" || req.ExecutorDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "contract_hash and executor_did are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	sr, err := s.Svc.ExecuteContract(ctx, req.ExecutorDID, req.ContractHash, req.ContractInput)
	if err != nil {
		c.JSON(rubixErrorStatus(err), errResponse{Error: "execute failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "DApp executed successfully",
		"data": gin.H{
			"transaction_id": sr.TransactionID,
			"message":        sr.Message,
			"Success":        true,
		},
	})
}

// -----------------------------------------------------------------------
// Contract chain (sync)
// -----------------------------------------------------------------------

func (s *Server) handleContractChainByKind(c *gin.Context) {
	adminDID := c.Param("admin_did")
	kind := c.Param("kind")
	if adminDID == "" || kind == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "admin_did and kind are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	hash, chain, err := s.Svc.GetContractChain(ctx, adminDID, kind)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, errResponse{Error: "contract not found for admin/kind"})
			return
		}
		c.JSON(rubixErrorStatus(err), errResponse{Error: "chain lookup failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did":     adminDID,
		"kind":          kind,
		"contract_hash": hash,
		"chain":         chain,
		"count":         len(chain),
	}})
}

func (s *Server) handleContractChainByHash(c *gin.Context) {
	hash := c.Param("contract_hash")
	if hash == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "contract_hash is required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHandlerTimeout)
	defer cancel()

	adminDID, chain, err := s.Svc.GetContractChainByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, errResponse{Error: "contract not registered"})
			return
		}
		c.JSON(rubixErrorStatus(err), errResponse{Error: "chain lookup failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"admin_did":     adminDID,
		"contract_hash": hash,
		"chain":         chain,
		"count":         len(chain),
	}})
}

// rubixErrorStatus maps Rubix client errors to an HTTP status.
func rubixErrorStatus(err error) int {
	if rubix.IsAPIError(err) {
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}
