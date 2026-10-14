package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceAgentCIndependentStoresRetainTheirIdentity(t *testing.T) {
	t.Parallel()
	firstRoot, secondRoot := t.TempDir(), t.TempDir()
	first := namespaceAgentCOpen(t, firstRoot)
	second := namespaceAgentCOpen(t, secondRoot)
	firstPrefix, err := first.ProjectPrefix(false)
	if err != nil {
		t.Fatal(err)
	}
	secondPrefix, err := second.ProjectPrefix(false)
	if err != nil {
		t.Fatal(err)
	}
	if firstPrefix == secondPrefix || firstPrefix == "sd-" || secondPrefix == "sd-" {
		t.Fatalf("independent installations share a namespace: %q, %q", firstPrefix, secondPrefix)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := HasLegacyLayout(firstRoot)
	if err != nil || !legacy {
		t.Fatalf("existing state layout: legacy=%v err=%v", legacy, err)
	}
	first = namespaceAgentCOpen(t, firstRoot)
	reloaded, err := first.ProjectPrefix(legacy)
	if err != nil || reloaded != firstPrefix {
		t.Fatalf("restart changed Docker project namespace: %q -> %q (%v)", firstPrefix, reloaded, err)
	}
	info, err := os.Stat(filepath.Join(firstRoot, projectPrefixFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("project namespace permissions: %v, %v", info, err)
	}
}

func TestNamespaceAgentCEmptyLegacyStateRetainsDeletedVolumeNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	legacy, err := HasLegacyLayout(root)
	if err != nil || !legacy {
		t.Fatalf("legacy empty state not recognized: %v, %v", legacy, err)
	}
	s := namespaceAgentCOpen(t, root)
	prefix, err := s.ProjectPrefix(legacy)
	if err != nil || prefix != "sd-" {
		t.Fatalf("legacy project identity changed: %q, %v", prefix, err)
	}
	data, err := os.ReadFile(filepath.Join(root, projectPrefixFile))
	if err != nil || string(data) != "sd-\n" {
		t.Fatalf("legacy identity was not persisted: %q, %v", data, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = namespaceAgentCOpen(t, root)
	prefix, err = s.ProjectPrefix(false)
	if err != nil || prefix != "sd-" {
		t.Fatalf("persisted legacy identity ignored: %q, %v", prefix, err)
	}
}

func TestNamespaceAgentCUnsafeMarkerFailsWithoutReplacingIt(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"empty", "invalid", "oversized", "public", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			s := namespaceAgentCOpen(t, root)
			path := filepath.Join(root, projectPrefixFile)
			data := "sd-\n"
			perm := os.FileMode(0600)
			switch kind {
			case "empty":
				data = ""
			case "invalid":
				data = "sd-../other-\n"
			case "oversized":
				data = strings.Repeat("x", 1024)
			case "public":
				perm = 0644
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "namespace-agent-c-target")
				if err := os.WriteFile(target, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "directory" && kind != "symlink" {
				if err := os.WriteFile(path, []byte(data), perm); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, perm); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ProjectPrefix(false); err == nil {
				t.Fatal("accepted unsafe project namespace")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("unsafe namespace marker was replaced: %v", err)
			}
		})
	}
}

func TestNamespaceAgentCLegacyDetectionAndStoreOwnership(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if legacy, err := HasLegacyLayout(root); err != nil || legacy {
		t.Fatalf("fresh data directory marked legacy: %v, %v", legacy, err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "state")); err != nil {
		t.Fatal(err)
	}
	if _, err := HasLegacyLayout(root); err == nil {
		t.Fatal("accepted symlink state directory")
	}
	s := namespaceAgentCOpen(t, t.TempDir())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectPrefix(false); err == nil {
		t.Fatal("created namespace after releasing store ownership")
	}
}

func TestNamespaceAgentCConcurrentInitializationUsesOneIdentity(t *testing.T) {
	t.Parallel()
	s := namespaceAgentCOpen(t, t.TempDir())
	type result struct {
		prefix string
		err    error
	}
	const callers = 16
	results := make(chan result, callers)
	for i := 0; i < callers; i++ {
		go func() {
			prefix, err := s.ProjectPrefix(false)
			results <- result{prefix: prefix, err: err}
		}()
	}
	var expected string
	for i := 0; i < callers; i++ {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if i == 0 {
			expected = got.prefix
		} else if got.prefix != expected {
			t.Fatalf("concurrent initializations disagreed: %q and %q", expected, got.prefix)
		}
	}
}

func namespaceAgentCOpen(t *testing.T, root string) *Store {
	t.Helper()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
