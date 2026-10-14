package orchestrator

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"site-deploy/internal/manifest"
	"site-deploy/internal/model"
	"site-deploy/internal/store"
)

const storageCompose = `x-site-deploy:
  domain: storage.localhost
  service: web
  port: 80
services:
  web:
    image: nginx:alpine
    volumes:
      - .:/app
      - data:/data
volumes:
  data: {}
`

func storageBundle(t *testing.T, extra ...string) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "site-deploy-storage-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	entry, err := w.Create("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(storageCompose)); err != nil {
		t.Fatal(err)
	}
	for _, name := range extra {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("uploaded reserved configuration")); err != nil {
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

func storageLegacy(t *testing.T) (string, model.App, []byte) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "apps", "storage-legacy")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(storageCompose), 0644); err != nil {
		t.Fatal(err)
	}
	spec, compose, err := manifest.Prepare(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := model.App{Name: "storage-legacy", Spec: spec, Project: "sd-storage-legacy", Directory: dir, ComposeFile: compose, State: model.StateSuspended}
	disk, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.Save(app); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(compose)
	if err != nil {
		t.Fatal(err)
	}
	return root, app, data
}

func TestSiteDeployStorageWritableBundleCannotChangeControl(t *testing.T) {
	t.Parallel()
	m, err := New(t.TempDir(), &fakeRuntime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	app, err := m.Deploy(context.Background(), "storage-app", storageBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(app.Directory, app.ComposeFile)
	if err != nil || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("trusted Compose is bind-accessible: %s (%v)", rel, err)
	}
	for path, want := range map[string]os.FileMode{app.ComposeFile: 0600, filepath.Dir(app.ComposeFile): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("control mode %s: %v %v", path, info, err)
		}
	}
	legacy := filepath.Join(app.Directory, "compose.generated.json")
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated control remained in mounted bundle: %v", err)
	}
	before, err := os.ReadFile(app.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"services":{"web":{"privileged":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(context.Background(), app.Name); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(app.ComposeFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("application write changed protected configuration: %s %v", after, err)
	}
}

func TestSiteDeployStorageRejectsReservedUploadedControlName(t *testing.T) {
	t.Parallel()
	runtime := &fakeRuntime{}
	m, err := New(t.TempDir(), runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_, err = m.Deploy(context.Background(), "storage-reserved", storageBundle(t, "compose.generated.json"))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved uploaded filename was accepted: %v", err)
	}
	if len(m.List()) != 0 || runtime.ups != 0 {
		t.Fatal("rejected deployment reached runtime or state")
	}
}

func TestSiteDeployStorageLegacyMigrationDetachesOpenFilesAndKeepsProject(t *testing.T) {
	t.Parallel()
	root, previous, expected := storageLegacy(t)
	oldFile, err := os.OpenFile(previous.ComposeFile, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()
	oldInfo, err := oldFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(previous.Directory, "retained-control-link")
	if err := os.Link(previous.ComposeFile, alias); err != nil {
		t.Fatal(err)
	}
	m, err := New(root, &fakeRuntime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	app, err := m.Get(previous.Name)
	if err != nil || app.Project != previous.Project || app.Directory != previous.Directory || app.ComposeFile != filepath.Join(root, "compose", app.Name+".json") {
		t.Fatalf("migration changed runtime identity: %+v %v", app, err)
	}
	newInfo, err := os.Stat(app.ComposeFile)
	if err != nil || os.SameFile(oldInfo, newInfo) {
		t.Fatalf("protected configuration reused application-owned inode: %v", err)
	}
	if _, err := oldFile.WriteAt([]byte("CORRUPTED"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alias, []byte("changed through hard link"), 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(app.ComposeFile)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("legacy descriptor modified trusted configuration: %s %v", actual, err)
	}
	stored, err := m.store.Load()
	if err != nil || len(stored) != 1 || stored[0].ComposeFile != app.ComposeFile {
		t.Fatalf("migration was not persisted: %+v %v", stored, err)
	}
}

func TestSiteDeployStorageInterruptedMigrationUsesProtectedCopy(t *testing.T) {
	t.Parallel()
	root, previous, expected := storageLegacy(t)
	control := filepath.Join(root, "compose", previous.Name+".json")
	if err := privateStorageDirectory(filepath.Dir(control)); err != nil {
		t.Fatal(err)
	}
	if err := copyCompose(previous.ComposeFile, control); err != nil {
		t.Fatal(err)
	}
	// A prior daemon installed control data but stopped before saving its path.
	if err := os.Remove(previous.ComposeFile); err != nil {
		t.Fatal(err)
	}
	m, err := New(root, &fakeRuntime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	app, err := m.Get(previous.Name)
	if err != nil || app.ComposeFile != control {
		t.Fatalf("interrupted migration failed: %+v %v", app, err)
	}
	actual, err := os.ReadFile(control)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("protected copy changed: %s %v", actual, err)
	}
}

func TestSiteDeployStorageDeleteRetriesProtectedCleanupAfterRestart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	runtime := &fakeRuntime{}
	m, err := New(root, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	app, err := m.Deploy(context.Background(), "storage-delete", storageBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	m.removeFiles = func(path string) error {
		if path == app.ComposeFile {
			return errors.New("injected protected file removal failure")
		}
		return os.RemoveAll(path)
	}
	if err := m.Delete(context.Background(), app.Name); err == nil {
		t.Fatal("protected cleanup failure was lost")
	}
	if got, _ := m.Get(app.Name); got.State != model.StateDeleting {
		t.Fatalf("lost durable deletion marker: %+v", got)
	}
	if _, err := os.Stat(app.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bundle was not removed first: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = New(root, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Delete(context.Background(), app.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(app.ComposeFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("protected configuration survived deletion: %v", err)
	}
	if _, err := m.Get(app.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted state survived: %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.downs != 1 || runtime.inspects != 0 {
		t.Fatalf("retry unexpectedly used Docker: downs=%d inspects=%d", runtime.downs, runtime.inspects)
	}
}

func TestSiteDeployStorageRejectsSymlinksAndForeignControlPaths(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"control-directory", "bundle-directory", "legacy-file", "control-file", "control-hardlink", "foreign-path"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			root, app, _ := storageLegacy(t)
			outside := t.TempDir()
			switch scenario {
			case "control-directory":
				if err := os.Symlink(outside, filepath.Join(root, "compose")); err != nil {
					t.Fatal(err)
				}
			case "bundle-directory":
				if err := os.Rename(app.Directory, filepath.Join(outside, "bundle")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "bundle"), app.Directory); err != nil {
					t.Fatal(err)
				}
			case "legacy-file":
				if err := os.Remove(app.ComposeFile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "compose.json"), app.ComposeFile); err != nil {
					t.Fatal(err)
				}
			case "control-file", "control-hardlink":
				if err := privateStorageDirectory(filepath.Join(root, "compose")); err != nil {
					t.Fatal(err)
				}
				link := os.Symlink
				if scenario == "control-hardlink" {
					link = os.Link
				}
				if err := link(app.ComposeFile, filepath.Join(root, "compose", app.Name+".json")); err != nil {
					t.Fatal(err)
				}
			case "foreign-path":
				app.ComposeFile = filepath.Join(outside, "foreign.json")
				disk, err := store.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				if err := disk.Save(app); err != nil {
					t.Fatal(err)
				}
				disk.Close()
			}
			if m, err := New(root, &fakeRuntime{}, nil); err == nil {
				m.Close()
				t.Fatal("accepted unsafe storage")
			}
		})
	}
}
