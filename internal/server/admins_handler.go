package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"ymca-wellness-dapp/internal/config"
	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/rubix"
)

// didsDir is the on-disk location for per-admin DID metadata files.
const didsDir = "./dids"

// defaultAdminPassword is used when a setup entry omits the password.
const defaultAdminPassword = "mypassword"

// SetupAdminsRequest is the body of POST /api/admins/setup.
type SetupAdminsRequest struct {
	Admins []SetupAdminEntry `json:"admins"`
}

// SetupAdminEntry is one admin to provision.
type SetupAdminEntry struct {
	NodePort string `json:"node_port"`
	Password string `json:"password,omitempty"`
}

// SetupAdminResult is one admin's outcome in the response.
type SetupAdminResult struct {
	NodePort string `json:"node_port"`
	DID      string `json:"did,omitempty"`
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
}

// SetupAdminsResponse is the body returned by /api/admins/setup.
type SetupAdminsResponse struct {
	Status bool             `json:"status"`
	Data   SetupAdminsData  `json:"data"`
}

// SetupAdminsData wraps the per-admin results plus a summary.
type SetupAdminsData struct {
	Admins  []SetupAdminResult `json:"admins"`
	Summary SetupAdminsSummary `json:"summary"`
}

// SetupAdminsSummary is the aggregate counts.
type SetupAdminsSummary struct {
	Requested int `json:"requested"`
	Created   int `json:"created"`
	Failed    int `json:"failed"`
}

// handleSetupAdmins provisions one or more admins on Rubix nodes,
// records them in the admins table, writes per-admin metadata files,
// and refreshes the in-memory admin map.
func (s *Server) handleSetupAdmins(c *gin.Context) {
	var req SetupAdminsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	if len(req.Admins) == 0 {
		c.JSON(http.StatusBadRequest, errResponse{Error: "admins list is required and must be non-empty"})
		return
	}

	// Per-admin processing — best-effort: failures don't stop the loop.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()

	if err := os.MkdirAll(didsDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{
			Error:   "dids directory",
			Message: err.Error(),
		})
		return
	}

	results := make([]SetupAdminResult, len(req.Admins))
	created := 0
	failed := 0

	for i, entry := range req.Admins {
		results[i] = SetupAdminResult{NodePort: entry.NodePort}

		if entry.NodePort == "" {
			results[i].Error = "node_port is required"
			failed++
			continue
		}
		password := entry.Password
		if password == "" {
			password = defaultAdminPassword
		}

		did, err := s.provisionOne(ctx, entry.NodePort, password)
		if err != nil {
			results[i].Error = err.Error()
			failed++
			continue
		}
		results[i].DID = did
		results[i].Success = true
		created++
	}

	// Refresh the in-memory admin map so newly-created admins are
	// usable on subsequent requests.
	if err := s.Cfg.ReloadAdmins(ctx); err != nil {
		// Log only — the rows are persisted; a restart will reload them.
		c.JSON(http.StatusInternalServerError, errResponse{
			Error:   "reload admins",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, SetupAdminsResponse{
		Status: true,
		Data: SetupAdminsData{
			Admins: results,
			Summary: SetupAdminsSummary{
				Requested: len(req.Admins),
				Created:   created,
				Failed:    failed,
			},
		},
	})
}

// provisionOne creates a DID on the node at port, persists the admin row,
// and writes the dids/<did>.json metadata file.
func (s *Server) provisionOne(ctx context.Context, nodePort, password string) (string, error) {
	baseURL := fmt.Sprintf("%s:%s", config.NodeHost, nodePort)
	client, err := s.Svc.Pool.ForBaseURL(baseURL)
	if err != nil {
		return "", fmt.Errorf("rubix client: %w", err)
	}

	res, err := client.CreateDID(ctx, rubix.CreateDIDRequest{Password: password})
	if err != nil {
		return "", fmt.Errorf("create DID on %s: %w", baseURL, err)
	}
	if res.DID == "" {
		return "", fmt.Errorf("rubix returned empty DID")
	}

	// Best-effort: announce the DID to the network. Failure is logged but
	// does not abort provisioning — the admin row will still be persisted
	// and can be re-registered out of band if needed.
	if reqID, rerr := client.RegisterDID(ctx, res.DID); rerr != nil {
		log.Printf("setup-admin %s: register DID failed: %v", res.DID, rerr)
	} else if _, serr := client.Sign(ctx, reqID, password); serr != nil {
		log.Printf("setup-admin %s: sign register failed: %v", res.DID, serr)
	}

	if err := database.CreateAdmin(ctx, &database.Admin{
		DID:      res.DID,
		NodePort: nodePort,
		Password: password,
	}); err != nil {
		return "", fmt.Errorf("persist admin: %w", err)
	}

	if err := writeDIDFile(res.DID, nodePort, password); err != nil {
		// Non-fatal — the admin is in the DB. Log via response message
		// would be better but we just append to error.
		return res.DID, fmt.Errorf("did persisted but file write failed: %w", err)
	}

	return res.DID, nil
}

// didFile is the JSON shape written to dids/<did>.json.
type didFile struct {
	DID       string `json:"did"`
	NodeHost  string `json:"node_host"`
	NodePort  string `json:"node_port"`
	Password  string `json:"password"`
	CreatedAt string `json:"created_at"`
}

// writeDIDFile persists per-admin metadata to dids/<did>.json with mode 0600.
func writeDIDFile(did, nodePort, password string) error {
	body := didFile{
		DID:       did,
		NodeHost:  config.NodeHost,
		NodePort:  nodePort,
		Password:  password,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	path := filepath.Join(didsDir, did+".json")
	return os.WriteFile(path, b, 0o600)
}
