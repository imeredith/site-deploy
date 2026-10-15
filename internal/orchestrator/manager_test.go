package orchestrator

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"site-deploy/internal/engine"
	"site-deploy/internal/model"
)

type fakeRuntime struct {
	mu                                      sync.Mutex
	ups, resumes, suspends, downs, inspects int
	resumeGate                              <-chan struct{}
	entered                                 chan struct{}
	startError                              error
	status                                  engine.Status
	// volumes holds the Docker names of existing volumes.
	volumes    map[string]bool
	copies     []volumeCopy
	copyGate   <-chan struct{}
	copyError  error
	copyPaused bool // a failed copy leaves the source paused
}

type volumeCopy struct {
	source, target string
	volumes        []string
}

func (f *fakeRuntime) Up(ctx context.Context, a model.App) (string, error) {
	f.mu.Lock()
	f.ups++
	err := f.startError
	f.mu.Unlock()
	return "127.0.0.1:12345", err
}
func (f *fakeRuntime) Resume(ctx context.Context, a model.App) (string, error) {
	f.mu.Lock()
	f.resumes++
	gate, entered, err := f.resumeGate, f.entered, f.startError
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "127.0.0.1:12345", err
}
func (f *fakeRuntime) Suspend(context.Context, model.App) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspends++
	return nil
}
func (f *fakeRuntime) Down(context.Context, model.App) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downs++
	return nil
}
func (f *fakeRuntime) Inspect(context.Context, model.App) (engine.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	return f.status, nil
}
func (f *fakeRuntime) Logs(context.Context, model.App, int) (string, error) { return "hello\n", nil }
func (f *fakeRuntime) ExistingVolumes(_ context.Context, a model.App, volumes []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var existing []string
	for _, volume := range volumes {
		if f.volumes[a.Project+"_"+volume] {
			existing = append(existing, a.Project+"_"+volume)
		}
	}
	return existing, nil
}
func (f *fakeRuntime) CopyVolumes(ctx context.Context, source, target model.App, volumes []string) error {
	f.mu.Lock()
	gate, err := f.copyGate, f.copyError
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		if f.copyPaused {
			f.status = engine.Status{Partial: true}
		}
		return err
	}
	f.copies = append(f.copies, volumeCopy{source: source.Name, target: target.Name, volumes: volumes})
	for _, volume := range volumes {
		f.volumes[target.Project+"_"+volume] = true
	}
	return nil
}

