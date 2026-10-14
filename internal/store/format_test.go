package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatRecordedAndNewerFormatRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckFormat(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, formatFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("format marker permissions: %v, %v", info, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "2\n" {
		t.Fatalf("format marker = %q", data)
	}
	// A restart with the same build accepts its own marker.
	if err := s.CheckFormat(); err != nil {
		t.Fatal(err)
	}
	// A 0.1.0 data directory is upgraded in place.
	if err := os.WriteFile(path, []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckFormat(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "2\n" {
		t.Fatalf("format 1 not upgraded: %q", data)
	}
	// A data directory migrated by a newer release must not be misread.
	if err := os.WriteFile(path, []byte("3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckFormat(); err == nil || !strings.Contains(err.Error(), "newer site-deploy release") {
		t.Fatalf("newer format accepted: %v", err)
	}
	for _, corrupt := range []string{"", "0\n", "one\n"} {
		if err := os.WriteFile(path, []byte(corrupt), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.CheckFormat(); err == nil {
			t.Errorf("corrupt format %q accepted", corrupt)
		}
	}
	if err := os.WriteFile(path, []byte("2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckFormat(); err == nil {
		t.Error("world-readable format marker accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckFormat(); err == nil {
		t.Error("closed store checked its format")
	}
}
