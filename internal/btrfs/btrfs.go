// Package btrfs creates, snapshots and deletes btrfs subvolumes with the
// kernel's ioctls, so site-deploy needs no btrfs-progs.
//
// The daemon doesn't call these itself: it runs this executable's helper
// command (Helper) as root in a short-lived container, because subvolumes hold
// files written by containers as other users, and the daemon's own user could
// neither snapshot a subvolume it doesn't own nor delete one without
// CAP_SYS_ADMIN or the user_subvol_rm_allowed mount option.
package btrfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// SuperMagic is the statfs type of a btrfs filesystem.
const SuperMagic = 0x9123683E

// firstFreeObjectID is the inode number of every subvolume's root directory.
const firstFreeObjectID = 256

// Layouts from linux/btrfs.h. Both structures are 4096 bytes.
const (
	ioctlMagic    = 0x94
	pathNameMax   = 4087
	subvolNameMax = 4039
)

type volArgs struct {
	fd   int64
	name [pathNameMax + 1]byte
}

type volArgsV2 struct {
	fd      int64
	transid uint64
	flags   uint64
	unused  [4]uint64
	name    [subvolNameMax + 1]byte
}

// iow is the kernel's _IOW for the asm-generic encoding that amd64 and arm64 use.
func iow(nr, size uintptr) uintptr { return 1<<30 | size<<16 | ioctlMagic<<8 | nr }

var (
	iocSubvolCreate = iow(14, unsafe.Sizeof(volArgs{}))
	iocSnapDestroy  = iow(15, unsafe.Sizeof(volArgs{}))
	iocSnapCreateV2 = iow(23, unsafe.Sizeof(volArgsV2{}))
)

// OnBtrfs reports whether path is on a btrfs filesystem.
func OnBtrfs(path string) (bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false, &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	return uint32(stat.Type) == SuperMagic, nil
}

// IsSubvolume reports whether path, without following a final symlink, is the
// root directory of a btrfs subvolume.
func IsSubvolume(path string) (bool, error) {
	var stat syscall.Stat_t
	if err := syscall.Lstat(path, &stat); err != nil {
		return false, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Ino != firstFreeObjectID {
		return false, nil
	}
	return OnBtrfs(path)
}

// Create makes path a new, empty subvolume. Its parent must be on btrfs.
func Create(path string) error {
	parent, name, err := split(path)
	if err != nil {
		return err
	}
	dir, err := openDirectory(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	var args volArgs
	copy(args.name[:], name)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dir.Fd(), iocSubvolCreate, uintptr(unsafe.Pointer(&args))); errno != 0 {
		return fmt.Errorf("create subvolume %s: %w", path, errno)
	}
	return nil
}

// Snapshot makes target a writable snapshot of the subvolume source. Both must
// be on the same btrfs filesystem, and target must not exist.
func Snapshot(source, target string) error {
	if ok, err := IsSubvolume(source); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("snapshot %s: not a btrfs subvolume", source)
	}
	src, err := openDirectory(source)
	if err != nil {
		return err
	}
	defer src.Close()
	parent, name, err := split(target)
	if err != nil {
		return err
	}
	dir, err := openDirectory(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	var args volArgsV2
	args.fd = int64(src.Fd())
	copy(args.name[:], name)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dir.Fd(), iocSnapCreateV2, uintptr(unsafe.Pointer(&args))); errno != 0 {
		return fmt.Errorf("snapshot %s as %s: %w", source, target, errno)
	}
	return nil
}

// Delete removes the subvolume path and everything in it. Without
// CAP_SYS_ADMIN or the user_subvol_rm_allowed mount option the kernel refuses
// the ioctl, as it does for a subvolume holding other subvolumes. Removing the
// contents, then the empty subvolume with rmdir, which Linux 4.18 and later
// allow, does the same more slowly.
func Delete(path string) error {
	if ok, err := IsSubvolume(path); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("delete %s: not a btrfs subvolume", path)
	}
	parent, name, err := split(path)
	if err != nil {
		return err
	}
	dir, err := openDirectory(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	var args volArgs
	copy(args.name[:], name)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dir.Fd(), iocSnapDestroy, uintptr(unsafe.Pointer(&args)))
	switch errno {
	case 0:
		return nil
	case syscall.EPERM, syscall.EACCES, syscall.ENOTEMPTY:
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("delete subvolume %s: %w", path, err)
		}
		return nil
	default:
		return fmt.Errorf("delete subvolume %s: %w", path, errno)
	}
}

// split returns path's parent directory and final component, which the
// ioctls take separately.
func split(path string) (string, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", fmt.Errorf("subvolume path %q must be clean and absolute", path)
	}
	parent, name := filepath.Split(path)
	if name == "" || len(name) > subvolNameMax || strings.ContainsRune(name, 0) {
		return "", "", fmt.Errorf("invalid subvolume name in %q", path)
	}
	return filepath.Clean(parent), name, nil
}

func openDirectory(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink", path)
		}
		return nil, err
	}
	return file, nil
}
