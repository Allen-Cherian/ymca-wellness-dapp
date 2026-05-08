// v1_aliases.go — compatibility shims for the original client API
// contract documented at proxy.rubix.network. Each handler reuses the
// existing v2 service-layer logic and only reshapes the JSON response.
//
// Some response fields have no equivalent in v2's architecture (no
// callback flow → no BlockNo / Epoch / InitiatorSignature). These are
// returned as nil/empty per the agreed contract; clients should treat
// them as advisory.
//
// Mapping conventions (set per agreed contract):
//   block_id          ← transaction_id
//   ft_transfer_txid  ← transaction_id
//   blockchain_tx_id  ← transaction_id
//   queued_at         ← "" (empty)
//   started_at        ← created_at
//   completed_at      ← updated_at
//   BlockNo / Epoch / InitiatorSignature / InitiatorSignData → nil

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/queue"
	"ymca-wellness-dapp/internal/service"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// POST /createdid
// -----------------------------------------------------------------------

func (s *Server) handleV1CreateDID(c *gin.Context) {
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
	c.JSON(http.StatusOK, gin.H{"did": did})
}

// -----------------------------------------------------------------------
// POST /admin/activity/add
// -----------------------------------------------------------------------

func (s *Server) handleV1AddActivity(c *gin.Context) {
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

	sr, _, err := s.Svc.AddActivity(ctx, req.AdminDID, req.ActivityID, req.RewardPoints, req.Description)
	if err != nil {
		c.JSON(rubixErrorStatus(err), errResponse{Error: "add activity failed", Message: err.Error()})
		return
	}

	scData := fmt.Sprintf(`{"activity_id":"%s","reward_points":%d}`, req.ActivityID, req.RewardPoints)
	c.JSON(http.StatusOK, gin.H{
		"status":  true,
		"message": "Activity added successfully",
		"result": []gin.H{{
			"BlockNo":           nil,
			"BlockId":           sr.TransactionID,
			"SmartContractData": scData,
			"Epoch":             nil,
		}},
	})
}

// -----------------------------------------------------------------------
// POST /admin/payouts (queues a reward transfer; client polls status)
// -----------------------------------------------------------------------

func (s *Server) handleV1Payouts(c *gin.Context) {
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
	if err := database.CreateTransferStatus(c.Request.Context(), &database.TransferStatus{
		RequestID:    requestID,
		Kind:         database.KindReward,
		AdminDID:     req.AdminDID,
		UserDID:      req.UserDID,
		ActivityIDs:  req.ActivityID,
		RewardPoints: 0,
		Status:       database.StatusQueued,
		Message:      "queued for processing",
	}); err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "Persistence error", Message: err.Error()})
		return
	}

	if err := s.Queue.Enqueue(&queue.TransferJob{
		RequestID:  requestID,
		Input:      in,
		EnqueuedAt: time.Now(),
	}); err != nil {
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

	c.JSON(http.StatusAccepted, gin.H{
		"status":  true,
		"message": "Transfer request queued for processing",
		"result":  gin.H{"request_id": requestID},
	})
}

// -----------------------------------------------------------------------
// GET /admin/payouts/status/:request_id
// -----------------------------------------------------------------------

func (s *Server) handleV1PayoutStatus(c *gin.Context) {
	requestID := c.Param("request_id")
	if requestID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "missing request_id"})
		return
	}
	t, err := database.GetTransferStatusByRequestID(c.Request.Context(), requestID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, errResponse{Status: false, Error: "Transfer not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}

	created := t.CreatedAt.UTC().Format(time.RFC3339)
	updated := t.UpdatedAt.UTC().Format(time.RFC3339)

	c.JSON(http.StatusOK, gin.H{
		"status":  true,
		"message": t.Message,
		"result": gin.H{
			"request_id":       t.RequestID,
			"activity_ids":     t.ActivityIDs,
			"admin_did":        t.AdminDID,
			"user_did":         t.UserDID,
			"reward_points":    t.RewardPoints,
			"contract_hash":    t.ContractHash,
			"status":           t.Status,
			"message":          t.Message,
			"error_details":    t.ErrorDetails,
			"block_id":         t.TransactionID,
			"blockchain_tx_id": t.TransactionID,
			"ft_transfer_txid": t.TransactionID,
			"queued_at":        "",
			"started_at":       created,
			"completed_at":     updated,
			"created_at":       created,
			"updated_at":       updated,
		},
	})
}

// -----------------------------------------------------------------------
// GET /admin/activity/list
// -----------------------------------------------------------------------

func (s *Server) handleV1ActivityList(c *gin.Context) {
	adminDID := c.Query("admin_did") // optional filter
	rows, err := database.ListActivities(c.Request.Context(), adminDID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, a := range rows {
		out = append(out, gin.H{
			"activity_id":   a.ActivityID,
			"block_hash":    a.TransactionID, // empty for activities created before migration 004
			"reward_points": a.RewardPoints,
		})
	}
	c.JSON(http.StatusOK, out)
}

// -----------------------------------------------------------------------
// POST /admin/user/add (alias of /api/admin/add)
// -----------------------------------------------------------------------

func (s *Server) handleV1UserAdd(c *gin.Context) {
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

	sr, _, err := s.Svc.AddAdmin(ctx, req.ExistingAdminDID, req.NewAdminDID)
	if err != nil {
		c.JSON(rubixErrorStatus(err), errResponse{Error: "add admin failed", Message: err.Error()})
		return
	}

	scData := fmt.Sprintf(`{"add_admin": {"admin_did":"%s"}}`, req.NewAdminDID)
	c.JSON(http.StatusOK, gin.H{
		"status":  true,
		"message": "Fetched latest block smart contract data",
		"result": []gin.H{{
			"BlockNo":            nil,
			"BlockId":            sr.TransactionID,
			"SmartContractData":  scData,
			"Epoch":              nil,
			"InitiatorSignature": nil,
			"ExecutorDID":        req.ExistingAdminDID,
			"InitiatorSignData":  nil,
		}},
	})
}

// -----------------------------------------------------------------------
// GET /users/:user_did/payouts
// -----------------------------------------------------------------------

func (s *Server) handleV1UserPayouts(c *gin.Context) {
	userDID := c.Param("user_did")
	if userDID == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "user_did is required"})
		return
	}
	entries, err := database.FTInfoForUser(c.Request.Context(), userDID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}
	ftInfo := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		ftInfo = append(ftInfo, gin.H{
			"ft_name":     s.Cfg.Env.FTName,
			"ft_count":    e.TotalCount,
			"creator_did": e.CreatorDID,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"status":  true,
		"message": "Got FT info successfully",
		"result":  nil,
		"ft_info": ftInfo,
	})
}

