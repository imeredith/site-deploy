package orchestrator

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"site-deploy/internal/engine"
	"site-deploy/internal/model"
	"site-deploy/internal/volumes"
	"site-deploy/internal/volumes/volumestest"
)

// sdVolumeBundle is a bundle whose web service mounts the site-deploy volume corpus at /corpus.
func sdVolumeBundle(t *testing.T, domain, key string, snapshot bool) string {
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
	mode := "false"
	if snapshot {
		mode = "true"
	}
	config := "x-site-deploy:\n  domain: " + domain + "\n  service: web\n  port: 80\n  volumes:\n    corpus:\n      key: " + key +
		"\n      target: /corpus\n      snapshot: " + mode + "\nservices:\n  web:\n    image: nginx:alpine\n"
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

func openVolumes(t *testing.T) (*volumes.Store, *volumestest.Subvolumes, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &volumestest.Subvolumes{}
	store, err := volumes.Open(dir, fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, fake, dir
}

func TestVolumesMountSnapshotsThatRedeploysReuse(t *testing.T) {
	store, fake, dir := openVolumes(t)
	data := t.TempDir()
	runtime := &fakeRuntime{}
	m, err := New(data, runtime, nil, WithVolumes(store))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, key, err := m.CreateVolume(ctx, "corpus")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, "bad", sdVolumeBundle(t, "bad.localhost", volumes.KeyPrefix+strings.Repeat("0", 64), true)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "wrong volume key") {
		t.Fatalf("deploy with a wrong key: %v", err)
	}
	if app, err := m.Deploy(ctx, "main", sdVolumeBundle(t, "main.localhost", key, false)); err != nil || app.State != model.StateRunning {
		t.Fatalf("deploy main: %+v %v", app, err)
	}
	if _, err := m.Deploy(ctx, "pr1", sdVolumeBundle(t, "pr1.localhost", key, true)); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(dir, "corpus", "snapshots", "pr1")
	compose, err := os.ReadFile(m.composePath("pr1"))
	if err != nil || !strings.Contains(string(compose), `"source": "`+snapshot+`"`) || strings.Contains(string(compose), key) {
		t.Fatalf("pr1's Compose file:\n%s %v", compose, err)
	}
	list, err := m.ListVolumes()
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	if want := []volumes.Use{{App: "main"}, {App: "pr1", Snapshot: true}}; !reflect.DeepEqual(list[0].UsedBy, want) {
		t.Fatalf("used by %+v", list[0].UsedBy)
	}
	if err := m.DeleteVolume(ctx, "corpus"); !errors.Is(err, volumes.ErrInUse) || !strings.Contains(err.Error(), "main, pr1") {
		t.Fatalf("delete a mounted volume: %v", err)
	}
	if err := m.DeleteVolumeSnapshot(ctx, "corpus", "pr1"); !errors.Is(err, volumes.ErrInUse) {
		t.Fatalf("delete a mounted snapshot: %v", err)
	}

	// A redeploy is delete, then deploy: it mounts the snapshot again.
	if err := os.WriteFile(filepath.Join(snapshot, "edited"), []byte("pr1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "pr1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, "pr1", sdVolumeBundle(t, "pr1.localhost", key, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(snapshot, "edited")); err != nil || fake.Snapshots() != 1 {
		t.Fatalf("redeploy took a new snapshot: %v, %d snapshots", err, fake.Snapshots())
	}

	// Every start checks the mount again: here the snapshot is no longer one.
	if _, err := m.Suspend(ctx, "pr1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(snapshot, snapshot+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshot, 0755); err != nil {
		t.Fatal(err)
	}
	if app, err := m.Resume(ctx, "pr1"); err == nil || !strings.Contains(err.Error(), "not a btrfs subvolume") || app.State != model.StateError {
		t.Fatalf("resume with a plain directory: %+v %v", app, err)
	}
	runtime.mu.Lock()
	resumes := runtime.resumes
	runtime.mu.Unlock()
	if resumes != 0 {
		t.Fatalf("Docker was asked to resume %d times", resumes)
	}
	if err := os.Remove(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(snapshot+".real", snapshot); err != nil {
		t.Fatal(err)
	}
	if app, err := m.Resume(ctx, "pr1"); err != nil || app.State != model.StateRunning {
		t.Fatalf("resume after repair: %+v %v", app, err)
	}

	// Deleting the apps frees the snapshot, then the volume.
	if err := m.Delete(ctx, "pr1"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteVolumeSnapshot(ctx, "corpus", "pr1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteVolume(ctx, "corpus"); err != nil {
		t.Fatal(err)
	}
	if list, err := m.ListVolumes(); err != nil || len(list) != 0 {
		t.Fatalf("list after deleting: %+v %v", list, err)
	}
}

func TestVolumesAfterARestart(t *testing.T) {
	store, _, _ := openVolumes(t)
	data := t.TempDir()
	m, err := New(data, &fakeRuntime{}, nil, WithVolumes(store))
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := m.CreateVolume(context.Background(), "corpus")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(context.Background(), "pr1", sdVolumeBundle(t, "pr1.localhost", key, true)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	running := engine.Status{Running: true, Upstream: "127.0.0.1:12345"}

	// With the volumes, the running app keeps running.
	m, err = New(data, &fakeRuntime{status: running}, nil, WithVolumes(store))
	if err != nil {
		t.Fatal(err)
	}
	if app, err := m.Get("pr1"); err != nil || app.State != model.StateRunning {
		t.Fatalf("restart with volumes: %+v %v", app, err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	// Without them, it's stopped: its mount is now just a host path nobody allowed.
	runtime := &fakeRuntime{status: running}
	m, err = New(data, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if app, err := m.Get("pr1"); err != nil || app.State != model.StateError || !strings.Contains(app.Error, "not allowed") || runtime.suspends != 1 {
		t.Fatalf("restart without volumes: %+v %v suspends=%d", app, err, runtime.suspends)
	}
	if _, err := m.Deploy(context.Background(), "pr2", sdVolumeBundle(t, "pr2.localhost", key, true)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "serve --volumes-dir") {
		t.Fatalf("deploy without volumes: %v", err)
	}
	if _, err := m.ListVolumes(); !errors.Is(err, ErrNoVolumes) {
		t.Fatalf("list without volumes: %v", err)
	}
}
