//go:build integration

package integration

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"site-deploy/internal/engine"
	"site-deploy/internal/images"
)

// TestNamespacedRegistryPruneAndCollect runs a real registry container on a
// random loopback port with private storage, pushes images to it, and checks
// that cleanup deletes only unused manifests and reclaims their blobs. It never
// prunes host images: that would touch resources this test does not own.
func TestNamespacedRegistryPruneAndCollect(t *testing.T) {
	if os.Getenv("SITE_DEPLOY_INTEGRATION") != "1" {
		t.Skip("set SITE_DEPLOY_INTEGRATION=1 to use the local Docker Engine")
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	namespace := "sdtest-" + hex.EncodeToString(random[:])
	registryImage := os.Getenv("SITE_DEPLOY_TEST_REGISTRY_IMAGE")
	if registryImage == "" {
		registryImage = "registry:3"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v %s", strings.Join(args, " "), err, output)
		}
		return string(output)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	storage := filepath.Join(t.TempDir(), "registry")
	name := namespace + "-registry"
	registry, err := images.NewRegistry(engine.New("docker", ""), address, registryImage, name, storage)
	if err != nil {
		t.Fatal(err)
	}
	var references []string
	defer func() {
		for _, reference := range references {
			exec.Command("docker", "image", "rm", reference).Run()
		}
		exec.Command("docker", "rm", "--force", name).Run()
	}()
	if err := registry.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	// Small single-layer images with distinct contents, made without a build.
	importImage := func(reference, content string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "layer.tar")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		w := tar.NewWriter(file)
		w.WriteHeader(&tar.Header{Name: "content.txt", Mode: 0644, Size: int64(len(content))})
		w.Write([]byte(content))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		file.Close()
		run("image", "import", path, reference)
		references = append(references, reference)
	}
	base1 := address + "/" + namespace + "/base:1"
	base2 := address + "/" + namespace + "/base:2"
	other := address + "/" + namespace + "/other:1"
	importImage(base1, "base "+namespace)
	run("image", "tag", base1, base2)
	references = append(references, base2)
	importImage(other, strings.Repeat("other "+namespace, 4096))
	for _, reference := range []string{base1, base2, other} {
		run("image", "push", "--quiet", reference)
	}
	blobs := func() int {
		count := 0
		filepath.WalkDir(filepath.Join(storage, "docker/registry/v2/blobs"), func(_ string, entry fs.DirEntry, err error) error {
			if err == nil && entry.Name() == "data" {
				count++
			}
			return nil
		})
		return count
	}
	before := blobs()
	listed, err := registry.List(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 || listed[0].Digest != listed[1].Digest || listed[0].Pushed.IsZero() {
		t.Fatalf("listed %+v", listed)
	}

	// base:2 is in use, and base:1 names the same manifest, so both stay.
	inUse := map[string][]string{base2: {"app"}}
	deleted, collected, err := registry.Prune(ctx, inUse, 0, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []string{namespace + "/other:1"}) || !collected {
		t.Fatalf("first prune deleted %v, collected %v", deleted, collected)
	}
	if after := blobs(); after >= before {
		t.Fatalf("garbage collection kept every blob: %d before, %d after", before, after)
	}
	// The registry is serving again after offline collection.
	listed, err = registry.List(ctx, inUse)
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, image := range listed {
		tags = append(tags, image.Tag)
	}
	sort.Strings(tags)
	if !reflect.DeepEqual(tags, []string{"1", "2"}) {
		t.Fatalf("after first prune: %+v", listed)
	}

	// Once nothing uses them, both tags go; a fresh push survives the grace period.
	importImage(address+"/"+namespace+"/fresh:1", "fresh "+namespace)
	run("image", "push", "--quiet", address+"/"+namespace+"/fresh:1")
	deleted, _, err = registry.Prune(ctx, nil, time.Hour, time.Now())
	if err != nil || len(deleted) != 0 {
		t.Fatalf("grace period ignored: %v %v", deleted, err)
	}
	deleted, _, err = registry.Prune(ctx, nil, time.Hour, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(deleted)
	if !reflect.DeepEqual(deleted, []string{namespace + "/base:1", namespace + "/base:2", namespace + "/fresh:1"}) {
		t.Fatalf("unused tags not deleted after the grace period: %v", deleted)
	}
	t.Logf("verified registry container lifecycle, push, listing, shared-manifest protection, grace period, deletion, and offline garbage collection (%d blobs before, %d after)", before, blobs())
}
