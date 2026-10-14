// Package store owns durable app records and an exclusive daemon lock.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"

	"site-deploy/internal/model"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

func ValidName(name string) bool { return namePattern.MatchString(name) }

type Store struct {
	directory string
	lock      *os.File
	mu        sync.Mutex
}

func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("data directory must be a real directory")
	}
	f, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("data directory is already in use by another daemon: %w", err)
	}
	dir := filepath.Join(root, "state")
	if err := os.MkdirAll(dir, 0700); err != nil {
		f.Close()
		return nil, err
	}
	return &Store{directory: dir, lock: f}, nil
}

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

func (s *Store) Load() ([]model.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := os.ReadDir(s.directory)
	if err != nil {
		return nil, err
	}
	apps := make([]model.App, 0, len(files))
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		if !file.Type().IsRegular() {
			return nil, fmt.Errorf("state record %s is not a regular file", file.Name())
		}
		data, err := os.ReadFile(filepath.Join(s.directory, file.Name()))
		if err != nil {
			return nil, err
		}
		var app model.App
		if err := json.Unmarshal(data, &app); err != nil {
			return nil, fmt.Errorf("read state %s: %w", file.Name(), err)
		}
		if !ValidName(app.Name) || app.Name+".json" != file.Name() {
			return nil, fmt.Errorf("invalid app name in %s", file.Name())
		}
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return apps, nil
}

func (s *Store) Save(app model.App) error {
	if !ValidName(app.Name) {
		return errors.New("invalid application name")
	}
	data, err := json.MarshalIndent(app, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.CreateTemp(s.directory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(s.directory, app.Name+".json")); err != nil {
		return err
	}
	return syncDirectory(s.directory)
}

func (s *Store) Delete(name string) error {
	if !ValidName(name) {
		return errors.New("invalid application name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(filepath.Join(s.directory, name+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(s.directory)
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
