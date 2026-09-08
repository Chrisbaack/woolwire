package inference

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestFairQueueExecutionAndLimits(t *testing.T) {
	limits := Limits{
		MaxActive:          1,
		MaxQueuedPerMember: 1,
		MaxQueuedTotal:     2,
		QueueTimeout:       2 * time.Second,
		ExecTimeout:        2 * time.Second,
	}
	q := NewFairQueue(limits)

	startedFirst := make(chan struct{})
	finishFirst := make(chan struct{})

	// Submit first request from member A (executes immediately)
	go func() {
		_ = q.Submit(context.Background(), "member-a", "req-1", func(ctx context.Context) error {
			close(startedFirst)
			<-finishFirst
			return nil
		})
	}()

	<-startedFirst

	// Member A queues a request (now waiting in queue)
	req2Started := make(chan struct{})
	go func() {
		_ = q.Submit(context.Background(), "member-a", "req-2", func(ctx context.Context) error {
			close(req2Started)
			return nil
		})
	}()

	time.Sleep(20 * time.Millisecond)

	// Member A tries to queue a second waiting request -> must fail with ErrMemberQueueFull
	err := q.Submit(context.Background(), "member-a", "req-3", func(ctx context.Context) error {
		return nil
	})
	if err != ErrMemberQueueFull {
		t.Fatalf("expected ErrMemberQueueFull, got %v", err)
	}

	// Member B queues request -> should succeed in waiting
	var wg sync.WaitGroup
	wg.Add(1)
	secondFinished := make(chan struct{})
	go func() {
		defer wg.Done()
		err := q.Submit(context.Background(), "member-b", "req-4", func(ctx context.Context) error {
			close(secondFinished)
			return nil
		})
		if err != nil {
			t.Errorf("member B request failed: %v", err)
		}
	}()

	time.Sleep(20 * time.Millisecond)
	active, queued := q.Stats()
	if active != 1 || queued != 2 {
		t.Fatalf("expected 1 active and 2 queued, got active=%d queued=%d", active, queued)
	}

	// Unblock first request
	close(finishFirst)

	// Second request should now execute
	select {
	case <-secondFinished:
	case <-time.After(time.Second):
		t.Fatal("second request did not execute after first finished")
	}

	wg.Wait()
}

func TestFairQueueCancellation(t *testing.T) {
	q := NewFairQueue(DefaultLimits())

	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		_ = q.Submit(ctx, "member-x", "req-cancel", func(execCtx context.Context) error {
			close(started)
			<-execCtx.Done()
			return execCtx.Err()
		})
	}()

	<-started
	// Cancel the request
	cancel()

	time.Sleep(20 * time.Millisecond)
	active, queued := q.Stats()
	if active != 0 || queued != 0 {
		t.Fatalf("cancelled request did not release active slot, got active=%d queued=%d", active, queued)
	}
}

func TestDuplicateRequestIDsAndCancelMember(t *testing.T) {
	limits := Limits{
		MaxActive:          2,
		MaxQueuedPerMember: 5,
		MaxQueuedTotal:     10,
		QueueTimeout:       5 * time.Second,
		ExecTimeout:        5 * time.Second,
	}
	q := NewFairQueue(limits)

	startedA1 := make(chan struct{})
	startedB1 := make(chan struct{})
	finishB1 := make(chan struct{})

	// Member A submits req-1 (takes active slot 1)
	go func() {
		_ = q.Submit(context.Background(), "member-a", "shared-req-id", func(execCtx context.Context) error {
			close(startedA1)
			<-execCtx.Done()
			return execCtx.Err()
		})
	}()

	// Member B submits shared-req-id (same ID, different member! takes active slot 2)
	go func() {
		_ = q.Submit(context.Background(), "member-b", "shared-req-id", func(execCtx context.Context) error {
			close(startedB1)
			<-finishB1
			return nil
		})
	}()

	<-startedA1
	<-startedB1

	// Member A submits shared-req-id again while active -> must be rejected
	if err := q.Submit(context.Background(), "member-a", "shared-req-id", func(ctx context.Context) error {
		return nil
	}); err != ErrDuplicateRequest {
		t.Fatalf("expected ErrDuplicateRequest for member A active ID, got %v", err)
	}

	// Member A submits req-2 (queued)
	reqA2Ready := make(chan struct{})
	go func() {
		_ = q.Submit(context.Background(), "member-a", "queued-req-id", func(ctx context.Context) error {
			close(reqA2Ready)
			return nil
		})
	}()

	time.Sleep(20 * time.Millisecond)

	// Member A submits queued-req-id again while queued -> must be rejected
	if err := q.Submit(context.Background(), "member-a", "queued-req-id", func(ctx context.Context) error {
		return nil
	}); err != ErrDuplicateRequest {
		t.Fatalf("expected ErrDuplicateRequest for member A queued ID, got %v", err)
	}

	active, queued := q.Stats()
	if active != 2 || queued != 1 {
		t.Fatalf("expected 2 active and 1 queued, got active=%d queued=%d", active, queued)
	}

	// Cancel member A (simulating member removal)
	cancelled := q.CancelMember("member-a")
	if cancelled != 2 {
		t.Fatalf("expected 2 requests cancelled for member A, got %d", cancelled)
	}

	time.Sleep(20 * time.Millisecond)

	// Member B must STILL be running!
	active, queued = q.Stats()
	if active != 1 || queued != 0 {
		t.Fatalf("expected member B to still hold active slot, got active=%d queued=%d", active, queued)
	}

	// Complete member B
	close(finishB1)
	time.Sleep(20 * time.Millisecond)

	active, queued = q.Stats()
	if active != 0 || queued != 0 {
		t.Fatalf("expected queue accounting to return to 0, got active=%d queued=%d", active, queued)
	}
}
