package inference

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestQueueSlotIsNeverLeaked is the regression for the dispatch/timeout race:
// when dispatchNextLocked closed ready at the same instant the timeout or
// cancel branch fired, Go's select could pick the abort branch on an item that
// had already been charged an active slot, and the host stopped serving.
func TestQueueSlotIsNeverLeaked(t *testing.T) {
	q := NewFairQueue(Limits{
		MaxActive:          2,
		MaxQueuedPerMember: 50,
		MaxQueuedTotal:     500,
		// A very short queue timeout maximizes the chance of racing the
		// dispatch that hands the item its slot.
		QueueTimeout: time.Millisecond,
		ExecTimeout:  5 * time.Second,
	})

	const (
		members  = 8
		perMember = 250
	)

	var wg sync.WaitGroup
	var completed atomic.Int64

	for m := 0; m < members; m++ {
		memberID := fmt.Sprintf("m-%d", m)
		for i := 0; i < perMember; i++ {
			wg.Add(1)
			requestID := fmt.Sprintf("%s-req-%d", memberID, i)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				// Half the submissions cancel themselves almost immediately,
				// so cancel, timeout, and dispatch all collide.
				if i%2 == 0 {
					go func() {
						time.Sleep(time.Duration(i%3) * time.Millisecond)
						cancel()
					}()
				}
				go func() {
					time.Sleep(time.Duration(i%5) * time.Millisecond)
					q.Cancel(requestID, memberID)
				}()

				_ = q.Submit(ctx, memberID, requestID, func(execCtx context.Context) error {
					completed.Add(1)
					return nil
				})
			}()
		}
	}

	wg.Wait()

	// Everything has returned, so the queue must be completely idle. A leaked
	// slot shows up here as a non-zero active count.
	deadline := time.Now().Add(5 * time.Second)
	for {
		active, queued := q.Stats()
		if active == 0 && queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue did not drain: active=%d queued=%d (leaked slot)", active, queued)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The queue must still serve after all that churn.
	ran := make(chan struct{})
	if err := q.Submit(context.Background(), "m-after", "req-after", func(context.Context) error {
		close(ran)
		return nil
	}); err != nil {
		t.Fatalf("queue refused work after the stress run: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("queue stopped serving after the stress run")
	}
}

// TestCancelIsIdempotent is the regression for double-decrementing
// memberQueued: Cancel on a queued item also woke ctx.Done() in Submit, which
// dequeued the same item a second time.
func TestCancelIsIdempotent(t *testing.T) {
	q := NewFairQueue(Limits{
		MaxActive:          1,
		MaxQueuedPerMember: 4,
		MaxQueuedTotal:     10,
		QueueTimeout:       5 * time.Second,
		ExecTimeout:        5 * time.Second,
	})

	hold := make(chan struct{})
	running := make(chan struct{})
	go func() {
		_ = q.Submit(context.Background(), "m-a", "active", func(context.Context) error {
			close(running)
			<-hold
			return nil
		})
	}()
	<-running

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		queued <- q.Submit(ctx, "m-a", "queued", func(context.Context) error { return nil })
	}()

	// Wait for it to be in the queue.
	waitFor(t, func() bool { _, n := q.Stats(); return n == 1 })

	// Cancel through both paths at once.
	q.Cancel("queued", "m-a")
	cancel()

	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled submission never returned")
	}

	close(hold)
	waitFor(t, func() bool { a, n := q.Stats(); return a == 0 && n == 0 })

	// The member must be able to queue its full allowance again. A double
	// decrement would have left the counter negative and, worse, corrupted
	// the total.
	for i := 0; i < 4; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = q.Submit(context.Background(), "m-a", fmt.Sprintf("again-%d", i), func(context.Context) error {
				return nil
			})
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("queue wedged after an idempotent cancel")
		}
	}
}

