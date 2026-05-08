// Package queue implements a per-admin job queue for reward transfers.
//
// Each admin gets its own buffered channel and its own worker goroutine.
// Jobs for different admins run in parallel; jobs for the same admin run
// serially (which is what the Rubix node expects — the admin's signing
// key is a single resource).
package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/service"
)

// TransferJob is one queued reward transfer.
type TransferJob struct {
	RequestID string
	Input     service.TransferRewardInput
	EnqueuedAt time.Time
}

// ErrQueueFull is returned when an admin's queue is at capacity.
var ErrQueueFull = errors.New("queue: admin queue is full")

type adminQueue struct {
	adminDID string
	ch       chan *TransferJob
	once     sync.Once
}

// Manager owns per-admin queues. Safe for concurrent use.
type Manager struct {
	svc        *service.Service
	bufferSize int
	procTimeout time.Duration

	mu     sync.RWMutex
	queues map[string]*adminQueue
}

// NewManager builds a Manager. bufferSize is the per-admin channel
// capacity (1000 in the default config). procTimeout caps how long a
// single job can run end-to-end; it should exceed the Rubix HTTP client
// timeout.
func NewManager(svc *service.Service, bufferSize int, procTimeout time.Duration) *Manager {
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	if procTimeout <= 0 {
		procTimeout = 5 * time.Minute
	}
	return &Manager{
		svc:         svc,
		bufferSize:  bufferSize,
		procTimeout: procTimeout,
		queues:      make(map[string]*adminQueue),
	}
}

// Enqueue submits a job for the admin. Non-blocking: returns ErrQueueFull
// if the buffer is saturated.
func (m *Manager) Enqueue(job *TransferJob) error {
	if job == nil || job.Input.AdminDID == "" {
		return fmt.Errorf("queue.Enqueue: job missing admin_did")
	}
	q := m.getOrCreate(job.Input.AdminDID)
	select {
	case q.ch <- job:
		return nil
	default:
		return fmt.Errorf("%w: admin=%s capacity=%d", ErrQueueFull, job.Input.AdminDID, m.bufferSize)
	}
}

// Snapshot returns a point-in-time view of per-admin queue depth. Used
// by /api/queue/metrics.
func (m *Manager) Snapshot() (total int, perAdmin map[string]int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	perAdmin = make(map[string]int, len(m.queues))
	for did, q := range m.queues {
		n := len(q.ch)
		perAdmin[did] = n
		total += n
	}
	return total, perAdmin
}

// getOrCreate lazily instantiates the admin's queue and starts its worker.
func (m *Manager) getOrCreate(adminDID string) *adminQueue {
	m.mu.RLock()
	if q, ok := m.queues[adminDID]; ok {
		m.mu.RUnlock()
		return q
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if q, ok := m.queues[adminDID]; ok {
		return q
	}
	q := &adminQueue{
		adminDID: adminDID,
		ch:       make(chan *TransferJob, m.bufferSize),
	}
	m.queues[adminDID] = q
	q.once.Do(func() {
		go m.worker(q)
	})
	return q
}

// worker consumes jobs for a single admin.
func (m *Manager) worker(q *adminQueue) {
	log.Printf("queue[%s]: worker started (buffer=%d)", q.adminDID, m.bufferSize)
	for job := range q.ch {
		m.processOne(job)
	}
}

// processOne runs one job end-to-end, updating transfer_status along the
// way. Errors are logged and recorded in the status row — they do not
// propagate; the goroutine must keep consuming.
func (m *Manager) processOne(job *TransferJob) {
	ctx, cancel := context.WithTimeout(context.Background(), m.procTimeout)
	defer cancel()

	// Mark processing.
	if err := database.UpdateTransferStatus(ctx, job.RequestID, map[string]any{
		"status": database.StatusProcessing,
	}); err != nil {
		log.Printf("queue[%s] req=%s: mark processing failed: %v", job.Input.AdminDID, job.RequestID, err)
	}

	res, err := m.svc.ProcessTransferReward(ctx, job.Input)
	if err != nil {
		log.Printf("queue[%s] req=%s: transfer failed: %v", job.Input.AdminDID, job.RequestID, err)
		_ = database.UpdateTransferStatus(ctx, job.RequestID, map[string]any{
			"status":        database.StatusFailed,
			"message":       "reward transfer failed",
			"error_details": err.Error(),
		})
		return
	}

	updates := map[string]any{
		"status":        database.StatusSuccess,
		"message":       fmt.Sprintf("transferred %d %s to %s", res.RewardPoints, m.svc.Cfg.Env.FTName, job.Input.UserDID),
		"reward_points": res.RewardPoints,
	}
	if res.TransactionID != "" {
		updates["transaction_id"] = res.TransactionID
	}
	if res.ContractHash != "" {
		updates["contract_hash"] = res.ContractHash
	}
	if err := database.UpdateTransferStatus(ctx, job.RequestID, updates); err != nil {
		log.Printf("queue[%s] req=%s: mark success failed: %v", job.Input.AdminDID, job.RequestID, err)
	}
	log.Printf("queue[%s] req=%s: success tx=%s points=%d",
		job.Input.AdminDID, job.RequestID, res.TransactionID, res.RewardPoints)
}
