package server

// Request/response DTOs for the dApp HTTP surface. Field names are
// stable and match v1 where practical so existing clients keep working.

// TransferRewardRequest is POST /api/rewards/transfer body.
type TransferRewardRequest struct {
	UserDID    string   `json:"user_did"`
	AdminDID   string   `json:"admin_did"`
	ActivityID []string `json:"activity_id"`
}

// TransferRewardResponse is returned with HTTP 202 Accepted.
type TransferRewardResponse struct {
	Status  bool                      `json:"status"`
	Message string                    `json:"message"`
	Data    TransferRewardAcceptedData `json:"data"`
}

type TransferRewardAcceptedData struct {
	RequestID string `json:"request_id"`
}

// AddActivityRequest is POST /api/activity/add body.
type AddActivityRequest struct {
	ActivityID   string `json:"activity_id"`
	RewardPoints int    `json:"reward_points"`
	AdminDID     string `json:"admin_did"`
	Description  string `json:"description,omitempty"`
}

// AddAdminRequest is POST /api/admin/add body.
type AddAdminRequest struct {
	NewAdminDID      string `json:"new_admin_did"`
	ExistingAdminDID string `json:"existing_admin_did"`
}

// CreateDIDWithPubKeyRequest is POST /api/create-did-with-pubkey body.
type CreateDIDWithPubKeyRequest struct {
	AdminDID  string `json:"admin_did"`
	PublicKey string `json:"public_key"`
}

// DeployContractRequest is POST /api/deploy-contract body.
type DeployContractRequest struct {
	DeployerDID string `json:"deployer_did"`
	Kind        string `json:"kind"`
	WASMPath    string `json:"wasm_path"`
	LibPath     string `json:"lib_path"`
	// StatePath is accepted for v1 compatibility but ignored: the v2
	// /rubix/v1/smart_contracts/generate endpoint only takes wasm + rs.
	StatePath string `json:"state_path,omitempty"`
}

// ExecuteContractRequest is POST /api/execute-contract body.
type ExecuteContractRequest struct {
	ContractHash   string `json:"contract_hash"`
	ExecutorDID    string `json:"executor_did"`
	ContractInput  string `json:"contract_input"`
}

// LoginRequest is POST /api/auth/login body.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// RefreshRequest is POST /api/auth/refresh body.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// LogoutRequest is POST /api/auth/logout body. Either revokes the single
// refresh token in the body or, with all=true, every active refresh for
// the authenticated user.
type LogoutRequest struct {
	RefreshToken string `json:"refresh_token,omitempty"`
	All          bool   `json:"all,omitempty"`
}

// TokenPairResponse is returned by /api/auth/login and /api/auth/refresh.
type TokenPairResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// ----- Generic response envelopes -----

type okResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

type errResponse struct {
	Status  bool   `json:"status"`
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}