// TestCancelRefusesOtherMembersRequests covers task 5.
func TestCancelRefusesOtherMembersRequests(t *testing.T) {
	q := NewFairQueue(DefaultLimits())

	running := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- q.Submit(context.Background(), "m-owner", "req-1", func(execCtx context.Context) error {
			close(running)
			select {
			case <-release:
			case <-execCtx.Done():
			}
			return execCtx.Err()
		})
	}()
	<-running

	if q.Cancel("req-1", "m-attacker") {
		t.Fatal("a member cancelled another member's active request")
	}
	select {
	case <-done:
		t.Fatal("the request was stopped by an unrelated member")
	case <-time.After(100 * time.Millisecond):
	}

	if !q.Cancel("req-1", "m-owner") {
		t.Fatal("the owner could not cancel its own request")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the owner's cancel had no effect")
	}
	close(release)
}

// TestRoundRobinFairness covers the documented fairness rule. FIFO let a
// member with a deeper allowance monopolize the host.
func TestRoundRobinFairness(t *testing.T) {
	q := NewFairQueue(Limits{
		MaxActive:          1,
		MaxQueuedPerMember: 10,
		MaxQueuedTotal:     50,
		QueueTimeout:       10 * time.Second,
		ExecTimeout:        10 * time.Second,
	})

	// Occupy the single active slot so everything else queues behind it.
	blocking := make(chan struct{})
	running := make(chan struct{})
	go func() {
		_ = q.Submit(context.Background(), "m-blocker", "blocker", func(context.Context) error {
			close(running)
			<-blocking
			return nil
		})
	}()
	<-running

	var order []string
	var orderMu sync.Mutex
	var wg sync.WaitGroup

	// Greedy queues six requests first; polite queues two afterwards. Under
	// FIFO, polite would wait behind all six.
	submit := func(memberID string, n int) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			id := fmt.Sprintf("%s-%d", memberID, i)
			go func() {
				defer wg.Done()
				_ = q.Submit(context.Background(), memberID, id, func(context.Context) error {
					orderMu.Lock()
					order = append(order, memberID)
					orderMu.Unlock()
					return nil
				})
			}()
			waitFor(t, func() bool {
				_, queued := q.Stats()
				return queued >= i+1 || memberID != "m-greedy"
			})
		}
	}

	submit("m-greedy", 6)
	waitFor(t, func() bool { _, queued := q.Stats(); return queued == 6 })
	submit("m-polite", 2)
	waitFor(t, func() bool { _, queued := q.Stats(); return queued == 8 })

	close(blocking)
	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()

	// The polite member's first request must not be stuck behind all six.
	firstPolite := -1
	for i, m := range order {
		if m == "m-polite" {
			firstPolite = i
			break
		}
	}
	if firstPolite < 0 {
		t.Fatalf("the polite member was never served: %v", order)
	}
	if firstPolite > 2 {
		t.Fatalf("round-robin did not interleave members; polite served at position %d in %v",
			firstPolite, order)
	}
}

func TestCancelMemberDropsEverythingTheyHold(t *testing.T) {
	q := NewFairQueue(Limits{
		MaxActive:          1,
		MaxQueuedPerMember: 5,
		MaxQueuedTotal:     20,
		QueueTimeout:       10 * time.Second,
		ExecTimeout:        10 * time.Second,
	})

	running := make(chan struct{})
	activeDone := make(chan error, 1)
	go func() {
		activeDone <- q.Submit(context.Background(), "m-removed", "active", func(execCtx context.Context) error {
			close(running)
			<-execCtx.Done()
			return execCtx.Err()
		})
	}()
	<-running

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		id := fmt.Sprintf("queued-%d", i)
		go func() {
			defer wg.Done()
			_ = q.Submit(context.Background(), "m-removed", id, func(context.Context) error { return nil })
		}()
	}
	waitFor(t, func() bool { _, queued := q.Stats(); return queued == 3 })

	if n := q.CancelMember("m-removed"); n != 4 {
		t.Fatalf("expected 4 cancellations, got %d", n)
	}

	select {
	case <-activeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the removed member's active request kept running")
	}
	wg.Wait()

	waitFor(t, func() bool { a, n := q.Stats(); return a == 0 && n == 0 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
