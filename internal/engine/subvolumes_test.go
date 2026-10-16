package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSubvolumesRunTheHelperAsRootWithOnlyTheVolumesDirectory(t *testing.T) {
	docker := New("docker", "unix:///run/test-docker.sock")
	var calls [][]string
	docker.run = func(_ context.Context, invocation command) (commandResult, error) {
		args := invocation.args[2:] // after --host HOST
		calls = append(calls, args)
		if args[0] == "image" {
			return commandResult{}, errors.New("no such image")
		}
		return commandResult{}, nil
	}
	subvolumes, err := NewSubvolumes(docker, "/srv/volumes", "/usr/local/lib/site-deploy/0.9.0/site-deploy")
	if err != nil {
		t.Fatal(err)
	}
	if err := subvolumes.Snapshot(context.Background(), "/srv/volumes/corpus/data", "/srv/volumes/corpus/snapshots/pr1"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"image", "inspect", "--format", "{{.Id}}", DefaultCopyImage},
		{"pull", "--quiet", DefaultCopyImage},
		{"run", "--rm", "--network", "none", "--user", "0:0", "--entrypoint", "/site-deploy",
			"--mount", "type=bind,source=/usr/local/lib/site-deploy/0.9.0/site-deploy,target=/site-deploy,readonly",
			"--mount", "type=bind,source=/srv/volumes,target=/volumes",
			DefaultCopyImage, "btrfs-helper", "snapshot", "/volumes/corpus/data", "/volumes/corpus/snapshots/pr1"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls\n%q\nwant\n%q", calls, want)
	}
	calls = nil
	for _, path := range []string{"/srv/volumes", "/srv/other/data", "/srv/volumes/../x"} {
		if err := subvolumes.Create(context.Background(), path); err == nil || !strings.Contains(err.Error(), "outside the volumes directory") {
			t.Errorf("Create(%s): %v", path, err)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("ran Docker for paths outside the volumes directory: %q", calls)
	}
	docker.run = func(_ context.Context, invocation command) (commandResult, error) {
		if invocation.args[2] == "run" {
			return commandResult{stderr: []byte("site-deploy btrfs-helper: delete /volumes/x: not a btrfs subvolume\n")}, errors.New("exit status 1")
		}
		return commandResult{}, nil
	}
	if err := subvolumes.Delete(context.Background(), "/srv/volumes/x"); err == nil || !strings.Contains(err.Error(), "btrfs delete: docker run failed: exit status 1: site-deploy btrfs-helper: delete /volumes/x: not a btrfs subvolume") {
		t.Fatalf("helper failure: %v", err)
	}
	for _, paths := range [][2]string{{"relative", "/bin/site-deploy"}, {"/srv/volumes", "/bin/site,deploy"}, {"/srv/volumes/", "/bin/site-deploy"}} {
		if _, err := NewSubvolumes(docker, paths[0], paths[1]); err == nil {
			t.Errorf("NewSubvolumes(%q, %q) accepted", paths[0], paths[1])
		}
	}
}
