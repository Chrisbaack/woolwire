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
