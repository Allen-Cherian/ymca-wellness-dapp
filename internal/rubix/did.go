package rubix

import (
	"context"
	"encoding/json"
	"fmt"
)

// CreateDIDRequest is the body of POST /rubix/v1/dids/create.
//
// If PublicKey is empty, the node creates a local DID: it generates the
// keypair from Mnemonic (auto-generating a mnemonic if none is supplied)
// and writes private key + mnemonic to disk under the node's did
// directory. Password protects the on-disk private key and is required
// later by /rubix/v1/signature.
type CreateDIDRequest struct {
	Password   string `json:"password"`
	PublicKey  string `json:"public_key,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Mnemonic   string `json:"mnemonic,omitempty"`
	ChildPath  int    `json:"childPath,omitempty"`
}

// CreateDIDResult is the parsed result of POST /rubix/v1/dids/create.
type CreateDIDResult struct {
	DID    string `json:"did"`
	PeerID string `json:"peer_id"`
}

// CreateDID provisions a new DID on the node.
func (c *Client) CreateDID(ctx context.Context, req CreateDIDRequest) (*CreateDIDResult, error) {
	br, err := c.postJSON(ctx, "/rubix/v1/dids/create", req)
	if err != nil {
		return nil, err
	}
	var res CreateDIDResult
	if jerr := json.Unmarshal(br.Result, &res); jerr != nil || res.DID == "" {
		return nil, fmt.Errorf("rubix CreateDID: could not decode result: %s", truncate(string(br.Result), 200))
	}
	return &res, nil
}

// RegisterDID announces a previously-created DID to the network. The
// returned requestID must be signed via Sign() to complete registration.
//
// Endpoint: POST /rubix/v1/dids/<did>/register
func (c *Client) RegisterDID(ctx context.Context, did string) (string, error) {
	if did == "" {
		return "", fmt.Errorf("rubix RegisterDID: did is required")
	}
	br, err := c.postJSON(ctx, "/rubix/v1/dids/"+did+"/register", nil)
	if err != nil {
		return "", err
	}
	// Preferred: SignReqData-shaped result {id, hash}.
	var asObj struct {
		ID        string `json:"id"`
		RequestID string `json:"request_id"`
	}
	if jerr := json.Unmarshal(br.Result, &asObj); jerr == nil {
		if asObj.ID != "" {
			return asObj.ID, nil
		}
		if asObj.RequestID != "" {
			return asObj.RequestID, nil
		}
	}
	// Fallback: bare request-id string.
	var asString string
	if jerr := json.Unmarshal(br.Result, &asString); jerr == nil && asString != "" {
		return asString, nil
	}
	return "", fmt.Errorf("rubix RegisterDID: could not extract request id from result: %s", truncate(string(br.Result), 200))
}

// Test RBT was previously issued via POST /api/generate-local-rbt. That
// endpoint is deprecated and now 404s, so the client method was removed
// rather than left as a call that cannot succeed. Fund nodes through
// whatever mechanism the current rubixgoplatform build provides.