func bundle(t *testing.T, domain string) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "app-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	config := fmt.Sprintf("x-site-deploy:\n  domain: %s\n  service: web\n  port: 80\n  idle_timeout: 1s\nservices:\n  web:\n    image: nginx:alpine\n    volumes:\n      - ./public:/usr/share/nginx/html:ro\n", domain)
	for path, body := range map[string]string{"compose.yaml": config, "public/index.html": "hello"} {
		entry, err := w.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func manager(t *testing.T) (*Manager, *fakeRuntime) {
	t.Helper()
	runtime := &fakeRuntime{}
	m, err := New(t.TempDir(), runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if _, err := m.Deploy(context.Background(), "example", bundle(t, "example.localhost")); err != nil {
		t.Fatal(err)
	}
	return m, runtime
}

func TestConcurrentWakeSharesStartupAndHoldsLeases(t *testing.T) {
	m, runtime := manager(t)
	ctx := context.Background()
	if _, err := m.Suspend(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	runtime.resumeGate = gate
	runtime.entered = make(chan struct{}, 1)
	const count = 30
	results := make(chan *Lease, count)
	failures := make(chan error, count)
	var launched sync.WaitGroup
	launched.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			launched.Done()
			lease, err := m.Acquire(ctx, "EXAMPLE.LOCALHOST:443")
			if err != nil {
				failures <- err
				return
			}
			results <- lease
		}()
	}
	launched.Wait()
	<-runtime.entered
	close(gate)
	leases := make([]*Lease, 0, count)
	for i := 0; i < count; i++ {
		select {
		case lease := <-results:
			leases = append(leases, lease)
		case err := <-failures:
			t.Fatal(err)
		case <-time.After(3 * time.Second):
			t.Fatal("wake stuck")
		}
	}
	runtime.mu.Lock()
	resumes := runtime.resumes
	runtime.mu.Unlock()
	if resumes != 1 {
		t.Fatalf("started %d times", resumes)
	}
	if _, err := m.Suspend(ctx, "example"); !errors.Is(err, ErrBusy) {
		t.Fatalf("suspended active app: %v", err)
	}
	for _, lease := range leases {
		lease.Release()
		lease.Release()
	}
	if _, err := m.Suspend(ctx, "example"); err != nil {
		t.Fatal(err)
	}
}

func TestDisconnectedRequestDoesNotCancelSharedStartup(t *testing.T) {
	m, runtime := manager(t)
	if _, err := m.Suspend(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	runtime.resumeGate = gate
	runtime.entered = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := m.Acquire(ctx, "example.localhost"); result <- err }()
	<-runtime.entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	close(gate)
	lease, err := m.Acquire(context.Background(), "example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.resumes != 1 {
		t.Fatalf("startup was restarted %d times", runtime.resumes)
	}
}

func TestIdleUsesLastCompletedRequestAndCanBeDisabled(t *testing.T) {
	m, runtime := manager(t)
	ctx := context.Background()
	e, _ := m.lookup("example")
	lease, err := m.Acquire(ctx, "example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.app.LastActivity = time.Now().Add(-time.Hour)
	e.mu.Unlock()
	if _, err := m.suspend(ctx, e, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("idle stopped active request: %v", err)
	}
	lease.Release()
	if _, err := m.suspend(ctx, e, true); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	stops := runtime.suspends
	runtime.mu.Unlock()
	if stops != 0 {
		t.Fatal("immediate idle stop after lease completion")
	}
	e.mu.Lock()
	e.app.LastActivity = time.Now().Add(-time.Hour)
	e.app.Spec.IdleTimeout = "0"
	e.mu.Unlock()
	if _, err := m.suspend(ctx, e, true); err != nil {
		t.Fatal(err)
	}
	if snapshot(e).State != model.StateRunning {
		t.Fatal("idle zero did not disable suspension")
	}
	e.mu.Lock()
	e.app.Spec.IdleTimeout = "1s"
	e.mu.Unlock()
	if _, err := m.suspend(ctx, e, true); err != nil {
		t.Fatal(err)
	}
	if snapshot(e).State != model.StateSuspended {
		t.Fatal("idle app not suspended")
	}
}

func TestConflictAndDeletePreserveIdentityRules(t *testing.T) {
	m, runtime := manager(t)
	ctx := context.Background()
	for _, name := range []string{"../bad", "Bad", "--flag"} {
		if _, err := m.Deploy(ctx, name, bundle(t, "bad.localhost")); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if _, err := m.Deploy(ctx, "another", bundle(t, "example.localhost")); !errors.Is(err, ErrConflict) {
		t.Fatalf("domain conflict: %v", err)
	}
	if _, err := m.Acquire(ctx, "unknown.localhost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown route: %v", err)
	}
	if err := m.Delete(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	if len(m.List()) != 0 || len(m.Domains()) != 0 {
		t.Fatal("deleted app retained")
	}
	if _, err := os.Stat(filepath.Join(m.directory, "apps", "example")); !os.IsNotExist(err) {
		t.Fatalf("files retained: %v", err)
	}
	if _, err := m.Deploy(ctx, "example", bundle(t, "example.localhost")); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.downs != 1 {
		t.Fatalf("delete count %d", runtime.downs)
	}
}

func TestBundleRootSupportsNonRootImagesButHostParentStaysPrivate(t *testing.T) {
	m, _ := manager(t)
	for path, want := range map[string]os.FileMode{filepath.Join(m.directory, "apps", "example"): 0755, filepath.Join(m.directory, "apps"): 0700, filepath.Join(m.directory, "staging"): 0700} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s permissions %o; want %o", path, info.Mode().Perm(), want)
		}
	}
}

func TestRestartReconcilesDockerAndFailedStartupCanRetry(t *testing.T) {
	dir := t.TempDir()
	runtime := &fakeRuntime{startError: errors.New("image pull failed")}
	m, err := New(dir, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	app, err := m.Deploy(context.Background(), "example", bundle(t, "example.localhost"))
	if !errors.Is(err, ErrUnavailable) || app.State != model.StateError {
		t.Fatalf("failed deployment state: %+v %v", app, err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.startError = nil
	runtime.status = engine.Status{Suspended: true}
	m, err = New(dir, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	app, err = m.Get("example")
	if err != nil || app.State != model.StateSuspended {
		t.Fatalf("recovery: %+v %v", app, err)
	}
	lease, err := m.Acquire(context.Background(), "example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.inspects != 1 || runtime.resumes != 1 {
		t.Fatalf("did not reconcile/wake: %+v", runtime)
	}
}

func TestExplicitResumeReconcilesOnlyWithoutActiveRequests(t *testing.T) {
	m, runtime := manager(t)
	ctx := context.Background()
	lease, err := m.Acquire(ctx, "example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	resumes := runtime.resumes
	runtime.mu.Unlock()
	if resumes != 0 {
		t.Fatal("explicit resume reconfigured an actively serving application")
	}
	lease.Release()
	if _, err := m.Resume(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.resumes != 1 {
		t.Fatal("explicit resume did not reconcile potentially stopped containers")
	}
}

func TestFailedStartupStopsPartiallyCreatedContainers(t *testing.T) {
	runtime := &fakeRuntime{startError: errors.New("database healthy but web healthcheck failed")}
	m, err := New(t.TempDir(), runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	app, err := m.Deploy(context.Background(), "example", bundle(t, "example.localhost"))
	if !errors.Is(err, ErrUnavailable) || app.State != model.StateError {
		t.Fatalf("failed startup: %+v %v", app, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.suspends != 1 {
		t.Fatal("partially started containers were not stopped")
	}
}

func TestRestartStopsPartiallyRunningProject(t *testing.T) {
	dir := t.TempDir()
	runtime := &fakeRuntime{}
	m, err := New(dir, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(context.Background(), "example", bundle(t, "example.localhost")); err != nil {
		m.Close()
		t.Fatal(err)
	}
	m.Close()
	runtime.status = engine.Status{Partial: true}
	m, err = New(dir, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	app, err := m.Get("example")
	if err != nil || app.State != model.StateSuspended {
		t.Fatalf("partial recovery: %+v %v", app, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.suspends != 1 {
		t.Fatal("partially running project escaped cleanup on restart")
	}
}

func TestFailedFileDeletionCanRetryAfterRestartWithoutComposeFile(t *testing.T) {
	dir := t.TempDir()
	runtime := &fakeRuntime{}
	m, err := New(dir, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	app, err := m.Deploy(context.Background(), "example", bundle(t, "example.localhost"))
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	m.removeFiles = func(string) error { return errors.New("root-owned uploaded directory cannot be removed") }
	if err := m.Delete(context.Background(), "example"); err == nil {
		m.Close()
		t.Fatal("failed deletion reported success")
	}
	if got, _ := m.Get("example"); got.State != model.StateDeleting {
		m.Close()
		t.Fatalf("deletion was not retained: %+v", got)
	}
	if _, err := m.Acquire(context.Background(), "example.localhost"); !errors.Is(err, ErrUnavailable) {
		m.Close()
		t.Fatalf("partly deleted app accepted traffic: %v", err)
	}
	if err := os.Remove(app.ComposeFile); err != nil {
		m.Close()
		t.Fatal(err)
	}
	m.Close()
	m, err = New(dir, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Delete(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	if len(m.List()) != 0 {
		t.Fatal("successful deletion retry retained record")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.downs != 1 || runtime.inspects != 0 {
		t.Fatalf("deletion retry used missing Compose file: downs=%d inspects=%d", runtime.downs, runtime.inspects)
	}
}

func TestImageCleanupWaitsForStartsAndSeesDeployedImages(t *testing.T) {
	m, runtime := manager(t)
	references, err := m.ImageReferences()
	if err != nil || !reflect.DeepEqual(references, map[string][]string{"nginx:alpine": {"example"}}) {
		t.Fatalf("ImageReferences = %v, %v", references, err)
	}
	if _, err := m.Suspend(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	// While a project starts (and may be pulling), cleanup must not run.
	gate := make(chan struct{})
	runtime.mu.Lock()
	runtime.resumeGate, runtime.entered = gate, make(chan struct{}, 1)
	runtime.mu.Unlock()
	resumed := make(chan error, 1)
	go func() { _, err := m.Resume(context.Background(), "example"); resumed <- err }()
	<-runtime.entered
	if m.ExclusiveImages(func() { t.Error("cleanup ran during a start") }) {
		t.Fatal("ExclusiveImages reported running during a start")
	}
	close(gate)
	if err := <-resumed; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !m.ExclusiveImages(func() {}) {
		if time.Now().After(deadline) {
			t.Fatal("cleanup still excluded after the start finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// An unreadable Compose file must stop cleanup, never hide an image in use.
	app, _ := m.Get("example")
	if err := os.Remove(app.ComposeFile); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ImageReferences(); err == nil {
		t.Fatal("ImageReferences ignored a missing Compose file")
	}
	if !strings.HasPrefix(m.ProjectPrefix(), "sd-") {
		t.Fatalf("ProjectPrefix = %q", m.ProjectPrefix())
	}
}

func TestDeleteBreaksOpenConnections(t *testing.T) {
	m, runtime := manager(t)
	first, err := m.Acquire(context.Background(), "example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Acquire(context.Background(), "example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	if first.Context == nil || first.Context.Err() != nil {
		t.Fatal("lease has no live context")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- m.Delete(context.Background(), "example") }()
	for _, lease := range []*Lease{first, second} {
		select {
		case <-lease.Context.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("delete did not break an open connection")
		}
	}
	// Containers stay up until the broken requests have finished.
	runtime.mu.Lock()
	downs := runtime.downs
	runtime.mu.Unlock()
	if downs != 0 {
		t.Fatal("containers removed before connections drained")
	}
	// A request arriving now waits for the deletion instead of reaching the app.
	waited := make(chan error, 1)
	go func() { _, err := m.Acquire(context.Background(), "example.localhost"); waited <- err }()
	first.Release()
	second.Release()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delete did not finish after connections drained")
	}
	if err := <-waited; !errors.Is(err, ErrNotFound) {
		t.Fatalf("request during deletion: %v", err)
	}
	if _, err := m.Get("example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("app survived deletion: %v", err)
	}
	// The name and domain are free again, as a redeploy needs.
	if _, err := m.Deploy(context.Background(), "example", bundle(t, "example.localhost")); err != nil {
		t.Fatal(err)
	}
	fresh, err := m.Acquire(context.Background(), "example.localhost")
	if err != nil || fresh.Context.Err() != nil {
		t.Fatalf("redeployed app's lease: %v", err)
	}
	fresh.Release()
}

func TestDeleteProceedsWhenAConnectionNeverFinishes(t *testing.T) {
	previous := leaseDrainTimeout
	leaseDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { leaseDrainTimeout = previous })
	m, runtime := manager(t)
	stuck, err := m.Acquire(context.Background(), "example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Release()
	if err := m.Delete(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.downs != 1 || stuck.Context.Err() == nil {
		t.Fatalf("downs=%d, lease context %v", runtime.downs, stuck.Context.Err())
	}
}
