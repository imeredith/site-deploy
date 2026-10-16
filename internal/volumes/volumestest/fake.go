// Package volumestest fakes btrfs subvolumes with plain directories, for
// tests of code that uses site-deploy volumes on filesystems other than btrfs.
package volumestest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
)

// Subvolumes implements volumes.Subvolumes. A snapshot is a copy. Like btrfs,
// it knows subvolumes by inode, so a directory put in a subvolume's place
// isn't one.
type Subvolumes struct {
	mu         sync.Mutex
	subvolumes map[uint64]bool
	snapshots  int
	// Fail, when set, is returned by every change.
	Fail error
}

func (f *Subvolumes) Create(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail != nil {
		return f.Fail
	}
	if err := os.Mkdir(path, 0755); err != nil {
		return err
	}
	return f.mark(path, true)
}

func (f *Subvolumes) Snapshot(_ context.Context, source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail != nil {
		return f.Fail
	}
	if !f.is(source) {
		return fmt.Errorf("snapshot %s: not a subvolume", source)
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("snapshot %s: %s exists", source, target)
	}
	if err := os.CopyFS(target, os.DirFS(source)); err != nil {
		return err
	}
	f.snapshots++
	return f.mark(target, true)
}

func (f *Subvolumes) Delete(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail != nil {
		return f.Fail
	}
	if !f.is(path) {
		return fmt.Errorf("delete %s: not a subvolume", path)
	}
	if err := f.mark(path, false); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func (f *Subvolumes) IsSubvolume(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return info.IsDir() && f.is(path), nil
}

// Snapshots counts the snapshots taken.
func (f *Subvolumes) Snapshots() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshots
}

func (f *Subvolumes) mark(path string, subvolume bool) error {
	inode, err := inode(path)
	if err != nil {
		return err
	}
	if f.subvolumes == nil {
		f.subvolumes = map[uint64]bool{}
	}
	if subvolume {
		f.subvolumes[inode] = true
	} else {
		delete(f.subvolumes, inode)
	}
	return nil
}

func (f *Subvolumes) is(path string) bool {
	inode, err := inode(path)
	return err == nil && f.subvolumes[inode]
}

func inode(path string) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Lstat(path, &stat); err != nil {
		return 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	return stat.Ino, nil
}

// ErrInjected is a ready-made failure for Fail.
var ErrInjected = errors.New("injected subvolume failure")
