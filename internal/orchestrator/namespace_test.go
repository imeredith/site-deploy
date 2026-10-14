package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"site-deploy/internal/archive"
	"site-deploy/internal/engine"
	"site-deploy/internal/manifest"
	"site-deploy/internal/model"
	"site-deploy/internal/store"
)

func TestNamespaceAgentCManagersIsolateAndRetainDockerProjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	first := namespaceAgentCManager(t, root)
	second := namespaceAgentCManager(t, t.TempDir())
	zip := bundle(t, "namespace-agent-c.localhost")
	app, err := first.Deploy(ctx, "namespace-agent-c", zip)
	if err != nil {
		t.Fatal(err)
	}
	other, err := second.Deploy(ctx, app.Name, zip)
	if err != nil {
		t.Fatal(err)
	}
	if app.Project == other.Project {
		t.Fatalf("independent managers control the same Docker project: %s", app.Project)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	first = namespaceAgentCManager(t, root)
	restored, err := first.Get(app.Name)
	if err != nil || restored.Project != app.Project {
		t.Fatalf("restart changed project identity: %+v, %v", restored, err)
	}
	if err := first.Delete(ctx, app.Name); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	first = namespaceAgentCManager(t, root)
	replacement, err := first.Deploy(ctx, app.Name, zip)
	if err != nil || replacement.Project != app.Project {
		t.Fatalf("delete/restart/redeploy disconnected named volumes: %+v, %v", replacement, err)
	}
}

func TestNamespaceAgentCLegacyProjectSurvivesDeleteAndRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	const name = "namespace-agent-c-legacy"
	zip := bundle(t, name+".localhost")
	directory := filepath.Join(root, "apps", name)
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := archive.Extract(zip, directory, archive.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	spec, composeFile, err := manifest.Prepare(directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy := model.App{
		Name: name, Spec: spec, Project: "sd-" + name,
		Directory: directory, ComposeFile: composeFile,
		State: model.StateSuspended, CreatedAt: time.Now().UTC(),
	}
	disk, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.Save(legacy); err != nil {
		disk.Close()
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	m := namespaceAgentCManager(t, root)
	app, err := m.Get(name)
	if err != nil || app.Project != legacy.Project {
		t.Fatalf("legacy deployment changed its Docker project: %+v, %v", app, err)
	}
	if err := m.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m = namespaceAgentCManager(t, root)
	app, err = m.Deploy(ctx, name, zip)
	if err != nil || app.Project != legacy.Project {
		t.Fatalf("legacy deleted-app volumes would be disconnected: %+v, %v", app, err)
	}
}

func TestNamespaceAgentCRejectsMismatchedPersistedProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := namespaceAgentCManager(t, root)
	app, err := m.Deploy(context.Background(), "namespace-agent-c-invalid", bundle(t, "namespace-agent-c-invalid.localhost"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	disk, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	app.Project = "sd-" + app.Name
	if err := disk.Save(app); err != nil {
		disk.Close()
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	if unexpected, err := New(root, runtime, nil); err == nil {
		unexpected.Close()
		t.Fatal("accepted project outside the persisted installation namespace")
	}
	if runtime.inspects != 0 {
		t.Fatal("inspected Docker resources before validating their project identity")
	}
}

func namespaceAgentCManager(t *testing.T, root string) *Manager {
	t.Helper()
	m, err := New(root, &fakeRuntime{status: engine.Status{Suspended: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}
