package orchestrator

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"site-deploy/internal/model"
)

const maxStoredComposeBytes = 8 << 20

// Control files live beside apps, never inside a directory an application can
// bind mount. Restrict directory access even when the daemon has a loose umask.
func privateStorageDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("storage directory must be a real directory: %w", err)
	}
	defer dir.Close()
	return dir.Chmod(0700)
}

func (m *Manager) composePath(name string) string {
	return filepath.Join(m.directory, "compose", name+".json")
}

func openComposeFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxStoredComposeBytes {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("generated Compose must be a regular file no larger than 8 MiB")
	}
	return file, nil
}

func checkProtectedCompose(path string) error {
	file, err := openComposeFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return errors.New("protected Compose must not have hard links")
	}
	return file.Chmod(0600)
}

// copyCompose installs a new inode. Renaming a legacy file would leave any
// application-held descriptors or hard links able to modify trusted control
// data after migration. The daemon lock protects this private destination.
func copyCompose(source, destination string) error {
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("protected Compose file already exists")
	}
	sourceFile, err := openComposeFile(source)
	if err != nil {
		return err
	}
	defer sourceFile.Close()
	data, err := io.ReadAll(io.LimitReader(sourceFile, maxStoredComposeBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxStoredComposeBytes {
		return errors.New("generated Compose exceeds 8 MiB")
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".compose-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
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
	if err := os.Rename(file.Name(), destination); err != nil {
		return err
	}
	return syncStorageDirectory(filepath.Dir(destination))
}

func syncStorageDirectory(path string) error {
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// prepareStoredCompose accepts only the historical bundle-local path or the
// protected path. Existing protected files win after an interrupted migration;
// a mutable legacy file must never overwrite a completed protected copy.
func (m *Manager) prepareStoredCompose(app *model.App) error {
	legacy := filepath.Join(app.Directory, "compose.generated.json")
	protected := m.composePath(app.Name)
	if app.ComposeFile != legacy && app.ComposeFile != protected {
		return errors.New("state refers to an unexpected Compose file")
	}
	if app.State == model.StateDeleting {
		return nil // Cleanup may already have removed either file or the bundle.
	}
	info, err := os.Lstat(app.Directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("application bundle must be an existing real directory")
	}
	if app.ComposeFile == protected {
		return checkProtectedCompose(protected)
	}
	if _, err := os.Lstat(protected); errors.Is(err, os.ErrNotExist) {
		if err := copyCompose(legacy, protected); err != nil {
			return fmt.Errorf("migrate generated Compose: %w", err)
		}
	} else if err != nil {
		return err
	}
	if err := checkProtectedCompose(protected); err != nil {
		return err
	}
	app.ComposeFile = protected
	return nil
}
