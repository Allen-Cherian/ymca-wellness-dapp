package rubix

import (
	"context"
	"encoding/json"
	"fmt"
)

// MintFTRequest is the body of POST /rubix/v1/fts/mint.
//
// token_count is the number of underlying RBT tokens to burn/split into the
// new FT. ft_count is the total FT units produced. The admin DID must hold
// at least token_count RBT before calling.
type MintFTRequest struct {
	DID             string `json:"did"`
	FTName          string `json:"ft_name"`
	FTCount         int    `json:"ft_count"`
	TokenCount      int    `json:"token_count"`
	FTNumStartIndex int    `json:"ft_num_start_index"`
}

// MintFT creates a new fungible token under did. Minting is asynchronous on
// the Rubix side: the returned requestID must be signed via Sign() before
// the FT becomes usable.
func (c *Client) MintFT(ctx context.Context, req MintFTRequest) (string, error) {
	br, err := c.postJSON(ctx, "/rubix/v1/fts/mint", req)
	if err != nil {
		return "", err
	}

	// Preferred: SignReqData {id, hash}.
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
	return "", fmt.Errorf("rubix MintFT: could not extract request id from result: %s", truncate(string(br.Result), 200))
}
