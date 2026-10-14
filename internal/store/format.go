package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const formatFile = "format"

// DataFormat is the on-disk layout this build reads and writes. Bump it when a
// release changes persisted records or directory layout incompatibly, and
// migrate older layouts in CheckFormat before the daemon loads any state.
//
//	1: initial layout (0.1.0).
//	2: app specs may carry basic_auth (0.2.0). An older daemon would ignore it
//	   and serve protected apps publicly, so it must refuse this format.
const DataFormat = 2

// CheckFormat records the data directory layout version and refuses a layout
// written by a newer release, so rolling back the binary after an upgrade can
// never misread migrated state. Directories created before this marker existed
// use format 1. Open's exclusive daemon lock protects creation.
func (s *Store) CheckFormat() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return errors.New("cannot check the data format of a closed store")
	}
	root := filepath.Dir(s.directory)
	path := filepath.Join(root, formatFile)
	format, err := readFormat(path)
	if os.IsNotExist(err) {
		if err := installPrivateFile(root, formatFile, strconv.Itoa(DataFormat)+"\n"); err != nil {
			return fmt.Errorf("record data format: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if format > DataFormat {
		return fmt.Errorf("data directory format %d was written by a newer site-deploy release (this build supports format %d); install that release, or restore the data backup taken before upgrading", format, DataFormat)
	}
	if format < DataFormat {
		// Apply migrations in order here. 1 -> 2 needs no record changes:
		// specs without basic_auth decode unchanged.
		if err := installPrivateFile(root, formatFile, strconv.Itoa(DataFormat)+"\n"); err != nil {
			return fmt.Errorf("upgrade data format %d to %d: %w", format, DataFormat, err)
		}
	}
	return nil
}

func readFormat(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return 0, errors.New("data format marker must be a private regular file (chmod 600)")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, fmt.Errorf("open data format marker: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16))
	if err != nil {
		return 0, fmt.Errorf("read data format marker: %w", err)
	}
	format, err := strconv.Atoi(strings.TrimSuffix(string(data), "\n"))
	if err != nil || format < 1 {
		return 0, errors.New("invalid data format marker; restore the original format file")
	}
	return format, nil
}

// installPrivateFile atomically creates or replaces root/name with an
// owner-only file, then syncs the directory so the rename is durable.
func installPrivateFile(root, name, content string) error {
	file, err := os.CreateTemp(root, "."+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(content); err != nil {
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
	if err := os.Rename(file.Name(), filepath.Join(root, name)); err != nil {
		return err
	}
	return syncDirectory(root)
}
