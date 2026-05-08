package rubix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// GenerateSmartContract uploads (wasm, rs) and starts async contract
// minting. The returned requestID is the correlation key for the
// subsequent /rubix/v1/signature call; the minted contract token id
// arrives only in the signature response. Callers MUST chain Sign() and
// then ContractTokenFromSignResult() to obtain the actual contract id.
//
// Endpoint: POST /rubix/v1/smart_contracts/generate (multipart/form-data)
// Fields:   did, binaryCodePath, rawCodePath
func (c *Client) GenerateSmartContract(ctx context.Context, did, wasmPath, rsPath string) (string, error) {
	if _, err := os.Stat(wasmPath); err != nil {
		return "", fmt.Errorf("rubix GenerateSmartContract: wasm not found: %w", err)
	}
	if _, err := os.Stat(rsPath); err != nil {
		return "", fmt.Errorf("rubix GenerateSmartContract: rs not found: %w", err)
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if err := w.WriteField("did", did); err != nil {
		return "", fmt.Errorf("rubix GenerateSmartContract: write did field: %w", err)
	}
	if err := attachFile(w, "binaryCodePath", wasmPath); err != nil {
		return "", err
	}
	if err := attachFile(w, "rawCodePath", rsPath); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("rubix GenerateSmartContract: close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.resolve("/rubix/v1/smart_contracts/generate", nil), &buf)
	if err != nil {
		return "", fmt.Errorf("rubix GenerateSmartContract: new request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	br, err := c.do(req, "/rubix/v1/smart_contracts/generate")
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
	return "", fmt.Errorf("rubix GenerateSmartContract: could not extract request id from result: %s", truncate(string(br.Result), 200))
}

// ContractTokenFromSignResult extracts the minted smart-contract token id
// from a SignResult produced by signing a GenerateSmartContract request.
// Handles bare-string, {smart_contract_token}, and {hash}/{id} shapes.
func ContractTokenFromSignResult(sr *SignResult) (string, error) {
	if sr == nil || len(sr.Result) == 0 {
		return "", fmt.Errorf("rubix: empty sign result")
	}
	var asString string
	if jerr := json.Unmarshal(sr.Result, &asString); jerr == nil && asString != "" {
		return asString, nil
	}
	var asObj struct {
		SmartContractToken string `json:"smart_contract_token"`
		SmartContract      string `json:"smartContract"`
		Hash               string `json:"hash"`
		ID                 string `json:"id"`
	}
	if jerr := json.Unmarshal(sr.Result, &asObj); jerr == nil {
		switch {
		case asObj.SmartContractToken != "":
			return asObj.SmartContractToken, nil
		case asObj.SmartContract != "":
			return asObj.SmartContract, nil
		case asObj.Hash != "":
			return asObj.Hash, nil
		case asObj.ID != "":
			return asObj.ID, nil
		}
	}
	return "", fmt.Errorf("rubix: could not extract contract token from sign result: %s", truncate(string(sr.Result), 200))
}

// SubscribeSmartContract tells the node to join the pubsub topic for the
// given smart contract token id, enabling chain updates.
//
// Endpoint: GET /rubix/v1/smart_contracts/subscribe?smartContractToken=...
func (c *Client) SubscribeSmartContract(ctx context.Context, contractID string) error {
	q := url.Values{}
	q.Set("smartContractToken", contractID)
	_, err := c.getJSON(ctx, "/rubix/v1/smart_contracts/subscribe", q)
	return err
}

// ChainEntry is one record on the smart-contract chain.
type ChainEntry struct {
	TransactionID string `json:"transactionId"`
	Initiator     string `json:"initiator"`
	Epoch         int64  `json:"epoch"`
	Data          string `json:"data"`
}

// GetSmartContractChain returns the contract's transaction history, oldest
// first. Useful for auditing how many txs have hit a contract and what
// data was written.
//
// Endpoint: GET /rubix/v1/smart_contracts/{contract_id}/chain
func (c *Client) GetSmartContractChain(ctx context.Context, contractID string) ([]ChainEntry, error) {
	path := fmt.Sprintf("/rubix/v1/smart_contracts/%s/chain", url.PathEscape(contractID))
	br, err := c.getJSON(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	var entries []ChainEntry
	if len(br.Result) == 0 {
		return entries, nil
	}
	if jerr := json.Unmarshal(br.Result, &entries); jerr == nil {
		return entries, nil
	}
	// Fallback: some builds wrap in an object with a `chain` field.
	var wrapped struct {
		Chain []ChainEntry `json:"chain"`
	}
	if jerr := json.Unmarshal(br.Result, &wrapped); jerr == nil {
		return wrapped.Chain, nil
	}
	return nil, fmt.Errorf("rubix GetSmartContractChain: could not decode result: %s", truncate(string(br.Result), 200))
}

func attachFile(w *multipart.Writer, field, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("rubix attachFile %s: open: %w", field, err)
	}
	defer f.Close()
	part, err := w.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return fmt.Errorf("rubix attachFile %s: form file: %w", field, err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("rubix attachFile %s: copy: %w", field, err)
	}
	return nil
}
