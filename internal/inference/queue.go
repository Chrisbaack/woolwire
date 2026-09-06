package inference

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrQueueOverflow    = errors.New("host queue is full")
	ErrMemberQueueFull  = errors.New("member has reached maximum queued requests")
	ErrQueueTimeout     = errors.New("request expired while waiting in queue")
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

func normalizeLimits(l Limits) Limits {
	if l.MaxActive <= 0 {
		l.MaxActive = 1
	}
	if l.MaxQueuedPerMember <= 0 {
		l.MaxQueuedPerMember = 1
	}
	if l.MaxQueuedTotal <= 0 {
		l.MaxQueuedTotal = 10
	}
	if l.QueueTimeout <= 0 {
		l.QueueTimeout = 5 * time.Minute
	}
	if l.ExecTimeout <= 0 {
		l.ExecTimeout = 10 * time.Minute
	}
	return l
}

type queueItem struct {
	requestID string
	memberID  string
	runFn     func(ctx context.Context) error
	ready     chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc

	// dispatched is set under the queue lock when the item has been charged an
	// active slot and ready has been closed. Once set, the item must reach
	// execute() so the slot is released, even if the waiter's timeout or
	// cancellation branch won the select race.
	dispatched bool
	// removed makes dequeuing idempotent. Cancel and the waiter's abort path
	// can both reach the same item; the second one must not decrement the
	// member's queue depth a second time.
	removed bool
}

// FairQueue admits inference work under per-member and host-wide limits and
// serves waiting members round-robin rather than first-come-first-served, so
// one member with a deep queue cannot starve the others.
type FairQueue struct {
	mu     sync.Mutex
	limits Limits

	activeCount  int
	totalQueued  int
	memberQueued map[string]int
	queues       map[string][]*queueItem
	rotation     []string
	activeItems  map[string]*queueItem
	closed       bool
}

func NewFairQueue(limits Limits) *FairQueue {
	return &FairQueue{
		limits:       normalizeLimits(limits),
		memberQueued: make(map[string]int),
		queues:       make(map[string][]*queueItem),
		activeItems:  make(map[string]*queueItem),
	}
}

// SetLimits applies new host limits to subsequent admissions. Requests already
// running keep the execution timeout they started with.
func (q *FairQueue) SetLimits(limits Limits) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.limits = normalizeLimits(limits)
	q.dispatchNextLocked()
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
	if q.memberQueued[memberID] >= q.limits.MaxQueuedPerMember {
		q.mu.Unlock()
		return ErrMemberQueueFull
	}
	if q.totalQueued >= q.limits.MaxQueuedTotal {
		q.mu.Unlock()
		return ErrQueueOverflow
	}

	reqCtx, reqCancel := context.WithCancel(ctx)
	item := &queueItem{
		requestID: requestID,
		memberID:  memberID,
		runFn:     runFn,
		ready:     make(chan struct{}),
		ctx:       reqCtx,
		cancel:    reqCancel,
	}

	if q.activeCount < q.limits.MaxActive && q.totalQueued == 0 {
		q.activeCount++
		q.activeItems[requestID] = item
		item.dispatched = true
		q.mu.Unlock()
		return q.execute(item)
	}

	q.enqueueLocked(item)
	queueTimeout := q.limits.QueueTimeout
	q.mu.Unlock()

	timer := time.NewTimer(queueTimeout)
	defer timer.Stop()

	select {
	case <-item.ready:
		return q.execute(item)

	case <-timer.C:
		if q.claimAbort(item) {
			item.cancel()
			return ErrQueueTimeout
		}
		// dispatchNextLocked closed ready in the same instant; the slot is
		// already charged, so the item must run and release it.
		return q.execute(item)

	case <-item.ctx.Done():
		if q.claimAbort(item) {
			return ErrRequestCancelled
		}
		return q.execute(item)
	}
}

// claimAbort reports whether the waiter may abandon the item. It returns false
// when the item already holds an active slot, in which case the caller must
// still run it so execute() releases the slot.
func (q *FairQueue) claimAbort(item *queueItem) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if item.dispatched {
		return false
	}
	if item.removed {
		return true
	}
	q.removeLocked(item)
	return true
}

