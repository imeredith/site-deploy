//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"site-deploy/internal/engine"
	"site-deploy/internal/gateway"
	"site-deploy/internal/orchestrator"
	"site-deploy/internal/volumes"
)

// TestSiteDeployVolumesOnBtrfs needs, besides SITE_DEPLOY_INTEGRATION=1,
// SITE_DEPLOY_TEST_BTRFS_DIR: a directory on btrfs, where it makes and then
// removes its own volumes directory.
func TestSiteDeployVolumesOnBtrfs(t *testing.T) {
	if os.Getenv("SITE_DEPLOY_INTEGRATION") != "1" {
		t.Skip("set SITE_DEPLOY_INTEGRATION=1 to create isolated Docker test resources")
	}
	base := os.Getenv("SITE_DEPLOY_TEST_BTRFS_DIR")
	if base == "" {
		t.Skip("set SITE_DEPLOY_TEST_BTRFS_DIR to a directory on btrfs")
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	namespace := "sdtest-" + hex.EncodeToString(random[:])
	t.Logf("test namespace: %s", namespace)
	image := os.Getenv("SITE_DEPLOY_TEST_VOLUME_IMAGE")
	if image == "" {
		image = "busybox:1.37"
	}

	// The helper container runs this program, so it must be static.
	executable := filepath.Join(t.TempDir(), "site-deploy")
	build := exec.Command("go", "build", "-o", executable, "site-deploy/cmd/site-deploy")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the helper: %v %s", err, output)
	}
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(resolved, namespace+"-")
	if err != nil {
		t.Fatal(err)
	}
	docker := engine.New("docker", "")
	subvolumes, err := engine.NewSubvolumes(docker, dir, executable)
	if err != nil {
		t.Fatal(err)
	}
	store, err := volumes.Open(dir, subvolumes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := orchestrator.New(t.TempDir(), docker, nil, orchestrator.WithVolumes(store))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	proxy := httptest.NewServer(gateway.New(m))
	defer proxy.Close()
	ctx := context.Background()
	volume, main, preview := namespace, namespace+"-main", namespace+"-pr"
	// Clean up only what this test made, in order: apps, snapshot, volume.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, app := range []string{preview, main} {
			if _, err := m.Get(app); err == nil {
				if err := m.Delete(cleanup, app); err != nil {
					t.Errorf("cleanup %s: %v", app, err)
				}
			}
		}
		if err := m.DeleteVolumeSnapshot(cleanup, volume, preview); err != nil && !strings.Contains(err.Error(), "no snapshot") {
			t.Errorf("cleanup snapshot: %v", err)
		}
		if err := m.DeleteVolume(cleanup, volume); err != nil && !strings.Contains(err.Error(), "not found") {
			t.Errorf("cleanup volume: %v", err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("cleanup %s: %v", dir, err)
		}
	}()

	created, key, err := m.CreateVolume(ctx, volume)
	if err != nil {
		t.Fatal(err)
	}
	// Fill the volume as root, as an operator would on the server.
	if output, err := exec.Command("docker", "run", "--rm", "--network", "none", "-v", created.Path+":/fill", image,
		"sh", "-c", "echo master > /fill/page.txt").CombinedOutput(); err != nil {
		t.Fatalf("fill the volume: %v %s", err, output)
	}
	bundle := func(app string, snapshot, readOnly bool) string {
		path := filepath.Join(t.TempDir(), app+".zip")
		writeArchive(t, path, map[string]string{"compose.yaml": fmt.Sprintf(`x-site-deploy:
  domain: %s.localhost
  service: web
  port: 8080
  start_timeout: 60s
  volumes:
    %s:
      key: %s
      target: /www
      read_only: %t
      snapshot: %t
services:
  web:
    image: %s
    init: true
    command: ["httpd", "-f", "-p", "8080", "-h", "/www"]
`, app, volume, key, readOnly, snapshot, image)})
		return path
	}
	get := func(app string) string {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, proxy.URL+"/page.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = app + ".localhost"
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return strings.TrimSpace(string(body))
	}
	write := func(app, text string) error {
		project := m.ProjectPrefix() + app
		output, err := exec.Command("docker", "exec", project+"-web-1", "sh", "-c", "echo "+text+" > /www/page.txt").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v %s", err, output)
		}
		return nil
	}

	if _, err := m.Deploy(ctx, main, bundle(main, false, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, preview, bundle(preview, true, false)); err != nil {
		t.Fatal(err)
	}
	if got := get(main) + " " + get(preview); got != "master master" {
		t.Fatalf("before writing: %q", got)
	}
	if err := write(preview, "preview"); err != nil {
		t.Fatal(err)
	}
	if err := write(main, "main"); err == nil {
		t.Fatal("wrote through a read-only mount")
	}
	if got := get(main) + " " + get(preview); got != "master preview" {
		t.Fatalf("after the preview wrote to its snapshot: %q", got)
	}

	// Redeploying the preview mounts its snapshot again, with its changes.
	if err := m.Delete(ctx, preview); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deploy(ctx, preview, bundle(preview, true, false)); err != nil {
		t.Fatal(err)
	}
	if got := get(preview); got != "preview" {
		t.Fatalf("after redeploying: %q", got)
	}
	list, err := m.ListVolumes()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range list {
		if v.Name == volume && (len(v.Snapshots) != 1 || len(v.UsedBy) != 2) {
			t.Fatalf("volume %+v", v)
		}
	}

	// The snapshot holds root's files; deleting it needs the helper.
	if err := m.Delete(ctx, preview); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteVolumeSnapshot(ctx, volume, preview); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, main); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteVolume(ctx, volume); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, volume)); !os.IsNotExist(err) {
		t.Fatalf("volume left behind: %v", err)
	}
}
