// Package server wires HTTP routes to handlers. Phase 1 exposed only a
// health endpoint; Phase 3 adds the full dApp surface.
package server

import (
	"net/http"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"ymca-wellness-dapp/internal/auth"
	"ymca-wellness-dapp/internal/config"
	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/queue"
	"ymca-wellness-dapp/internal/service"
)

// Server bundles the gin engine and shared dependencies.
type Server struct {
	Cfg    *config.AppConfig
	Svc    *service.Service
	Queue  *queue.Manager
	Keys   *auth.Keys
	Engine *gin.Engine
}

// New builds a configured *Server.
func New(cfg *config.AppConfig, svc *service.Service, qm *queue.Manager, keys *auth.Keys) *Server {
	r := gin.Default()

	// Permissive CORS for dev; tighten in prod.
	r.Use(cors.New(cors.Config{
		AllowAllOrigins:  true,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: false,
	}))

	s := &Server{Cfg: cfg, Svc: svc, Queue: qm, Keys: keys, Engine: r}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	// --- Open routes (no token required) ---
	s.Engine.GET("/api/health", s.handleHealth)
	s.Engine.POST("/api/auth/login", s.handleLogin)
	s.Engine.POST("/api/auth/refresh", s.handleRefresh)

	// --- Protected routes ---
	// Everything else (v2 /api/* and the v1 aliases) sits behind
	// RequireAuth. Bootstrap flow: env-seeded operator -> login ->
	// /api/admins/setup. The earlier "setup is open" trust boundary is
	// gone; the env-seeded credential is the new boundary.
	protected := s.Engine.Group("", auth.RequireAuth(s.Keys, database.RoleOperator))

	// Authenticated auth endpoints
	protected.POST("/api/auth/logout", s.handleLogout)
	protected.GET("/api/auth/me", s.handleMe)
	protected.POST("/api/auth/users", s.handleCreateUser)

	api := protected.Group("/api")
	{
		// Reward transfer (async)
		api.POST("/rewards/transfer", s.handleTransferReward)
		api.GET("/rewards/status/:request_id", s.handleTransferStatus)
		api.GET("/queue/metrics", s.handleQueueMetrics)

		// Reward history
		api.GET("/rewards/user/:user_did", s.handleRewardsByUser)
		api.GET("/rewards/admin/:admin_did", s.handleRewardsByAdmin)

		// User -> admin mapping
		api.GET("/users/:user_did/admin", s.handleUserAdmin)
		api.GET("/admins/:admin_did/users/count", s.handleAdminUserCount)

		// Direct-chain ops (sync)
		api.POST("/activity/add", s.handleAddActivity)
		api.POST("/admin/add", s.handleAddAdmin)
		api.POST("/create-did-with-pubkey", s.handleCreateDIDWithPubKey)
		api.POST("/deploy-contract", s.handleDeployContract)
		api.POST("/execute-contract", s.handleExecuteContract)

		// Contract audit (sync)
		api.GET("/contracts/:admin_did/:kind/chain", s.handleContractChainByKind)
		api.GET("/contracts/by-hash/:contract_hash/chain", s.handleContractChainByHash)

		// Admin provisioning
		api.POST("/admins/setup", s.handleSetupAdmins)
	}

	// v1 client-contract aliases. Same handlers under different paths /
	// response shapes to honor the original proxy.rubix.network API doc.
	// Reuses v2 service-layer logic; only the request/response
	// marshaling differs.
	protected.POST("/createdid", s.handleV1CreateDID)
	protected.POST("/admin/activity/add", s.handleV1AddActivity)
	protected.GET("/admin/activity/list", s.handleV1ActivityList)
	protected.POST("/admin/payouts", s.handleV1Payouts)
	protected.GET("/admin/payouts/status/:request_id", s.handleV1PayoutStatus)
	protected.POST("/admin/user/add", s.handleV1UserAdd)
	protected.GET("/users/:user_did/payouts", s.handleV1UserPayouts)
}

func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"admins":  s.Cfg.AdminCount(),
		"ft_name": s.Cfg.Env.FTName,
	})
}

// Run starts the HTTP server on the configured port.
func (s *Server) Run() error {
	return s.Engine.Run(":" + s.Cfg.Env.ServerPort)
}
