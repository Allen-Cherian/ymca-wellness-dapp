package rubix

import (
	"fmt"
	"sync"
	"time"
)

// Pool maintains one *Client per admin base URL. It is safe for concurrent
// use.
type Pool struct {
	mu      sync.RWMutex
	clients map[string]*Client // key = base URL
	timeout time.Duration
}

// NewPool creates an empty pool. timeout is used when building new clients.
func NewPool(timeout time.Duration) *Pool {
	return &Pool{
		clients: make(map[string]*Client),
		timeout: timeout,
	}
}

// ForBaseURL returns (creating if needed) a client for the given base URL.
func (p *Pool) ForBaseURL(baseURL string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("rubix: empty base URL")
	}
	p.mu.RLock()
	if c, ok := p.clients[baseURL]; ok {
		p.mu.RUnlock()
		return c, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[baseURL]; ok {
		return c, nil
	}
	c := New(baseURL, p.timeout)
	p.clients[baseURL] = c
	return c, nil
}
