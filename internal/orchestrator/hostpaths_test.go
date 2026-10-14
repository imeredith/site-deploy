package orchestrator

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"site-deploy/internal/engine"
	"site-deploy/internal/hostpaths"
	"site-deploy/internal/model"
)

// hostBundle is a bundle whose web service mounts source at /data.
func hostBundle(t *testing.T, domain, mount string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	entry, err := w.Create("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	config := "x-site-deploy:\n  domain: " + domain + "\n  service: web\n  port: 80\nservices:\n  web:\n    image: nginx:alpine\n    volumes: [\"" + mount + "\"]\n"
	if _, err := entry.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHostPathsCheckedAtDeployAndEveryStart(t *testing.T) {
	host, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(host, "media")
	if err := os.Mkdir(media, 0755); err != nil {
		t.Fatal(err)
	}
	policy := hostpaths.Policy{{Path: media}}
	dir := t.TempDir()
	runtime := &fakeRuntime{}
	m, err := New(dir, runtime, policy)
	if err != nil {
		t.Fatal(err)
	}
	for mount, want := range map[string]string{
		host + ":/data:ro":    "not allowed for refused",
		media + ":/data":      "read-only",
		media + "/x:/data:ro": "no such file",
	} {
		if _, err := m.Deploy(context.Background(), "refused", hostBundle(t, "refused.localhost", mount)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("deploy mounting %s: %v; want %q", mount, err, want)
		}
	}
	app, err := m.Deploy(context.Background(), "example", hostBundle(t, "example.localhost", media+":/data:ro"))
	if err != nil || app.State != model.StateRunning {
		t.Fatalf("deploy: %+v %v", app, err)
	}
	if _, err := m.Suspend(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}

	// A container with write access elsewhere could swap the directory for a
	// symlink after the deploy. The next start refuses before Docker runs.
	if err := os.Rename(media, media+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", media); err != nil {
		t.Fatal(err)
	}
	app, err = m.Resume(context.Background(), "example")
	if err == nil || !strings.Contains(err.Error(), "symlink") || app.State != model.StateError {
		t.Fatalf("resume through symlink: %+v %v", app, err)
	}
	runtime.mu.Lock()
	resumes := runtime.resumes
	runtime.mu.Unlock()
	if resumes != 0 {
		t.Fatalf("Docker was asked to resume %d times", resumes)
	}
	if err := os.Remove(media); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(media+".old", media); err != nil {
		t.Fatal(err)
	}
	if app, err := m.Resume(context.Background(), "example"); err != nil || app.State != model.StateRunning {
		t.Fatalf("resume after repair: %+v %v", app, err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	// Removing the entry and restarting the daemon stops the running app.
	runtime = &fakeRuntime{status: engine.Status{Running: true, Upstream: "127.0.0.1:12345"}}
	m, err = New(dir, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	app, err = m.Get("example")
	if err != nil || app.State != model.StateError || !strings.Contains(app.Error, "not allowed for example") || runtime.suspends != 1 {
		t.Fatalf("restart without the entry: %+v %v suspends=%d", app, err, runtime.suspends)
	}
	if _, err := m.Resume(context.Background(), "example"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("resume without the entry: %v", err)
	}
}