func (q *FairQueue) execute(item *queueItem) error {
	q.mu.Lock()
	execTimeout := q.limits.ExecTimeout
	q.mu.Unlock()

	execCtx, execCancel := context.WithTimeout(item.ctx, execTimeout)
	defer execCancel()

	err := item.runFn(execCtx)

	q.mu.Lock()
	delete(q.activeItems, item.requestID)
	q.activeCount--
	if q.activeCount < 0 {
		q.activeCount = 0
	}
	q.dispatchNextLocked()
	q.mu.Unlock()

	item.cancel()
	return err
}

func (q *FairQueue) enqueueLocked(item *queueItem) {
	if _, ok := q.queues[item.memberID]; !ok {
		q.rotation = append(q.rotation, item.memberID)
	}
	q.queues[item.memberID] = append(q.queues[item.memberID], item)
	q.memberQueued[item.memberID]++
	q.totalQueued++
}

func (q *FairQueue) removeLocked(target *queueItem) {
	if target.removed {
		return
	}
	target.removed = true

	items := q.queues[target.memberID]
	for i, item := range items {
		if item == target {
			q.queues[target.memberID] = append(items[:i:i], items[i+1:]...)
			break
		}
	}

	q.memberQueued[target.memberID]--
	if q.memberQueued[target.memberID] <= 0 {
		delete(q.memberQueued, target.memberID)
	}
	q.totalQueued--
	if q.totalQueued < 0 {
		q.totalQueued = 0
	}

	if len(q.queues[target.memberID]) == 0 {
		delete(q.queues, target.memberID)
		q.dropFromRotationLocked(target.memberID)
	}
}

func (q *FairQueue) dropFromRotationLocked(memberID string) {
	for i, m := range q.rotation {
		if m == memberID {
			q.rotation = append(q.rotation[:i:i], q.rotation[i+1:]...)
			return
		}
	}
}

// dispatchNextLocked hands free slots to waiting members in rotation order.
func (q *FairQueue) dispatchNextLocked() {
	for q.activeCount < q.limits.MaxActive && q.totalQueued > 0 && len(q.rotation) > 0 {
		memberID := q.rotation[0]
		items := q.queues[memberID]
		if len(items) == 0 {
			q.rotation = q.rotation[1:]
			delete(q.queues, memberID)
			continue
		}

		next := items[0]
		q.queues[memberID] = items[1:]

		q.memberQueued[memberID]--
		if q.memberQueued[memberID] <= 0 {
			delete(q.memberQueued, memberID)
		}
		q.totalQueued--

		// Rotate: this member goes to the back if it still has work waiting.
		q.rotation = q.rotation[1:]
		if len(q.queues[memberID]) > 0 {
			q.rotation = append(q.rotation, memberID)
		} else {
			delete(q.queues, memberID)
		}

		next.removed = true
		next.dispatched = true
		q.activeCount++
		q.activeItems[next.requestID] = next
		close(next.ready)
	}
}

// Cancel stops a request on behalf of the member that submitted it. A caller
// asking to cancel someone else's request is refused.
func (q *FairQueue) Cancel(requestID, memberID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if item, ok := q.activeItems[requestID]; ok {
		if item.memberID != memberID {
			return false
		}
		item.cancel()
		return true
	}

	for _, items := range q.queues {
		for _, item := range items {
			if item.requestID != requestID {
				continue
			}
			if item.memberID != memberID {
				return false
			}
			q.removeLocked(item)
			item.cancel()
			q.dispatchNextLocked()
			return true
		}
	}

	return false
}

// CancelMember drops every queued and active request belonging to a member.
// It is what makes a newly learned removal take effect on work in flight.
func (q *FairQueue) CancelMember(memberID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	cancelled := 0
	for _, item := range q.activeItems {
		if item.memberID == memberID {
			item.cancel()
			cancelled++
		}
	}
	for _, item := range append([]*queueItem(nil), q.queues[memberID]...) {
		q.removeLocked(item)
		item.cancel()
		cancelled++
	}
	q.dispatchNextLocked()
	return cancelled
}

func (q *FairQueue) Stats() (active int, queued int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.activeCount, q.totalQueued
}

func (q *FairQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	for _, item := range q.activeItems {
		item.cancel()
	}
	for _, items := range q.queues {
		for _, item := range items {
			item.cancel()
		}
	}
}
