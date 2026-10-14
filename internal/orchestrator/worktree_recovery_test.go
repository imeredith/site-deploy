package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"site-deploy/internal/model"
)

func TestWorktreeRecoveryDrainsStreamsAndCoalescesRestart(t *testing.T) {
	m, runtime := manager(t)
	ctx := context.Background()
	stream, err := m.Acquire(ctx, "example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := m.Acquire(ctx, "example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	failed.ReportFailure(errors.New("connection refused"))
	failed.Release()
	if app, _ := m.Get("example"); app.State != model.StateError || app.Upstream != "" {
		t.Fatalf("failed connection retained stale upstream: %+v", app)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	if _, err := m.Acquire(waitCtx, "example.localhost"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restarted with another live stream: %v", err)
	}
	runtime.mu.Lock()
	attempts := runtime.resumes
	runtime.mu.Unlock()
	if attempts != 0 {
		t.Fatal("reconfigured application while stream lease was active")
	}
	stream.Release()
	gate := make(chan struct{})
	runtime.resumeGate = gate
	runtime.entered = make(chan struct{}, 1)
	var group sync.WaitGroup
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			lease, err := m.Acquire(ctx, "example.localhost")
			if err != nil {
				failures <- err
				return
			}
			lease.Release()
		}()
	}
	<-runtime.entered
	close(gate)
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	runtime.mu.Lock()
	attempts = runtime.resumes
	runtime.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("concurrent recovery started %d times", attempts)
	}
	failed.ReportFailure(errors.New("late error from old request"))
	if app, _ := m.Get("example"); app.State != model.StateRunning {
		t.Fatal("late failure invalidated repaired app")
	}
}

func TestWorktreeRecoveryFailedWakeCooldownAndExplicitResume(t *testing.T) {
	m, runtime := manager(t)
	ctx := context.Background()
	if _, err := m.Suspend(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.startError = errors.New("healthcheck failed")
	runtime.mu.Unlock()
	for i := 0; i < 20; i++ {
		if _, err := m.Acquire(ctx, "example.localhost"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("failed wake response: %v", err)
		}
	}
	runtime.mu.Lock()
	attempts := runtime.resumes
	runtime.startError = nil
	runtime.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("failure burst triggered %d startups", attempts)
	}
	if _, err := m.Resume(ctx, "example"); err != nil {
		t.Fatalf("manual resume could not bypass cooldown: %v", err)
	}
	if _, err := m.Suspend(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.startError = errors.New("second failure")
	runtime.mu.Unlock()
	if _, err := m.Acquire(ctx, "example.localhost"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	e, _ := m.lookup("example")
	e.mu.Lock()
	e.retryAfter = time.Now().Add(-time.Second)
	e.mu.Unlock()
	runtime.mu.Lock()
	runtime.startError = nil
	runtime.mu.Unlock()
	lease, err := m.Acquire(ctx, "example.localhost")
	if err != nil {
		t.Fatalf("expired cooldown blocked recovery: %v", err)
	}
	lease.Release()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.resumes != 4 {
		t.Fatalf("unexpected attempt count %d", runtime.resumes)
	}
}
