package orchestrator

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"site-deploy/internal/engine"
	"site-deploy/internal/model"
)

// volumeBundle is a ZIP whose Compose application has the named volumes.
func volumeBundle(t *testing.T, domain string, volumes ...string) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "app-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	var mounts, declared strings.Builder
	for _, volume := range volumes {
		fmt.Fprintf(&mounts, "      - %s:/%s\n", volume, volume)
		fmt.Fprintf(&declared, "  %s: {}\n", volume)
	}
	config := fmt.Sprintf("x-site-deploy:\n  domain: %s\n  service: web\n  port: 80\nservices:\n  web:\n    image: nginx:alpine\n    volumes:\n%svolumes:\n%s", domain, mounts.String(), declared.String())
	w := zip.NewWriter(file)
	entry, err := w.Create("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

// volumeManager deploys "main" with volumes data and cache, both existing.
func volumeManager(t *testing.T) (*Manager, *fakeRuntime) {
	t.Helper()
	runtime := &fakeRuntime{status: engine.Status{Running: true, Upstream: "127.0.0.1:12345"}}
	m, err := New(t.TempDir(), runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if _, err := m.Deploy(context.Background(), "main", volumeBundle(t, "main.localhost", "cache", "data")); err != nil {
		t.Fatal(err)
	}
	runtime.volumes = map[string]bool{
		m.projectPrefix + "main_data":  true,
		m.projectPrefix + "main_cache": true,
	}
	return m, runtime
}

func TestDeployCopiesSharedVolumesBeforeFirstStart(t *testing.T) {
	m, runtime := volumeManager(t)
	ctx := context.Background()
	// uploads is not in main, so it starts empty.
	app, err := m.DeployWith(ctx, "preview", volumeBundle(t, "preview.localhost", "data", "uploads"), DeployOptions{CopyVolumesFrom: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if app.State != model.StateRunning {
		t.Fatalf("state %s", app.State)
	}
	want := []volumeCopy{{source: "main", target: "preview", volumes: []string{"data"}}}
	if !reflect.DeepEqual(runtime.copies, want) {
		t.Fatalf("copies %+v", runtime.copies)
	}
	if source, _ := m.Get("main"); source.State != model.StateRunning {
		t.Fatalf("source state %s", source.State)
	}
	// Resuming copies nothing again.
	if _, err := m.Suspend(ctx, "preview"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(ctx, "preview"); err != nil {
		t.Fatal(err)
	}
	if len(runtime.copies) != 1 {
		t.Fatalf("copies %+v", runtime.copies)
	}
}

func TestDeployRefusesVolumeCopiesItCannotMake(t *testing.T) {
	m, runtime := volumeManager(t)
	ctx := context.Background()
	for _, test := range []struct {
		name, source string
		volumes      []string
		want         error
		message      string
	}{
		{"self", "self", []string{"data"}, ErrInvalid, "its own volumes"},
		{"orphan", "missing", []string{"data"}, ErrInvalid, "not deployed"},
		{"unrelated", "main", []string{"other"}, ErrInvalid, "no named volumes"},
		{"kept", "main", []string{"data"}, ErrConflict, "already exists"},
	} {
		if test.name == "kept" {
			runtime.mu.Lock()
			runtime.volumes[m.projectPrefix+"kept_data"] = true
			runtime.mu.Unlock()
		}
		_, err := m.DeployWith(ctx, test.name, volumeBundle(t, test.name+".localhost", test.volumes...), DeployOptions{CopyVolumesFrom: test.source})
		if !errors.Is(err, test.want) || !strings.Contains(err.Error(), test.message) {
			t.Fatalf("%s: %v", test.name, err)
		}
		if _, err := m.Get(test.name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s was saved: %v", test.name, err)
		}
	}
	runtime.mu.Lock()
	delete(runtime.volumes, m.projectPrefix+"main_data")
	runtime.mu.Unlock()
	if _, err := m.DeployWith(ctx, "early", volumeBundle(t, "early.localhost", "data"), DeployOptions{CopyVolumesFrom: "main"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "volume data does not exist") {
		t.Fatalf("missing source volume: %v", err)
	}
	if len(runtime.copies) != 0 {
		t.Fatalf("copies %+v", runtime.copies)
	}
}

func TestSourceIsHeldWhileItsVolumesCopy(t *testing.T) {
	m, runtime := volumeManager(t)
	ctx := context.Background()
	gate := make(chan struct{})
	runtime.copyGate = gate
	deployed := make(chan error, 1)
	go func() {
		_, err := m.DeployWith(ctx, "preview", volumeBundle(t, "preview.localhost", "data"), DeployOptions{CopyVolumesFrom: "main"})
		deployed <- err
	}()
	source, err := m.lookup("main")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		source.mu.Lock()
		held := source.op != nil
		source.mu.Unlock()
		if held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("source never held")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := m.Suspend(ctx, "main"); !errors.Is(err, ErrBusy) {
		t.Fatalf("suspend during copy: %v", err)
	}
	if err := m.Delete(ctx, "main"); !errors.Is(err, ErrBusy) {
		t.Fatalf("delete during copy: %v", err)
	}
	if err := m.Delete(ctx, "preview"); !errors.Is(err, ErrBusy) {
		t.Fatalf("delete target during copy: %v", err)
	}
	leased := make(chan *Lease, 1)
	go func() {
		lease, err := m.Acquire(ctx, "main.localhost")
		if err != nil {
			t.Error(err)
		}
		leased <- lease
	}()
	select {
	case <-leased:
		t.Fatal("request reached the source during the copy")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	if err := <-deployed; err != nil {
		t.Fatal(err)
	}
	if lease := <-leased; lease != nil {
		lease.Release()
	}
}

func TestFailedCopyMakesAPausedSourceReconcile(t *testing.T) {
	m, runtime := volumeManager(t)
	ctx := context.Background()
	runtime.copyError = errors.New("disk full")
	runtime.copyPaused = true
	app, err := m.DeployWith(ctx, "preview", volumeBundle(t, "preview.localhost", "data"), DeployOptions{CopyVolumesFrom: "main"})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("deploy: %v", err)
	}
	if app.State != model.StateError {
		t.Fatalf("target state %s", app.State)
	}
	source, _ := m.Get("main")
	if source.State != model.StateError || source.Upstream != "" {
		t.Fatalf("source %s %q", source.State, source.Upstream)
	}
	// The next request resumes it, which unpauses its containers.
	runtime.mu.Lock()
	runtime.status = engine.Status{Running: true, Upstream: "127.0.0.1:12345"}
	runtime.mu.Unlock()
	lease, err := m.Acquire(ctx, "main.localhost")
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if runtime.resumes != 1 {
		t.Fatalf("resumes %d", runtime.resumes)
	}
}
