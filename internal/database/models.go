package database

import "time"

// Status values for TransferStatus.Status.
const (
	StatusQueued     = "queued"
	StatusProcessing = "processing"
	StatusSuccess    = "success"
	StatusFailed     = "failed"
)

// Kind values for TransferStatus.Kind.
const (
	KindReward       = "reward"
	KindAddActivity  = "add_activity"
	KindAddAdmin     = "add_admin"
	KindDeploy       = "deploy"
	KindCreateDID    = "create_did"
)

// TransferStatus mirrors a row in transfer_status.
type TransferStatus struct {
	RequestID     string
	TransactionID string
	Kind          string
	AdminDID      string
	UserDID       string
	ActivityIDs   []string
	RewardPoints  int
	ContractHash  string
	Status        string
	Message       string
	ErrorDetails  string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// UserAdmin mirrors a row in user_admins. Records which admin minted /
// onboarded a given user DID. One admin per user.
type UserAdmin struct {
	UserDID   string
	AdminDID  string
	CreatedAt time.Time
}

// AdminContract mirrors a row in admin_contracts.
type AdminContract struct {
	AdminDID     string
	ContractKind string
	ContractHash string
	DeployedAt   time.Time
}

// Activity mirrors a row in activities.
type Activity struct {
	AdminDID     string
	ActivityID   string
	RewardPoints int
	Description  string
	CreatedAt    time.Time
}

// Admin mirrors a row in admins. Admins are provisioned via
// POST /api/admins/setup; node_host is implicit ("http://localhost").
type Admin struct {
	DID       string
	NodePort  string
	Password  string
	CreatedAt time.Time
}
