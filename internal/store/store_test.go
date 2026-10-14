package store

import (
	"os"
	"path/filepath"
	"testing"

	"site-deploy/internal/model"
)

func TestExclusiveLockAndDurableRecords(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := Open(dir); err == nil {
		duplicate.Close()
		t.Fatal("second daemon acquired same data directory")
	}
	a := model.App{Name: "example", State: model.StateSuspended}
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	a.State = model.StateRunning
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	apps, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].State != model.StateRunning {
		t.Fatalf("bad durable records: %+v", apps)
	}
	info, err := os.Stat(filepath.Join(dir, "state", "example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("record permissions: %v", info.Mode())
	}
	if err := s.Delete(a.Name); err != nil {
		t.Fatal(err)
	}
	apps, err = s.Load()
	if err != nil || len(apps) != 0 {
		t.Fatalf("delete: %+v %v", apps, err)
	}
}

func TestRejectCorruptAndMismatchedRecords(t *testing.T) {
	for _, data := range []string{`not json`, `{"name":"another"}`, `{"name":"../escape"}`} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := os.WriteFile(filepath.Join(dir, "state", "example.json"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(); err == nil {
				t.Fatal("accepted corrupt record")
			}
		})
	}
}
