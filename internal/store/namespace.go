package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const projectPrefixFile = "project-prefix"

var projectPrefixPattern = regexp.MustCompile(`^sd-[0-9a-f]{24}-$`)

// HasLegacyLayout must run before Open creates the state directory. An existing
// state directory, even an empty one, may own sd-NAME volumes from deleted apps.
// Preserve that namespace instead of silently disconnecting persistent data.
func HasLegacyLayout(root string) (bool, error) {
	info, err := os.Lstat(filepath.Join(root, "state"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect existing state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("state directory must be a real directory")
	}
	return true, nil
}

// ProjectPrefix returns the durable Docker project namespace for this data
// directory. Open's exclusive daemon lock and this store mutex protect creation.
// A stored prefix always takes precedence over the initial legacy-layout check.
func (s *Store) ProjectPrefix(legacy bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return "", errors.New("cannot load project namespace from a closed store")
	}
	root := filepath.Dir(s.directory)
	path := filepath.Join(root, projectPrefixFile)
	prefix, err := readProjectPrefix(path)
	if err == nil {
		return prefix, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	prefix = "sd-"
	if !legacy {
		var identity [12]byte
		if _, err := rand.Read(identity[:]); err != nil {
			return "", fmt.Errorf("create Docker project namespace: %w", err)
		}
		prefix += hex.EncodeToString(identity[:]) + "-"
	}
	if err := installPrivateFile(root, projectPrefixFile, prefix+"\n"); err != nil {
		return "", fmt.Errorf("install project namespace: %w", err)
	}
	return prefix, nil
}

func readProjectPrefix(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("project namespace must be a private regular file (chmod 600)")
	}
	// Do not follow a symlink swapped in after inspection. The containing data
	// directory is private, but its namespace determines every Docker operation.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("open project namespace: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect project namespace: %w", err)
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return "", errors.New("project namespace changed during inspection")
	}
	data, err := io.ReadAll(io.LimitReader(file, 33))
	if err != nil {
		return "", fmt.Errorf("read project namespace: %w", err)
	}
	prefix := strings.TrimSuffix(string(data), "\n")
	if prefix != "sd-" && !projectPrefixPattern.MatchString(prefix) {
		return "", errors.New("invalid Docker project namespace; restore the original project-prefix file")
	}
	return prefix, nil
}
