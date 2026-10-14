package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func TestUnusedKeepsSharedRecentAndUnknown(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-48*time.Hour), now.Add(-time.Hour)
	cutoff := now.Add(-24 * time.Hour)
	images := []Image{
		// Same manifest as a used tag: deleting it would delete the used tag.
		{Repository: "base", Tag: "old-alias", Digest: digestOf('a'), Pushed: old},
		{Repository: "base", Tag: "current", Digest: digestOf('a'), Pushed: old, UsedBy: []string{"blog"}},
		// Unused and old: both tags of the manifest go together.
		{Repository: "base", Tag: "v1", Digest: digestOf('b'), Pushed: old},
		{Repository: "base", Tag: "v1-copy", Digest: digestOf('b'), Pushed: old},
		// Unused but pushed recently, for example just before its deploy.
		{Repository: "base", Tag: "next", Digest: digestOf('c'), Pushed: recent},
		// Unused, old tag sharing a manifest with a recent push.
		{Repository: "base", Tag: "stale", Digest: digestOf('d'), Pushed: old},
		{Repository: "base", Tag: "fresh", Digest: digestOf('d'), Pushed: recent},
		// Unknown push time counts as recent.
		{Repository: "other", Tag: "1", Digest: digestOf('e')},
		// Same digest in another repository is a separate manifest.
		{Repository: "other", Tag: "2", Digest: digestOf('b'), Pushed: old},
	}
	got := unused(images, cutoff)
	want := []Manifest{
		{Repository: "base", Digest: digestOf('b'), Tags: []string{"v1", "v1-copy"}},
		{Repository: "other", Digest: digestOf('b'), Tags: []string{"2"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unused = %+v\nwant %+v", got, want)
	}
}

func TestLocalReferences(t *testing.T) {
	r := &Registry{address: "127.0.0.1:9082"}
	tags, digests := r.localReferences(map[string][]string{
		"127.0.0.1:9082/app-base:2026-10-13":     {"blog"},
		"127.0.0.1:9082/team/app":                {"a", "b"},
		"127.0.0.1:9082/team/app:latest":         {"c"},
		"127.0.0.1:9082/pinned@" + digestOf('f'): {"d"},
		"127.0.0.1:9083/elsewhere:1":             {"x"},
		"nginx:alpine":                           {"y"},
		"ghcr.io/example/app:1":                  {"z"},
	})
	if !reflect.DeepEqual(tags, map[string][]string{"app-base:2026-10-13": {"blog"}, "team/app:latest": {"a", "b", "c"}}) &&
		!reflect.DeepEqual(tags, map[string][]string{"app-base:2026-10-13": {"blog"}, "team/app:latest": {"c", "a", "b"}}) {
		t.Fatalf("tag references = %v", tags)
	}
	if !reflect.DeepEqual(digests, map[string][]string{"pinned@" + digestOf('f'): {"d"}}) {
		t.Fatalf("digest references = %v", digests)
	}
}

// fakeRegistry implements the distribution API endpoints the client uses.
type fakeRegistry struct {
	mu      sync.Mutex
	tags    map[string]map[string]string // repository -> tag -> digest
	deleted []string
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/v2/":
		w.WriteHeader(http.StatusOK)
	case path == "/v2/_catalog":
		var repositories []string
		for repository := range f.tags {
			repositories = append(repositories, repository)
		}
		sort.Strings(repositories)
		// One repository per page, to exercise pagination.
		start := 0
		if last := r.URL.Query().Get("last"); last != "" {
			start = sort.SearchStrings(repositories, last) + 1
		}
		page := repositories[start:min(start+1, len(repositories))]
		if start+1 < len(repositories) {
			w.Header().Set("Link", fmt.Sprintf(`</v2/_catalog?last=%s&n=1>; rel="next"`, page[0]))
		}
		fmt.Fprintf(w, `{"repositories":%s}`, jsonList(page))
	case strings.HasSuffix(path, "/tags/list"):
		repository := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/tags/list")
		var tags []string
		for tag := range f.tags[repository] {
			tags = append(tags, tag)
		}
		sort.Strings(tags)
		fmt.Fprintf(w, `{"name":%q,"tags":%s}`, repository, jsonList(tags))
	case strings.Contains(path, "/manifests/"):
		name, reference, _ := strings.Cut(strings.TrimPrefix(path, "/v2/"), "/manifests/")
		switch r.Method {
		case http.MethodHead:
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				w.WriteHeader(http.StatusNotAcceptable)
				return
			}
			digest, ok := f.tags[name][reference]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
		case http.MethodDelete:
			f.deleted = append(f.deleted, name+"@"+reference)
			for tag, digest := range f.tags[name] {
				if digest == reference {
					delete(f.tags[name], tag)
				}
			}
			w.WriteHeader(http.StatusAccepted)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func jsonList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	return `["` + strings.Join(items, `","`) + `"]`
}

// fakeDocker records commands and answers container inspection.
type fakeDocker struct {
	mu       sync.Mutex
	commands []string
	inspect  func() ([]byte, error)
	fail     map[string]error
	output   map[string]string
}

func (f *fakeDocker) Command(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(args, " ")
	f.commands = append(f.commands, line)
	if err := f.fail[args[0]]; err != nil {
		return nil, err
	}
	if args[0] == "container" && f.inspect != nil {
		return f.inspect()
	}
	return []byte(f.output[args[0]]), nil
}

func (f *fakeDocker) verbs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var verbs []string
	for _, command := range f.commands {
		verbs = append(verbs, strings.Fields(command)[0])
	}
	return verbs
}

