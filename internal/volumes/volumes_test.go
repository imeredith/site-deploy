package volumes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"site-deploy/internal/volumes/volumestest"
)

func open(t *testing.T) (*Store, *volumestest.Subvolumes, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &volumestest.Subvolumes{}
	s, err := Open(dir, fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, fake, dir
}

func TestCreateMakesAnEmptyVolume(t *testing.T) {
	s, _, dir := open(t)
	volume, err := s.Create(context.Background(), "corpus")
	if err != nil {
		t.Fatal(err)
	}
	if volume.Name != "corpus" || volume.Path != filepath.Join(dir, "corpus", "data") || volume.Created.IsZero() {
		t.Fatalf("volume %+v", volume)
	}
	if entries, err := os.ReadDir(volume.Path); err != nil || len(entries) != 0 {
		t.Fatalf("data: %v %v", entries, err)
	}
	info, err := os.Stat(filepath.Join(dir, "corpus", "volume.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("volume.json: %v %v", info, err)
	}
	if _, err := s.Create(context.Background(), "corpus"); !errors.Is(err, ErrExists) {
		t.Fatalf("second create: %v", err)
	}
	if _, err := s.Create(context.Background(), "Corpus"); err == nil {
		t.Fatal("accepted an invalid name")
	}
	// A second daemon can't use the same directory.
	if _, err := Open(dir, &volumestest.Subvolumes{}); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("second open: %v", err)
	}
}

func TestFailedCreateLeavesNothing(t *testing.T) {
	s, fake, dir := open(t)
	fake.Fail = volumestest.ErrInjected
	if _, err := s.Create(context.Background(), "corpus"); !errors.Is(err, volumestest.ErrInjected) {
		t.Fatalf("create: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "corpus")); !os.IsNotExist(err) {
		t.Fatalf("left %v", err)
	}
	// An interrupted create, without volume.json, is reported and can be deleted.
	fake.Fail = nil
	if err := os.Mkdir(filepath.Join(dir, "partial"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), "partial"); !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("create over partial: %v", err)
	}
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Fatalf("list shows a partial volume: %+v %v", list, err)
	}
	if err := s.Delete(context.Background(), "partial"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), "partial"); err != nil {
		t.Fatal(err)
	}
}

func TestSourceKeepsEachAppsSnapshot(t *testing.T) {
	s, fake, dir := open(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "corpus"); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "corpus", "data")
	if err := os.WriteFile(filepath.Join(data, "page"), []byte("master"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Source(ctx, "missing", "main", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing volume: %v", err)
	}
	if source, err := s.Source(ctx, "corpus", "main", false); err != nil || source != data {
		t.Fatalf("direct source %q %v", source, err)
	}
	snapshot := filepath.Join(dir, "corpus", "snapshots", "pr1")
	if source, err := s.Source(ctx, "corpus", "pr1", true); err != nil || source != snapshot {
		t.Fatalf("snapshot source %q %v", source, err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "page"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	// A redeploy mounts the same snapshot, with its changes, without a new one.
	if source, err := s.Source(ctx, "corpus", "pr1", true); err != nil || source != snapshot || fake.Snapshots() != 1 {
		t.Fatalf("second snapshot source %q %v, %d snapshots", source, err, fake.Snapshots())
	}
	if page, _ := os.ReadFile(filepath.Join(snapshot, "page")); string(page) != "changed" {
		t.Fatalf("snapshot reset: %q", page)
	}
	if page, _ := os.ReadFile(filepath.Join(data, "page")); string(page) != "master" {
		t.Fatalf("volume changed with its snapshot: %q", page)
	}
	if _, err := s.Source(ctx, "corpus", "pr2", true); err != nil || fake.Snapshots() != 2 {
		t.Fatalf("second app: %v, %d snapshots", err, fake.Snapshots())
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	want := []Snapshot{{"pr1", snapshot}, {"pr2", filepath.Join(dir, "corpus", "snapshots", "pr2")}}
	if !reflect.DeepEqual(list[0].Snapshots, want) {
		t.Fatalf("snapshots %+v", list[0].Snapshots)
	}
}

func TestVolumesMadeBy090StillWork(t *testing.T) {
	s, _, dir := open(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "corpus"); err != nil {
		t.Fatal(err)
	}
	// 0.9.0 also stored the hash of a key, which nothing checks any more.
	old := `{"key_sha256":"0d7e5a1c3e3b1f4e9a2b6c8d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b","created":"2026-10-16T00:49:30Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "corpus", "volume.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Created.Format("2006-01-02") != "2026-10-16" {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := s.Source(ctx, "corpus", "pr1", true); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAllowsOnlyRealVolumesAndOwnSnapshots(t *testing.T) {
	s, _, dir := open(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "corpus"); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "corpus", "data")
	snapshot, err := s.Source(ctx, "corpus", "pr1", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ app, source string }{{"main", data}, {"pr1", data}, {"pr1", snapshot}} {
		if err := s.Check(test.app, test.source); err != nil {
			t.Errorf("Check(%s, %s): %v", test.app, test.source, err)
		}
	}
	plain := filepath.Join(dir, "corpus", "snapshots", "pr3")
	if err := os.Mkdir(plain, 0755); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ app, source, want string }{
		{"pr2", snapshot, "may only mount its own"},
		{"pr3", plain, "not a btrfs subvolume"},
		{"main", filepath.Join(dir, "corpus", "volume.json"), "not a site-deploy volume"},
		{"main", filepath.Join(dir, "corpus"), "not a site-deploy volume"},
		{"main", filepath.Join(dir, "other", "data"), "volume not found"},
		{"main", "/etc/passwd", "not a site-deploy volume"},
	} {
		if err := s.Check(test.app, test.source); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("Check(%s, %s) = %v; want %q", test.app, test.source, err, test.want)
		}
	}
	// A snapshot swapped for a symlink to the volume is refused.
	if err := os.Rename(snapshot, snapshot+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(data, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := s.Check("pr1", snapshot); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked snapshot: %v", err)
	}
	if name, app, ok := s.Locate(snapshot); !ok || name != "corpus" || app != "pr1" {
		t.Fatalf("Locate = %q %q %v", name, app, ok)
	}
	if _, _, ok := s.Locate(dir + "/corpus/data/sub"); ok {
		t.Fatal("located a path inside a volume")
	}
}

func TestDeleteRefusesWhileSnapshotsRemain(t *testing.T) {
	s, _, dir := open(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "corpus"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Source(ctx, "corpus", "pr1", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "corpus"); !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), "pr1") {
		t.Fatalf("delete with a snapshot: %v", err)
	}
	if err := s.DeleteSnapshot(ctx, "corpus", "pr2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete a missing snapshot: %v", err)
	}
	if err := s.DeleteSnapshot(ctx, "corpus", "pr1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "corpus"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "corpus")); !os.IsNotExist(err) {
		t.Fatalf("volume directory left: %v", err)
	}
	if err := s.Delete(ctx, "corpus"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again: %v", err)
	}
}

func TestOpenNeedsARealDirectory(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, dir+"/link"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", dir + "/", dir + "/link", dir + "/missing"} {
		if _, err := Open(path, &volumestest.Subvolumes{}); err == nil {
			t.Errorf("opened %q", path)
		}
	}
}
