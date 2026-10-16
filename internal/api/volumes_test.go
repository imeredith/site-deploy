package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"site-deploy/internal/orchestrator"
	"site-deploy/internal/volumes"
	"site-deploy/internal/volumes/volumestest"
)

func newVolumesFixture(t *testing.T) fixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := volumes.Open(dir, &volumestest.Subvolumes{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	runtime := &fakeRuntime{}
	manager, err := orchestrator.New(t.TempDir(), runtime, nil, orchestrator.WithVolumes(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Close() })
	temp := t.TempDir()
	return fixture{manager: manager, runtime: runtime, tempDir: temp, handler: New(manager, testToken, temp, nil)}
}

func snapshotBundle(t *testing.T, domain string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	entry, err := w.Create("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	compose := "x-site-deploy:\n  domain: " + domain + "\n  service: web\n  port: 80\n  volumes:\n    corpus: {target: /corpus, snapshot: true}\nservices:\n  web:\n    image: nginx:alpine\n"
	if _, err := io.WriteString(entry, compose); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestVolumeEndpoints(t *testing.T) {
	f := newVolumesFixture(t)
	response := request(f.handler, http.MethodPost, "/v1/volumes/corpus", nil)
	requireStatus(t, response, http.StatusCreated)
	var created volumes.Volume
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.Name != "corpus" || !strings.HasSuffix(created.Path, "/corpus/data") {
		t.Fatalf("created %s %v", response.Body, err)
	}
	requireStatus(t, request(f.handler, http.MethodPost, "/v1/volumes/corpus", nil), http.StatusConflict)

	response = request(f.handler, http.MethodPost, "/v1/apps/pr1", bytes.NewReader(snapshotBundle(t, "pr1.localhost")))
	requireStatus(t, response, http.StatusCreated)
	response = request(f.handler, http.MethodGet, "/v1/volumes", nil)
	requireStatus(t, response, http.StatusOK)
	var list []volumes.Volume
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || len(list) != 1 || len(list[0].Snapshots) != 1 || len(list[0].UsedBy) != 1 || list[0].UsedBy[0] != (volumes.Use{App: "pr1", Snapshot: true}) {
		t.Fatalf("list %s %v", response.Body, err)
	}
	requireStatus(t, request(f.handler, http.MethodDelete, "/v1/volumes/corpus", nil), http.StatusConflict)
	requireStatus(t, request(f.handler, http.MethodDelete, "/v1/volumes/corpus/snapshots/pr1", nil), http.StatusConflict)

	requireStatus(t, request(f.handler, http.MethodDelete, "/v1/apps/pr1", nil), http.StatusOK)
	requireStatus(t, request(f.handler, http.MethodDelete, "/v1/volumes/corpus/snapshots/pr1", nil), http.StatusOK)
	requireStatus(t, request(f.handler, http.MethodDelete, "/v1/volumes/corpus/snapshots/pr1", nil), http.StatusNotFound)
	requireStatus(t, request(f.handler, http.MethodDelete, "/v1/volumes/corpus", nil), http.StatusOK)
	requireStatus(t, request(f.handler, http.MethodDelete, "/v1/volumes/corpus", nil), http.StatusNotFound)

	for _, test := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/v1/volumes/Corpus", http.StatusBadRequest},
		{http.MethodPost, "/v1/volumes/corpus?force=1", http.StatusBadRequest},
		{http.MethodDelete, "/v1/volumes/corpus/snapshots/Pr1", http.StatusBadRequest},
		{http.MethodGet, "/v1/volumes/corpus", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/volumes/corpus/rekey", http.StatusNotFound},
		{http.MethodPost, "/v1/volumes/corpus/snapshots", http.StatusNotFound},
		{http.MethodPost, "/v1/volumes/corpus/other", http.StatusNotFound},
		{http.MethodPost, "/v1/volumes", http.StatusMethodNotAllowed},
	} {
		requireStatus(t, request(f.handler, test.method, test.path, nil), test.want)
	}
}

func TestVolumeEndpointsWithoutVolumes(t *testing.T) {
	f := newFixture(t, nil)
	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/v1/volumes"},
		{http.MethodPost, "/v1/volumes/corpus"},
		{http.MethodDelete, "/v1/volumes/corpus/snapshots/pr1"},
	} {
		response := request(f.handler, test.method, test.path, nil)
		requireStatus(t, response, http.StatusNotFound)
		if !strings.Contains(response.Body.String(), "--volumes-dir") {
			t.Errorf("%s %s: %s", test.method, test.path, response.Body)
		}
	}
}