func newTestRegistry(t *testing.T, registry http.Handler, docker Docker) *Registry {
	t.Helper()
	server := httptest.NewServer(registry)
	t.Cleanup(server.Close)
	r, err := NewRegistry(docker, strings.TrimPrefix(server.URL, "http://"), "registry:3", "sd-test-registry", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// touchTag records a push time where registry:3 stores it.
func touchTag(t *testing.T, r *Registry, repository, tag string, at time.Time) {
	t.Helper()
	link := filepath.Join(r.dataDir, "docker/registry/v2/repositories", repository, "_manifests/tags", tag, "current/link")
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(link, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestListAndPruneAgainstRegistryAPI(t *testing.T) {
	fake := &fakeRegistry{tags: map[string]map[string]string{
		"app-base": {"2026-10-01": digestOf('a'), "2026-10-13": digestOf('b')},
		"team/web": {"1": digestOf('c')},
	}}
	docker := &fakeDocker{inspect: func() ([]byte, error) { return nil, errors.New("Error: No such container: sd-test-registry") }}
	r := newTestRegistry(t, fake, docker)
	now := time.Now()
	for repository, tags := range fake.tags {
		for tag := range tags {
			touchTag(t, r, repository, tag, now.Add(-72*time.Hour))
		}
	}
	references := map[string][]string{r.Address() + "/app-base:2026-10-13": {"blog"}, "nginx:alpine": {"web"}}
	images, err := r.List(context.Background(), references)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 3 || images[1].Reference != r.Address()+"/app-base:2026-10-13" || !reflect.DeepEqual(images[1].UsedBy, []string{"blog"}) || images[1].Pushed.IsZero() {
		t.Fatalf("List = %+v", images)
	}
	deleted, collected, err := r.Prune(context.Background(), references, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(deleted)
	if !reflect.DeepEqual(deleted, []string{"app-base:2026-10-01", "team/web:1"}) || !collected {
		t.Fatalf("Prune deleted %v, collected %v", deleted, collected)
	}
	// Garbage collection stops the registry, sweeps offline, then restarts it.
	if verbs := docker.verbs(); !reflect.DeepEqual(verbs, []string{"stop", "run", "container", "run"}) {
		t.Fatalf("docker commands = %q", docker.commands)
	}
	gc := docker.commands[1]
	if !strings.Contains(gc, "--rm") || !strings.Contains(gc, "garbage-collect --delete-untagged /etc/distribution/config.yml") || !strings.Contains(gc, r.dataDir+":/var/lib/registry") {
		t.Fatalf("garbage collection command = %q", gc)
	}
	// Nothing unused and a recent collection: no deletion, no restart.
	docker.commands = nil
	deleted, collected, err = r.Prune(context.Background(), references, 24*time.Hour, now.Add(time.Hour))
	if err != nil || len(deleted) != 0 || collected || len(docker.commands) != 0 {
		t.Fatalf("idle Prune: %v %v %v %q", deleted, collected, err, docker.commands)
	}
	// A day later, collection runs anyway to drop manifests orphaned by re-pushes.
	_, collected, err = r.Prune(context.Background(), references, 24*time.Hour, now.Add(25*time.Hour))
	if err != nil || !collected {
		t.Fatalf("daily collection did not run: %v %v", collected, err)
	}
}

func TestGarbageCollectionFailureStillRestartsRegistry(t *testing.T) {
	fake := &fakeRegistry{tags: map[string]map[string]string{"old": {"1": digestOf('a')}}}
	docker := &fakeDocker{
		inspect: func() ([]byte, error) { return nil, errors.New("Error: No such container: sd-test-registry") },
		fail:    map[string]error{},
	}
	r := newTestRegistry(t, fake, docker)
	touchTag(t, r, "old", "1", time.Now().Add(-72*time.Hour))
	calls := 0
	docker.inspect = func() ([]byte, error) {
		calls++
		return []byte(r.spec() + " false"), nil
	}
	// Fail the sweep itself: docker run is used both for it and never for the
	// restart of an existing container, which uses docker start.
	docker.fail["run"] = errors.New("sweep failed")
	_, collected, err := r.Prune(context.Background(), nil, time.Hour, time.Now())
	if err == nil || !strings.Contains(err.Error(), "sweep failed") || collected {
		t.Fatalf("Prune = %v, %v", collected, err)
	}
	if verbs := docker.verbs(); !reflect.DeepEqual(verbs, []string{"stop", "run", "container", "start"}) || calls != 1 {
		t.Fatalf("registry not restarted after failed collection: %q", docker.commands)
	}
}

func TestEnsureCreatesReplacesAndStarts(t *testing.T) {
	fake := &fakeRegistry{tags: map[string]map[string]string{}}
	docker := &fakeDocker{}
	r := newTestRegistry(t, fake, docker)
	for _, test := range []struct {
		name    string
		inspect func() ([]byte, error)
		verbs   []string
	}{
		{"missing", func() ([]byte, error) { return nil, errors.New("Error response from daemon: No such container: x") }, []string{"container", "run"}},
		{"other spec", func() ([]byte, error) { return []byte("0123456789abcdef true"), nil }, []string{"container", "rm", "run"}},
		{"stopped", func() ([]byte, error) { return []byte(r.spec() + " false"), nil }, []string{"container", "start"}},
		{"running", func() ([]byte, error) { return []byte(r.spec() + " true"), nil }, []string{"container"}},
	} {
		docker.commands, docker.inspect = nil, test.inspect
		if err := r.Ensure(context.Background()); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if verbs := docker.verbs(); !reflect.DeepEqual(verbs, test.verbs) {
			t.Fatalf("%s: docker commands = %q", test.name, docker.commands)
		}
	}
	docker.commands, docker.inspect = nil, func() ([]byte, error) { return nil, errors.New("No such container") }
	if err := r.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := docker.commands[1]
	for _, want := range []string{"--restart unless-stopped", "--publish " + r.Address() + ":5000", "--volume " + r.dataDir + ":/var/lib/registry", "--label site-deploy.registry=" + r.spec(), "--user ", "registry:3"} {
		if !strings.Contains(run, want) {
			t.Errorf("docker run lacks %q: %s", want, run)
		}
	}
	if info, err := os.Stat(r.dataDir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("registry storage: %v %v", info, err)
	}
}

func TestNewRegistryRequiresLoopback(t *testing.T) {
	for _, address := range []string{"", "0.0.0.0:9082", "192.0.2.1:9082", "localhost:9082", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:70000"} {
		if _, err := NewRegistry(&fakeDocker{}, address, "registry:3", "x", "/tmp/x"); err == nil {
			t.Errorf("accepted registry address %q", address)
		}
	}
	if _, err := NewRegistry(&fakeDocker{}, "127.0.0.1:9082", "registry:3", "x", "relative"); err == nil {
		t.Error("accepted a relative data directory")
	}
}

func TestServiceCleanupPrunesDockerAndSkipsDuringStarts(t *testing.T) {
	docker := &fakeDocker{output: map[string]string{"image": "Deleted Images:\nuntagged: nginx:alpine\nuntagged: nginx@sha256:abc\ndeleted: sha256:def\n\nTotal reclaimed space: 94.4MB\n"}}
	busy := false
	service := &Service{
		Docker: docker, PruneDocker: true,
		References: func() (map[string][]string, error) { return nil, nil },
		Exclusive: func(fn func()) bool {
			if busy {
				return false
			}
			fn()
			return true
		},
	}
	report := service.Cleanup(context.Background())
	if !reflect.DeepEqual(report.Pruned, []string{"nginx:alpine", "nginx@sha256:abc"}) || report.Reclaimed != "94.4MB" || report.Skipped != "" || len(report.Errors) != 0 {
		t.Fatalf("report = %+v", report)
	}
	if docker.commands[0] != "image prune --all --force" {
		t.Fatalf("prune command = %q", docker.commands)
	}
	busy = true
	if report := service.Cleanup(context.Background()); report.Skipped == "" || len(report.Pruned) != 0 {
		t.Fatalf("cleanup ran while an application was starting: %+v", report)
	}
	busy = false
	docker.fail = map[string]error{"image": errors.New("daemon unavailable")}
	if report := service.Cleanup(context.Background()); len(report.Errors) != 1 {
		t.Fatalf("prune failure not reported: %+v", report)
	}
	if _, err := service.List(context.Background()); err == nil {
		t.Error("List succeeded without a registry")
	}
	if service.Address() != "" {
		t.Error("Address without a registry")
	}
	service.Trigger()
	service.Trigger() // never blocks
}
