// Package volumes keeps site-deploy volumes: btrfs subvolumes that exist apart
// from any deployment. A bundle names a volume to bind-mount the volume itself,
// or its own snapshot of it. An application's snapshot is made on its first
// deploy and kept, so redeploying the application mounts it again.
//
//	DIR/
//	  .lock
//	  NAME/
//	    volume.json       the creation time; written last, so it marks a complete volume
//	    data/             subvolume: the volume
//	    snapshots/APP/    subvolume: APP's snapshot of data
//
// Applications see only data or their snapshot, never volume.json.
package volumes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"site-deploy/internal/store"
)

var (
	ErrNotFound = errors.New("volume not found")
	ErrExists   = errors.New("volume already exists")
	ErrInUse    = errors.New("volume is in use")
)

// Subvolumes performs the btrfs operations. The daemon's implementation makes
// changes as root in a helper container; tests fake it with plain directories.
type Subvolumes interface {
	Create(ctx context.Context, path string) error
	Snapshot(ctx context.Context, source, target string) error
	Delete(ctx context.Context, path string) error
	// IsSubvolume reports whether path is a subvolume's root directory.
	IsSubvolume(path string) (bool, error)
}

// Volume describes a volume for the API.
type Volume struct {
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Created   time.Time  `json:"created"`
	Snapshots []Snapshot `json:"snapshots"`
	// UsedBy lists the deployed applications that mount the volume or one of
	// its snapshots. The orchestrator fills it in.
	UsedBy []Use `json:"used_by"`
}

// Snapshot is an application's snapshot of a volume.
type Snapshot struct {
	App  string `json:"app"`
	Path string `json:"path"`
}

// Use is a deployed application's mount of a volume.
type Use struct {
	App      string `json:"app"`
	Snapshot bool   `json:"snapshot"`
}

// metadata is volume.json. Volumes made by 0.9.0 also hold a key_sha256,
// which is no longer used.
type metadata struct {
	Created time.Time `json:"created"`
}

const metadataFile = "volume.json"

// Store manages the volumes in one directory. Its changes are serialized;
// Check, which every start of an application with a volume runs, only reads,
// so a long deletion never delays a wake-up.
type Store struct {
	dir        string
	subvolumes Subvolumes
	lock       *os.File
	mu         sync.Mutex
}

