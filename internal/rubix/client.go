// Package rubix is a thin HTTP client for the Rubix v2 API exposed by
// rubixgoplatform (release-v1 branch). It is intentionally small: only
// the endpoints the ymca-wellness-dapp dApp server needs.
package rubix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BasicResponse mirrors model.BasicResponse on the Rubix side: the common
// envelope used by most endpoints. `Result` is endpoint-specific and is
// decoded separately via RawResult.
type BasicResponse struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
}

// APIError is returned when Rubix responds with status=false.
type APIError struct {
	Endpoint   string
	HTTPStatus int
	Message    string
	Body       string // raw body for debugging (truncated)
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("rubix %s: %s (http %d)", e.Endpoint, e.Message, e.HTTPStatus)
	}
	return fmt.Sprintf("rubix %s: http %d: %s", e.Endpoint, e.HTTPStatus, truncate(e.Body, 200))
}

// IsAPIError reports whether err was produced by the Rubix server returning
// status=false.
func IsAPIError(err error) bool {
	var a *APIError
	return errors.As(err, &a)
}

// Client talks to one Rubix node. Create one Client per (admin DID, node)
// pair; they are cheap.
type Client struct {
	baseURL string       // e.g. "http://localhost:20000"
	http    *http.Client
}

// New builds a client bound to baseURL. Timeout is the per-request deadline
// applied on top of any ctx deadline.
func New(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// BaseURL returns the configured base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// resolve builds an absolute URL from a path and optional query.
func (c *Client) resolve(path string, query url.Values) string {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// postJSON POSTs `body` as JSON, decodes into BasicResponse, and returns
// an APIError if status=false.
func (c *Client) postJSON(ctx context.Context, path string, body any) (*BasicResponse, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, fmt.Errorf("rubix encode %s: %w", path, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve(path, nil), &buf)
	if err != nil {
		return nil, fmt.Errorf("rubix request %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, path)
}

// getJSON GETs the path with optional query, decodes into BasicResponse.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values) (*BasicResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve(path, query), nil)
	if err != nil {
		return nil, fmt.Errorf("rubix request %s: %w", path, err)
	}
	return c.do(req, path)
}

func (c *Client) do(req *http.Request, path string) (*BasicResponse, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rubix call %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rubix read %s: %w", path, err)
	}
	var br BasicResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &br); err != nil {
			return nil, &APIError{
				Endpoint:   path,
				HTTPStatus: resp.StatusCode,
				Message:    "non-JSON response",
				Body:       string(raw),
			}
		}
	}
	if !br.Status {
		return &br, &APIError{
			Endpoint:   path,
			HTTPStatus: resp.StatusCode,
			Message:    br.Message,
			Body:       string(raw),
		}
	}
	return &br, nil
}

// decodeResult JSON-decodes br.Result into out; if Result is a bare string,
// out must be *string.
func decodeResult(br *BasicResponse, out any) error {
	if len(br.Result) == 0 {
		return nil
	}
	return json.Unmarshal(br.Result, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
