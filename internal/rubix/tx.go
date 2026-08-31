package rubix

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// txIDFromMessage matches Rubix Sign success messages of the form
// "Transaction <hex64> completed successfully" and extracts the hex tx id.
var txIDFromMessage = regexp.MustCompile(`Transaction\s+([0-9a-fA-F]{64})\b`)

// FTInfo is one FT leg of a unified transaction.
type FTInfo struct {
	FTName      string  `json:"ftName"`
	NumberOfFts float64 `json:"numberOfFts"`
	CreatorDID  string  `json:"creatorDID"`
}

// NFTInfo is one NFT leg of a unified transaction.
type NFTInfo struct {
	NFTId string  `json:"nftId"`
	Value float64 `json:"value"`
	Data  string  `json:"data"`
}

// SmartContractInfo is one smart-contract leg of a unified transaction.
// Data is an opaque string forwarded to chain subscribers.
type SmartContractInfo struct {
	SmartContractId string  `json:"smartContractId"`
	Value           float64 `json:"value"`
	Data            string  `json:"data"`
}

// TransactionTokenDetails groups all asset classes in one tx.
type TransactionTokenDetails struct {
	RBT                  float64             `json:"rbt,omitempty"`
	FT                   []FTInfo            `json:"ft,omitempty"`
	NFT                  []NFTInfo           `json:"nft,omitempty"`
	SmartContract        []SmartContractInfo `json:"smartContract,omitempty"`
	TransferNFTOwnership bool                `json:"transferNftOwnership,omitempty"`
}

// TransactionRequest is the body of POST /rubix/v1/tx.
type TransactionRequest struct {
	Initiator string                  `json:"initiator"`
	Owner     string                  `json:"owner"`
	Tokens    TransactionTokenDetails `json:"tokens"`
	Memo      string                  `json:"memo,omitempty"`
}

// PostTx submits a unified transaction. The returned requestID is the
// value the Rubix node uses to correlate the signing step; it is NOT the
// final on-chain transaction id. Callers must follow up with Sign() using
// this requestID.
func (c *Client) PostTx(ctx context.Context, req TransactionRequest) (string, error) {
	br, err := c.postJSON(ctx, "/rubix/v1/tx", req)
	if err != nil {
		return "", err
	}
	// Preferred: handler emits SignReqData-shaped result {id, hash} where
	// id is the request id and hash is the payload the caller signs via
	// /rubix/v1/signature. See server/transaction.go + types/crypto.go.
	var asObj struct {
		ID        string `json:"id"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(br.Result, &asObj); err == nil {
		if asObj.ID != "" {
			return asObj.ID, nil
		}
		if asObj.RequestID != "" {
			return asObj.RequestID, nil
		}
	}
	// Fallback: bare request-id string.
	var asString string
	if err := json.Unmarshal(br.Result, &asString); err == nil && asString != "" {
		return asString, nil
	}
	return "", fmt.Errorf("rubix /rubix/v1/tx: could not extract request id from result: %s", truncate(string(br.Result), 200))
}

// SignRequest is the body of POST /rubix/v1/signature.
// Mode is 0 for standard password-based signing.
type SignRequest struct {
	ID       string `json:"id"`
	Mode     int    `json:"mode"`
	Password string `json:"password"`
}

// SignResult is the parsed response of POST /rubix/v1/signature.
//
// Per the verification doc, the terminal .result shape is NOT statically
// typed — the originating handler goroutine writes whatever it wants into
// the response. For RBT/FT transfers this is typically a bare tx id
// string; for contract generate it carries the minted contract token id.
// Callers that need the flow-specific payload should decode Result
// themselves.
type SignResult struct {
	TransactionID string
	Message       string
	Result        json.RawMessage
}

// Sign completes the async request started by PostTx/MintFT/
// GenerateSmartContract. This call blocks on the Rubix side until quorum
// signing finishes, then returns the terminal response.
func (c *Client) Sign(ctx context.Context, requestID, password string) (*SignResult, error) {
	body := SignRequest{ID: requestID, Mode: 0, Password: password}
	br, err := c.postJSON(ctx, "/rubix/v1/signature", body)
	if err != nil {
		return nil, err
	}
	res := &SignResult{Message: br.Message, Result: br.Result}

	// Best-effort tx id extraction. Some flows embed it here; flows that
	// use Result for a different payload will leave TransactionID empty
	// and the caller must decode Result itself.
	var asString string
	if jerr := json.Unmarshal(br.Result, &asString); jerr == nil && asString != "" {
		res.TransactionID = asString
		return res, nil
	}
	var asObj struct {
		TxID          string `json:"tx_id"`
		TransactionID string `json:"transaction_id"`
		ID            string `json:"id"`
	}
	if jerr := json.Unmarshal(br.Result, &asObj); jerr == nil {
		switch {
		case asObj.TransactionID != "":
			res.TransactionID = asObj.TransactionID
		case asObj.TxID != "":
			res.TransactionID = asObj.TxID
		case asObj.ID != "":
			res.TransactionID = asObj.ID
		}
	}
	// Final fallback: some flows return the tx id only inside the Message
	// (e.g. "Transaction <hex64> completed successfully").
	if res.TransactionID == "" && res.Message != "" {
		if m := txIDFromMessage.FindStringSubmatch(res.Message); len(m) == 2 {
			res.TransactionID = m[1]
		}
	}
	return res, nil
}
