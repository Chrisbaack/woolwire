package inference

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrQueueOverflow   = errors.New("host queue is full")
	ErrMemberQueueFull = errors.New("member has reached maximum queued requests")
	ErrQueueTimeout    = errors.New("request expired while waiting in queue")
	ErrRequestCancelled = errors.New("request was cancelled")
)

type Limits struct {
	MaxActive          int
	MaxQueuedPerMember int
	MaxQueuedTotal     int
	QueueTimeout       time.Duration
	ExecTimeout        time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxActive:          1,
		MaxQueuedPerMember: 1,
		MaxQueuedTotal:     10,
		QueueTimeout:       5 * time.Minute,
		ExecTimeout:        10 * time.Minute,
	}
}

type queueItem struct {
	requestID string
	memberID  string
	runFn     func(ctx context.Context) error
	ready     chan struct{}
	errCh     chan error
	ctx       context.Context
	cancel    context.CancelFunc
}

type FairQueue struct {
	mu     sync.Mutex
	limits Limits

	activeCount  int
	memberQueued map[string]int
	queue        []*queueItem
	activeItems  map[string]*queueItem
	closed       bool
}

func NewFairQueue(limits Limits) *FairQueue {
	if limits.MaxActive <= 0 {
		limits.MaxActive = 1
	}
	if limits.MaxQueuedPerMember <= 0 {
		limits.MaxQueuedPerMember = 1
	}
	if limits.MaxQueuedTotal <= 0 {
		limits.MaxQueuedTotal = 10
	}
	if limits.QueueTimeout <= 0 {
		limits.QueueTimeout = 5 * time.Minute
	}
	if limits.ExecTimeout <= 0 {
		limits.ExecTimeout = 10 * time.Minute
	}

	return &FairQueue{
		limits:       limits,
		memberQueued: make(map[string]int),
		activeItems:  make(map[string]*queueItem),
	}
}

func (q *FairQueue) Submit(
	ctx context.Context,
	memberID string,
	requestID string,
	runFn func(execCtx context.Context) error,
) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return errors.New("queue is closed")
	}

	// Quota checks
	if q.memberQueued[memberID] >= q.limits.MaxQueuedPerMember {
		q.mu.Unlock()
		return ErrMemberQueueFull
	}
	if len(q.queue) >= q.limits.MaxQueuedTotal {
		q.mu.Unlock()
		return ErrQueueOverflow
	}

	reqCtx, reqCancel := context.WithCancel(ctx)
	item := &queueItem{
		requestID: requestID,
		memberID:  memberID,
		runFn:     runFn,
		ready:     make(chan struct{}),
		errCh:     make(chan error, 1),
		ctx:       reqCtx,
		cancel:    reqCancel,
	}

	q.memberQueued[memberID]++

	// Immediate execution if active slots available and queue is empty
	if q.activeCount < q.limits.MaxActive && len(q.queue) == 0 {
		q.activeCount++
		q.activeItems[requestID] = item
		q.memberQueued[memberID]--
		q.mu.Unlock()

		return q.execute(item)
	}

	q.queue = append(q.queue, item)
	q.mu.Unlock()

	// Wait in queue
	select {
	case <-item.ready:
		return q.execute(item)

	case <-time.After(q.limits.QueueTimeout):
		q.dequeue(item)
		item.cancel()
		return ErrQueueTimeout

	case <-item.ctx.Done():
		q.dequeue(item)
		return ErrRequestCancelled
	}
}

func (q *FairQueue) execute(item *queueItem) error {
	execCtx, execCancel := context.WithTimeout(item.ctx, q.limits.ExecTimeout)
	defer execCancel()

	err := item.runFn(execCtx)

	q.mu.Lock()
	delete(q.activeItems, item.requestID)
	q.activeCount--
	q.dispatchNextLocked()
	q.mu.Unlock()

	return err
}

func (q *FairQueue) dequeue(target *queueItem) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.memberQueued[target.memberID]--
	if q.memberQueued[target.memberID] <= 0 {
		delete(q.memberQueued, target.memberID)
	}

	for i, item := range q.queue {
		if item == target {
			q.queue = append(q.queue[:i], q.queue[i+1:]...)
			break
		}
	}
}

func (q *FairQueue) dispatchNextLocked() {
	if q.activeCount >= q.limits.MaxActive || len(q.queue) == 0 {
		return
	}

	// Pop next item
	next := q.queue[0]
	q.queue = q.queue[1:]

	q.memberQueued[next.memberID]--
	if q.memberQueued[next.memberID] <= 0 {
		delete(q.memberQueued, next.memberID)
	}

	q.activeCount++
	q.activeItems[next.requestID] = next
	close(next.ready)
}

func (q *FairQueue) Cancel(requestID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if item, ok := q.activeItems[requestID]; ok {
		item.cancel()
		return true
	}

	for i, item := range q.queue {
		if item.requestID == requestID {
			item.cancel()
			q.queue = append(q.queue[:i], q.queue[i+1:]...)
			q.memberQueued[item.memberID]--
			if q.memberQueued[item.memberID] <= 0 {
				delete(q.memberQueued, item.memberID)
			}
			return true
		}
	}

	return false
}

func (q *FairQueue) Stats() (active int, queued int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.activeCount, len(q.queue)
}