// Open takes an exclusive lock on dir, which must be an existing real
// directory named by a clean absolute path. The caller checks it's on btrfs.
func Open(dir string, subvolumes Subvolumes) (*Store, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, fmt.Errorf("volumes directory %q must be a clean absolute path", dir)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err != nil {
		return nil, fmt.Errorf("volumes directory: %w", err)
	} else if resolved != dir {
		return nil, fmt.Errorf("volumes directory %s goes through a symlink; use %s", dir, resolved)
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("volumes directory: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("volumes directory %s is already in use by another daemon: %w", dir, err)
	}
	return &Store{dir: dir, subvolumes: subvolumes, lock: lock}, nil
}

// Close releases the directory lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

// Dir is the volumes directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) dataPath(name string) string { return filepath.Join(s.dir, name, "data") }

func (s *Store) snapshotPath(name, app string) string {
	return filepath.Join(s.dir, name, "snapshots", app)
}

// Create makes an empty volume.
func (s *Store) Create(ctx context.Context, name string) (Volume, error) {
	if !store.ValidName(name) {
		return Volume{}, invalidName(name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.dir, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			if _, loadErr := s.load(name); errors.Is(loadErr, ErrNotFound) {
				return Volume{}, fmt.Errorf("%w: %s holds an incomplete volume, left by an interrupted create; delete it first", ErrExists, name)
			}
			return Volume{}, fmt.Errorf("%w: %s", ErrExists, name)
		}
		return Volume{}, err
	}
	created := false
	defer func() {
		if !created {
			if _, err := os.Lstat(s.dataPath(name)); err == nil {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
				s.subvolumes.Delete(cleanup, s.dataPath(name))
				cancel()
			}
			os.RemoveAll(dir)
		}
	}()
	if err := os.Mkdir(filepath.Join(dir, "snapshots"), 0700); err != nil {
		return Volume{}, err
	}
	if err := s.subvolumes.Create(ctx, s.dataPath(name)); err != nil {
		return Volume{}, err
	}
	meta := metadata{Created: time.Now().UTC()}
	if err := s.save(name, meta); err != nil {
		return Volume{}, err
	}
	created = true
	return Volume{Name: name, Path: s.dataPath(name), Created: meta.Created, Snapshots: []Snapshot{}, UsedBy: []Use{}}, nil
}

// List returns every complete volume with its snapshots, sorted by name.
func (s *Store) List() ([]Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	volumes := []Volume{}
	for _, entry := range entries {
		if !entry.IsDir() || !store.ValidName(entry.Name()) {
			continue
		}
		meta, err := s.load(entry.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		volume := Volume{Name: entry.Name(), Path: s.dataPath(entry.Name()), Created: meta.Created, Snapshots: []Snapshot{}, UsedBy: []Use{}}
		snapshots, err := os.ReadDir(filepath.Join(s.dir, entry.Name(), "snapshots"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, snapshot := range snapshots {
			if snapshot.IsDir() && store.ValidName(snapshot.Name()) {
				volume.Snapshots = append(volume.Snapshots, Snapshot{App: snapshot.Name(), Path: s.snapshotPath(entry.Name(), snapshot.Name())})
			}
		}
		volumes = append(volumes, volume)
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
	return volumes, nil
}

// Source returns the host path that app bind-mounts: the volume, or with
// snapshot, app's snapshot of it. app's first deploy makes the snapshot; later
// deploys mount the same one.
func (s *Store) Source(ctx context.Context, name, app string, snapshot bool) (string, error) {
	if !store.ValidName(name) {
		return "", invalidName(name)
	}
	if !store.ValidName(app) {
		return "", fmt.Errorf("invalid application name %q", app)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.load(name); err != nil {
		return "", err
	}
	data := s.dataPath(name)
	if err := s.checkSubvolume(data); err != nil {
		return "", err
	}
	if !snapshot {
		return data, nil
	}
	target := s.snapshotPath(name, app)
	if _, err := os.Lstat(target); err == nil {
		return target, s.checkSubvolume(target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := s.subvolumes.Snapshot(ctx, data, target); err != nil {
		return "", fmt.Errorf("snapshot volume %s for %s: %w", name, app, err)
	}
	return target, nil
}

// Contains reports whether path is inside the volumes directory.
func (s *Store) Contains(path string) bool {
	return strings.HasPrefix(filepath.Clean(path), s.dir+"/")
}

// Locate parses a path inside the volumes directory: a volume's data, or an
// application's snapshot of it.
func (s *Store) Locate(path string) (name, snapshotApp string, ok bool) {
	if !s.Contains(path) || filepath.Clean(path) != path {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, s.dir+"/"), "/")
	switch {
	case len(parts) == 2 && parts[1] == "data" && store.ValidName(parts[0]):
		return parts[0], "", true
	case len(parts) == 3 && parts[1] == "snapshots" && store.ValidName(parts[0]) && store.ValidName(parts[2]):
		return parts[0], parts[2], true
	}
	return "", "", false
}

// Check confirms, before a start, that source is still a volume app may
// mount: the volume's data, or app's own snapshot, as a real subvolume.
func (s *Store) Check(app, source string) error {
	name, snapshotApp, ok := s.Locate(source)
	if !ok {
		return fmt.Errorf("%s is not a site-deploy volume or snapshot", source)
	}
	if snapshotApp != "" && snapshotApp != app {
		return fmt.Errorf("%s is %s's snapshot of volume %s; %s may only mount its own", source, snapshotApp, name, app)
	}
	if _, err := s.load(name); err != nil {
		return err
	}
	return s.checkSubvolume(source)
}

// Delete removes a volume. It refuses while the volume has snapshots, which
// are kept when their applications are deleted so redeploys can reuse them.
// The caller checks that no deployed application mounts the volume.
func (s *Store) Delete(ctx context.Context, name string) error {
	if !store.ValidName(name) {
		return invalidName(name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.dir, name)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	snapshots, err := os.ReadDir(filepath.Join(dir, "snapshots"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(snapshots) > 0 {
		apps := make([]string, 0, len(snapshots))
		for _, snapshot := range snapshots {
			apps = append(apps, snapshot.Name())
		}
		return fmt.Errorf("%w: %s still has snapshots for %s; delete them first", ErrInUse, name, strings.Join(apps, ", "))
	}
	if _, err := os.Lstat(s.dataPath(name)); err == nil {
		if err := s.subvolumes.Delete(ctx, s.dataPath(name)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.RemoveAll(dir)
}

// DeleteSnapshot removes app's snapshot of a volume. The caller checks that
// app doesn't mount it.
func (s *Store) DeleteSnapshot(ctx context.Context, name, app string) error {
	if !store.ValidName(name) {
		return invalidName(name)
	}
	if !store.ValidName(app) {
		return fmt.Errorf("invalid application name %q", app)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.snapshotPath(name, app)
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s has no snapshot for %s", ErrNotFound, name, app)
	} else if err != nil {
		return err
	}
	return s.subvolumes.Delete(ctx, target)
}

// checkSubvolume refuses a path reached through a symlink, or one that isn't a
// subvolume, which only something outside site-deploy could have put there.
func (s *Store) checkSubvolume(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	if resolved != path {
		return fmt.Errorf("%s goes through a symlink", path)
	}
	ok, err := s.subvolumes.IsSubvolume(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not a btrfs subvolume", path)
	}
	return nil
}

func (s *Store) load(name string) (metadata, error) {
	var meta metadata
	file, err := os.OpenFile(filepath.Join(s.dir, name, metadataFile), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return meta, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return meta, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("read volume %s: %w", name, err)
	}
	return meta, nil
}

// save replaces NAME/volume.json atomically with an owner-only file.
func (s *Store) save(name string, meta metadata) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.dir, name)
	file, err := os.CreateTemp(dir, ".volume-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, metadataFile)); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func invalidName(name string) error {
	return fmt.Errorf("volume name %q must start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens", name)
}
