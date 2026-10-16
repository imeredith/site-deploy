package btrfs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

func TestIoctlNumbersMatchLinuxHeaders(t *testing.T) {
	// From linux/btrfs.h, as _IOW computes them on amd64 and arm64.
	for name, test := range map[string]struct{ got, want uintptr }{
		"BTRFS_IOC_SUBVOL_CREATE":  {iocSubvolCreate, 0x5000940e},
		"BTRFS_IOC_SNAP_DESTROY":   {iocSnapDestroy, 0x5000940f},
		"BTRFS_IOC_SNAP_CREATE_V2": {iocSnapCreateV2, 0x50009417},
	} {
		if test.got != test.want {
			t.Errorf("%s = %#x; want %#x", name, test.got, test.want)
		}
	}
	if unsafe.Sizeof(volArgs{}) != 4096 || unsafe.Sizeof(volArgsV2{}) != 4096 {
		t.Fatalf("argument sizes %d and %d; both must be 4096", unsafe.Sizeof(volArgs{}), unsafe.Sizeof(volArgsV2{}))
	}
}

func TestSplitNeedsACleanAbsolutePath(t *testing.T) {
	parent, name, err := split("/volumes/corpus/data")
	if err != nil || parent != "/volumes/corpus" || name != "data" {
		t.Fatalf("split = %q %q %v", parent, name, err)
	}
	for _, path := range []string{"data", "/volumes/../etc", "/volumes/corpus/", "/", "/volumes/" + strings.Repeat("x", subvolNameMax+1)} {
		if _, _, err := split(path); err == nil {
			t.Errorf("split accepted %q", path)
		}
	}
}

func TestHelperOnlyTouchesItsRoot(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"create"},
		{"create", "/etc/x"},
		{"create", "/volumes"},
		{"snapshot", "/volumes/a/data", "/tmp/b"},
		{"delete", "/volumes/a", "/volumes/b"},
		{"format", "/volumes/a"},
	} {
		if code := Helper(args, io.Discard); code == 0 {
			t.Errorf("helper accepted %q", args)
		}
	}
}

// TestSubvolumesOnBtrfs runs only with SITE_DEPLOY_TEST_BTRFS_DIR naming a
// directory on btrfs. It works unprivileged: a subvolume's owner may snapshot
// it, and Delete falls back to removing the files and then the empty subvolume.
func TestSubvolumesOnBtrfs(t *testing.T) {
	base := os.Getenv("SITE_DEPLOY_TEST_BTRFS_DIR")
	if base == "" {
		t.Skip("set SITE_DEPLOY_TEST_BTRFS_DIR to a directory on btrfs")
	}
	dir, err := os.MkdirTemp(base, "btrfs-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if ok, err := OnBtrfs(dir); err != nil || !ok {
		t.Fatalf("OnBtrfs(%s) = %v %v", dir, ok, err)
	}
	if ok, err := IsSubvolume(dir); err != nil || ok {
		t.Fatalf("a plain directory is a subvolume: %v %v", ok, err)
	}
	volume, snapshot := filepath.Join(dir, "volume"), filepath.Join(dir, "snapshot")
	if err := Create(volume); err != nil {
		t.Fatal(err)
	}
	if ok, err := IsSubvolume(volume); err != nil || !ok {
		t.Fatalf("IsSubvolume after Create = %v %v", ok, err)
	}
	if err := os.MkdirAll(filepath.Join(volume, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volume, "docs", "a"), []byte("master"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Snapshot(volume, snapshot); err != nil {
		t.Fatal(err)
	}
	// The snapshot is writable and independent of the volume.
	if err := os.WriteFile(filepath.Join(snapshot, "docs", "a"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(volume, "docs", "a")); err != nil || string(data) != "master" {
		t.Fatalf("volume after writing the snapshot: %q %v", data, err)
	}
	if err := Snapshot(filepath.Join(volume, "docs"), filepath.Join(dir, "docs")); err == nil {
		t.Fatal("snapshotted a plain directory")
	}
	if err := Snapshot(volume, snapshot); err == nil {
		t.Fatal("snapshot replaced an existing target")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(volume, link); err != nil {
		t.Fatal(err)
	}
	if err := Delete(link); err == nil {
		t.Fatal("deleted through a symlink")
	}
	for _, path := range []string{snapshot, volume} {
		if err := Delete(path); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", path, err)
		}
	}
}
