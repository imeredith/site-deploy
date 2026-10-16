package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"site-deploy/internal/btrfs"
	"site-deploy/internal/hostpaths"
)

func TestTokenGeneratedOnceAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	first, created, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created || len(first) != 64 {
		t.Fatal("expected new random 256-bit token")
	}
	second, created, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if created || first != second {
		t.Fatal("token changed across daemon restart")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadToken(path); err == nil {
		t.Fatal("accepted token readable by other users")
	}
}

func TestControlAndGatewayAddressesMustBeLoopback(t *testing.T) {
	for _, address := range []string{"127.0.0.1:9081", "[::1]:9081"} {
		if err := loopbackAddress(address); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
	for _, address := range []string{":9081", "0.0.0.0:9081", "192.0.2.1:9081", "example.com:9081", "localhost:9081"} {
		if err := loopbackAddress(address); err == nil {
			t.Errorf("accepted %s", address)
		}
	}
}

func TestVolumesDirChecks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(base, "run", "docker.sock")
	media := filepath.Join(base, "media")
	policy := hostpaths.Policy{{Path: media}}
	for dir, want := range map[string]string{
		"relative":                       "clean absolute path",
		base + "/volumes,x":              "clean absolute path",
		base + "/volumes/":               "clean absolute path",
		filepath.Join(base, "link", "v"): "goes through a symlink",
		filepath.Join(data, "volumes"):   "overlaps the data directory",
		base:                             "overlaps the data directory",
		filepath.Join(base, "run"):       "overlaps the Docker socket",
		filepath.Join(media, "volumes"):  "host path " + media + " overlaps volumes-dir",
	} {
		if err := os.MkdirAll(filepath.Join(base, "run"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := checkVolumesDir(dir, data, socket, policy); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("checkVolumesDir(%s) = %v; want %q", dir, err, want)
		}
	}
	// The remaining check is the filesystem's.
	err = checkVolumesDir(filepath.Join(base, "volumes"), data, socket, policy)
	if onBtrfs, _ := btrfs.OnBtrfs(base); onBtrfs {
		if err != nil {
			t.Fatalf("on btrfs: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "not on a btrfs filesystem") {
		t.Fatalf("not on btrfs: %v", err)
	}
}
