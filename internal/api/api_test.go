package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"site-deploy/internal/engine"
	"site-deploy/internal/images"
	"site-deploy/internal/model"
	"site-deploy/internal/orchestrator"
	"site-deploy/internal/version"
)

const testToken = "api-test-token-that-is-at-least-32-characters"

type fakeRuntime struct {
	mu                          sync.Mutex
	starts, stops, wakes, downs int
	tail                        int
	upError, logError           error
	upGate, downGate            <-chan struct{}
	upEntered, downEntered      chan struct{}
}

func (f *fakeRuntime) Up(ctx context.Context, _ model.App) (string, error) {
	f.mu.Lock()
	f.starts++
	gate, entered, err := f.upGate, f.upEntered, f.upError
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "127.0.0.1:12345", err
}

func (f *fakeRuntime) Suspend(context.Context, model.App) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

func (f *fakeRuntime) Resume(context.Context, model.App) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakes++
	return "127.0.0.1:12345", nil
}

func (f *fakeRuntime) Down(ctx context.Context, _ model.App) error {
	f.mu.Lock()
	f.downs++
	gate, entered := f.downGate, f.downEntered
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (*fakeRuntime) Inspect(context.Context, model.App) (engine.Status, error) {
	return engine.Status{Suspended: true}, nil
}

func (*fakeRuntime) ExistingVolumes(context.Context, model.App, []string) ([]string, error) {
	return nil, nil
}

func (*fakeRuntime) CopyVolumes(context.Context, model.App, model.App, []string) error {
	return nil
}

func (f *fakeRuntime) Logs(_ context.Context, _ model.App, tail int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tail = tail
	return "api-test log line\n", f.logError
}

type fixture struct {
	manager *orchestrator.Manager
	runtime *fakeRuntime
	tempDir string
	handler http.Handler
}

func newFixture(t *testing.T, onChange func(context.Context) error) fixture {
	t.Helper()
	runtime := &fakeRuntime{}
	manager, err := orchestrator.New(t.TempDir(), runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	return fixture{manager: manager, runtime: runtime, tempDir: dir, handler: New(manager, testToken, dir, onChange)}
}

func bundle(t *testing.T, domain string) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	compose := fmt.Sprintf("x-site-deploy:\n  domain: %s\n  service: web\n  port: 80\nservices:\n  web:\n    image: nginx:alpine\n    volumes:\n      - ./public:/usr/share/nginx/html:ro\n", domain)
	for path, content := range map[string]string{"compose.yaml": compose, "public/index.html": "api test fixture"} {
		entry, err := w.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func request(handler http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if method == http.MethodPost && !strings.HasSuffix(path, "/suspend") && !strings.HasSuffix(path, "/resume") {
		req.Header.Set("Content-Type", "application/zip")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func requireStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("HTTP status %d, want %d: %s", response.Code, want, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("non-JSON content type %q", response.Header().Get("Content-Type"))
	}
	if !json.Valid(response.Body.Bytes()) {
		t.Fatalf("invalid JSON: %s", response.Body.String())
	}
	if response.Code >= 400 {
		var body map[string]string
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["error"] == "" {
			t.Fatalf("missing JSON error: %s (%v)", response.Body.String(), err)
		}
	}
}

func decodeApp(t *testing.T, response *httptest.ResponseRecorder) model.App {
	t.Helper()
	var app model.App
	if err := json.Unmarshal(response.Body.Bytes(), &app); err != nil {
		t.Fatal(err)
	}
	return app
}

func deploy(t *testing.T, f fixture, name, domain string) model.App {
	t.Helper()
	response := request(f.handler, http.MethodPost, "/v1/apps/"+name, bytes.NewReader(bundle(t, domain)))
	requireStatus(t, response, http.StatusCreated)
	return decodeApp(t, response)
}

func assertEmptyDir(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("upload files were not removed: %v (%v)", entries, err)
	}
}

func TestAuthenticationAndPublicHealth(t *testing.T) {
	f := newFixture(t, nil)
	health := httptest.NewRecorder()
	f.handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	requireStatus(t, health, http.StatusOK)
	if strings.TrimSpace(health.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("health exposed extra details: %s", health.Body.String())
	}
	for _, authorization := range []string{"", "Basic " + testToken, "Bearer wrong-token", "Bearer " + testToken + " extra", "Bearer"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		requireStatus(t, response, http.StatusUnauthorized)
		if response.Header().Get("WWW-Authenticate") == "" || strings.Contains(response.Body.String(), testToken) {
			t.Fatalf("invalid authentication failure response: %+v", response)
		}
	}
	for _, path := range []string{"/v1/apps", "/v1/apps/missing", "/v1/caddy", "/v1/unknown"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		requireStatus(t, response, http.StatusUnauthorized)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
	req.Header.Add("Authorization", "Bearer "+testToken)
	req.Header.Add("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	requireStatus(t, response, http.StatusUnauthorized)
	emptyToken := New(f.manager, "", f.tempDir, nil)
	requireStatus(t, request(emptyToken, http.MethodGet, "/v1/apps", nil), http.StatusUnauthorized)
	response = request(f.handler, http.MethodGet, "/v1/apps", nil)
	requireStatus(t, response, http.StatusOK)
	if strings.TrimSpace(response.Body.String()) != "[]" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected empty list: %s", response.Body.String())
	}
}

func TestExactRoutesAndMethods(t *testing.T) {
	f := newFixture(t, nil)
	for _, test := range []struct {
		method, path, allow string
		status              int
	}{
		{http.MethodPost, "/healthz", "GET", http.StatusMethodNotAllowed},
		{http.MethodHead, "/healthz", "GET", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/apps", "GET", http.StatusMethodNotAllowed},
		{http.MethodPut, "/v1/apps/example", "GET, POST, DELETE", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/apps/example/suspend", "POST", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v1/apps/example/resume", "POST", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/apps/example/logs", "GET", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/caddy", "GET", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/apps/missing", "", http.StatusNotFound},
		{http.MethodGet, "/v1/apps/", "", http.StatusNotFound},
		{http.MethodGet, "/v1/apps/example/", "", http.StatusNotFound},
		{http.MethodPost, "/v1/apps/example/resume/extra", "", http.StatusNotFound},
		{http.MethodDelete, "/v1/apps/example/unknown", "", http.StatusNotFound},
		{http.MethodGet, "/v1/caddy/", "", http.StatusNotFound},
		{http.MethodGet, "/v1/apps/%65xample", "", http.StatusBadRequest},
		{http.MethodGet, "/v1/apps/example%2fresume", "", http.StatusBadRequest},
		{http.MethodGet, "/v1/apps/%2e%2e", "", http.StatusBadRequest},
		{http.MethodGet, "/v1/apps/BadName", "", http.StatusBadRequest},
		{http.MethodGet, "/v1/apps/-flag", "", http.StatusBadRequest},
		{http.MethodGet, "/v1/apps/" + strings.Repeat("x", 49), "", http.StatusBadRequest},
		{http.MethodGet, "/health%7a", "", http.StatusNotFound},
		{http.MethodGet, "/unknown", "", http.StatusNotFound},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			response := request(f.handler, test.method, test.path, nil)
			requireStatus(t, response, test.status)
			if got := response.Header().Get("Allow"); got != test.allow {
				t.Fatalf("Allow %q, want %q", got, test.allow)
			}
		})
	}
}

func TestDeploymentLifecycle(t *testing.T) {
	changes := 0
	f := newFixture(t, func(ctx context.Context) error { changes++; return ctx.Err() })
	app := deploy(t, f, "api-example", "api-example.localhost")
	if app.Name != "api-example" || app.State != model.StateRunning || app.Spec.SuspendMode != "stop" {
		t.Fatalf("bad deployment response: %+v", app)
	}
	data, err := os.ReadFile(filepath.Join(app.Directory, "public/index.html"))
	if err != nil || string(data) != "api test fixture" {
		t.Fatalf("deployment files missing: %q (%v)", data, err)
	}
	assertEmptyDir(t, f.tempDir)
	response := request(f.handler, http.MethodGet, "/v1/apps/api-example", nil)
	requireStatus(t, response, http.StatusOK)
	if decodeApp(t, response).Name != app.Name {
		t.Fatal("inspect returned a different deployment")
	}
	response = request(f.handler, http.MethodGet, "/v1/apps", nil)
	requireStatus(t, response, http.StatusOK)
	var apps []model.App
	if err := json.Unmarshal(response.Body.Bytes(), &apps); err != nil || len(apps) != 1 {
		t.Fatalf("unexpected app list: %s (%v)", response.Body.String(), err)
	}
	response = request(f.handler, http.MethodPost, "/v1/apps/api-example/suspend", nil)
	requireStatus(t, response, http.StatusOK)
	if decodeApp(t, response).State != model.StateSuspended {
		t.Fatal("app did not suspend")
	}
	response = request(f.handler, http.MethodPost, "/v1/apps/api-example/resume", nil)
	requireStatus(t, response, http.StatusOK)
	if decodeApp(t, response).State != model.StateRunning {
		t.Fatal("app did not resume")
	}
	response = request(f.handler, http.MethodDelete, "/v1/apps/api-example", nil)
	requireStatus(t, response, http.StatusOK)
	if strings.TrimSpace(response.Body.String()) != `{"deleted":"api-example"}` || changes != 2 {
		t.Fatalf("delete response %s, route change count %d", response.Body.String(), changes)
	}
	requireStatus(t, request(f.handler, http.MethodGet, "/v1/apps/api-example", nil), http.StatusNotFound)
	if _, err := os.Stat(app.Directory); !os.IsNotExist(err) {
		t.Fatalf("deleted app files remained: %v", err)
	}
	f.runtime.mu.Lock()
	defer f.runtime.mu.Unlock()
	if f.runtime.starts != 1 || f.runtime.stops != 1 || f.runtime.wakes != 1 || f.runtime.downs != 1 {
		t.Fatalf("unexpected runtime lifecycle: %+v", f.runtime)
	}
}

func TestConflictsBusyAndValidationErrors(t *testing.T) {
	f := newFixture(t, nil)
	deploy(t, f, "api-example", "api-example.localhost")
	for _, name := range []string{"api-example", "api-domain-conflict"} {
		response := request(f.handler, http.MethodPost, "/v1/apps/"+name, bytes.NewReader(bundle(t, "api-example.localhost")))
		requireStatus(t, response, http.StatusConflict)
	}
	lease, err := f.manager.Acquire(context.Background(), "api-example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	requireStatus(t, request(f.handler, http.MethodPost, "/v1/apps/api-example/suspend", nil), http.StatusConflict)
	// Deletion breaks open connections instead of refusing.
	go func() { <-lease.Context.Done(); lease.Release() }()
	requireStatus(t, request(f.handler, http.MethodDelete, "/v1/apps/api-example", nil), http.StatusOK)
	requireStatus(t, request(f.handler, http.MethodPost, "/v1/apps/api-broken", strings.NewReader("not a ZIP")), http.StatusBadRequest)
	response := request(f.handler, http.MethodPost, "/v1/apps/api-invalid-domain", bytes.NewReader(bundle(t, "https://invalid.localhost")))
	requireStatus(t, response, http.StatusBadRequest)
	for _, query := range []string{"unknown=1", "copy_volumes_from=Bad", "copy_volumes_from=a&copy_volumes_from=b", "copy_volumes_from=missing"} {
		response := request(f.handler, http.MethodPost, "/v1/apps/api-copy?"+query, bytes.NewReader(bundle(t, "api-copy.localhost")))
		requireStatus(t, response, http.StatusBadRequest)
	}
	requireStatus(t, request(f.handler, http.MethodGet, "/v1/apps/api-copy", nil), http.StatusNotFound)
	for _, endpoint := range []struct{ method, path string }{
		{http.MethodPost, "/v1/apps/missing/suspend"},
		{http.MethodPost, "/v1/apps/missing/resume"},
		{http.MethodDelete, "/v1/apps/missing"},
		{http.MethodGet, "/v1/apps/missing/logs"},
	} {
		requireStatus(t, request(f.handler, endpoint.method, endpoint.path, nil), http.StatusNotFound)
	}
	assertEmptyDir(t, f.tempDir)
}

func TestUploadLimitMediaTypeAndPrivateStreaming(t *testing.T) {
	f := newFixture(t, nil)
	for _, contentType := range []string{"", "text/plain", "multipart/form-data; boundary=x", "application/zip; malformed"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/apps/api-example", strings.NewReader("bad"))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", contentType)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		requireStatus(t, response, http.StatusUnsupportedMediaType)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/apps/api-example", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/zip")
	req.ContentLength = maxUploadBytes + 1
	req.Body = io.NopCloser(failOnRead{t: t})
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	requireStatus(t, response, http.StatusRequestEntityTooLarge)
	assertEmptyDir(t, f.tempDir)

	// Exercise the same streaming limit with an unknown-length body and a
	// small limit, without writing a 100 MiB fixture for every test run.
	req = httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(bytes.Repeat([]byte{'x'}, 1025)))
	req.ContentLength = -1
	path, status, err := receiveZIP(httptest.NewRecorder(), req, f.tempDir, 1024)
	if err == nil || status != http.StatusRequestEntityTooLarge || path != "" {
		t.Fatalf("unknown-length oversized upload accepted: %q %d %v", path, status, err)
	}
	assertEmptyDir(t, f.tempDir)
	checked := false
	body := &checkingReader{reader: strings.NewReader("private streaming bytes"), check: func() {
		entries, err := os.ReadDir(f.tempDir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("upload was buffered before creating private file: %v (%v)", entries, err)
		}
		info, err := entries[0].Info()
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("upload file was not 0600: %v (%v)", info, err)
		}
		checked = true
	}}
	req = httptest.NewRequest(http.MethodPost, "/upload", body)
	path, status, err = receiveZIP(httptest.NewRecorder(), req, f.tempDir, 1024)
	if err != nil || status != http.StatusOK || !checked {
		t.Fatalf("streaming upload failed: %q %d %v", path, status, err)
	}
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "private streaming bytes" {
		t.Fatalf("bad streamed body %q (%v)", data, err)
	}
}

type failOnRead struct{ t *testing.T }

func (r failOnRead) Read([]byte) (int, error) {
	r.t.Error("oversized declared body was read")
	return 0, errors.New("must not read")
}

type checkingReader struct {
	reader io.Reader
	check  func()
}

func (r *checkingReader) Read(buffer []byte) (int, error) {
	r.check()
	return r.reader.Read(buffer)
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("test upload interrupted") }

func TestFailedUploadCleansTemporaryFile(t *testing.T) {
	f := newFixture(t, nil)
	response := request(f.handler, http.MethodPost, "/v1/apps/api-broken", brokenReader{})
	requireStatus(t, response, http.StatusBadRequest)
	assertEmptyDir(t, f.tempDir)
	missingDir := filepath.Join(f.tempDir, "missing")
	handler := New(f.manager, testToken, missingDir, nil)
	response = request(handler, http.MethodPost, "/v1/apps/api-broken", strings.NewReader("payload"))
	requireStatus(t, response, http.StatusInternalServerError)
}

func TestLogsTailValidation(t *testing.T) {
	f := newFixture(t, nil)
	deploy(t, f, "api-example", "api-example.localhost")
	for _, tail := range []string{"0", "-1", "10001", "all", "", "10&tail=20", "10;bad", "999999999999999999999999"} {
		requireStatus(t, request(f.handler, http.MethodGet, "/v1/apps/api-example/logs?tail="+tail, nil), http.StatusBadRequest)
	}
	for _, test := range []struct {
		query string
		tail  int
	}{{"", 100}, {"?tail=1", 1}, {"?tail=10000", 10000}} {
		response := request(f.handler, http.MethodGet, "/v1/apps/api-example/logs"+test.query, nil)
		requireStatus(t, response, http.StatusOK)
		var body map[string]string
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["logs"] != "api-test log line\n" {
			t.Fatalf("unexpected logs: %s (%v)", response.Body.String(), err)
		}
		f.runtime.mu.Lock()
		got := f.runtime.tail
		f.runtime.mu.Unlock()
		if got != test.tail {
			t.Fatalf("tail=%d, want %d", got, test.tail)
		}
	}
	f.runtime.mu.Lock()
	f.runtime.logError = errors.New("fake runtime logs unavailable")
	f.runtime.mu.Unlock()
	requireStatus(t, request(f.handler, http.MethodGet, "/v1/apps/api-example/logs", nil), http.StatusInternalServerError)
}

func TestCaddyConfigurationAndValidation(t *testing.T) {
	f := newFixture(t, nil)
	deploy(t, f, "api-example", "api-example.localhost")
	response := request(f.handler, http.MethodGet, "/v1/caddy?http=true&gateway=127.0.0.1:19080", nil)
	requireStatus(t, response, http.StatusOK)
	for _, want := range []string{"api-example.localhost", "127.0.0.1:19080", ":80"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("Caddy configuration missing %q: %s", want, response.Body.String())
		}
	}
	for _, query := range []string{"http=unknown", "http=", "gateway=", "gateway=0.0.0.0:9080", "gateway=http://127.0.0.1:9080", "http=true&http=false", "unknown=1"} {
		requireStatus(t, request(f.handler, http.MethodGet, "/v1/caddy?"+query, nil), http.StatusBadRequest)
	}
}

func TestCaddyWarningPreservesSuccessfulOperation(t *testing.T) {
	f := newFixture(t, func(context.Context) error { return errors.New("admin unavailable\r\n" + testToken) })
	response := request(f.handler, http.MethodPost, "/v1/apps/api-example", bytes.NewReader(bundle(t, "api-example.localhost")))
	requireStatus(t, response, http.StatusCreated)
	if app := decodeApp(t, response); app.Name != "api-example" || app.State != model.StateRunning {
		t.Fatalf("sync failure misrepresented deployment: %+v", app)
	}
	if response.Header().Get("Location") != "/v1/apps/api-example" {
		t.Fatal("missing created resource location")
	}
	checkWarning := func(response *httptest.ResponseRecorder) {
		t.Helper()
		warning := response.Header().Get(warningHeader)
		if !strings.Contains(warning, "Caddy synchronization failed") || strings.ContainsAny(warning, "\r\n") || strings.Contains(warning, testToken) {
			t.Fatalf("invalid warning header: %q", warning)
		}
	}
	checkWarning(response)
	response = request(f.handler, http.MethodDelete, "/v1/apps/api-example", nil)
	requireStatus(t, response, http.StatusOK)
	checkWarning(response)
	if _, err := f.manager.Get("api-example"); !errors.Is(err, orchestrator.ErrNotFound) {
		t.Fatalf("sync failure prevented deletion: %v", err)
	}
}

func TestFailedStartupStillSyncsAndCanBeManaged(t *testing.T) {
	changes := 0
	f := newFixture(t, func(context.Context) error { changes++; return nil })
	f.runtime.upError = errors.New("fake image pull failure " + testToken)
	response := request(f.handler, http.MethodPost, "/v1/apps/api-failed", bytes.NewReader(bundle(t, "api-failed.localhost")))
	requireStatus(t, response, http.StatusServiceUnavailable)
	if changes != 1 || !strings.Contains(response.Body.String(), "api-failed") || !strings.Contains(response.Body.String(), "resume or delete") || strings.Contains(response.Body.String(), testToken) {
		t.Fatalf("failed deployment was not made actionable: changes=%d body=%s", changes, response.Body.String())
	}
	for _, path := range []string{"/v1/apps/api-failed", "/v1/apps"} {
		response = request(f.handler, http.MethodGet, path, nil)
		requireStatus(t, response, http.StatusOK)
		if strings.Contains(response.Body.String(), testToken) {
			t.Fatal("runtime error exposed API token")
		}
	}
	response = request(f.handler, http.MethodPost, "/v1/apps/api-failed/resume", nil)
	requireStatus(t, response, http.StatusOK)
	if decodeApp(t, response).State != model.StateRunning {
		t.Fatal("failed deployment could not be resumed")
	}
	assertEmptyDir(t, f.tempDir)
}

func TestCanceledDeployStillSyncsWithIndependentBoundedContext(t *testing.T) {
	type result struct {
		err      error
		deadline time.Time
		hasLimit bool
	}
	synced := make(chan result, 1)
	f := newFixture(t, func(ctx context.Context) error {
		deadline, hasLimit := ctx.Deadline()
		synced <- result{ctx.Err(), deadline, hasLimit}
		return nil
	})
	gate := make(chan struct{})
	f.runtime.upGate = gate
	f.runtime.upEntered = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/apps/api-cancel", bytes.NewReader(bundle(t, "api-cancel.localhost"))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/zip")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.handler.ServeHTTP(response, req) }()
	wait(t, f.runtime.upEntered, "startup")
	cancel()
	wait(t, done, "canceled deployment response")
	close(gate)
	requireStatus(t, response, http.StatusRequestTimeout)
	select {
	case got := <-synced:
		if got.err != nil || !got.hasLimit || time.Until(got.deadline) <= 0 || time.Until(got.deadline) > syncTimeout {
			t.Fatalf("sync inherited cancellation or lacked deadline: %+v", got)
		}
	default:
		t.Fatal("saved canceled deployment was not synchronized")
	}
	if _, err := f.manager.Get("api-cancel"); err != nil {
		t.Fatalf("canceled deployment was not retained: %v", err)
	}
}

func TestCanceledDeleteSyncsAfterRemoval(t *testing.T) {
	f := newFixture(t, nil)
	deploy(t, f, "api-cancel", "api-cancel.localhost")
	gate := make(chan struct{})
	f.runtime.downGate = gate
	f.runtime.downEntered = make(chan struct{}, 1)
	synced := make(chan int, 1)
	f.handler = New(f.manager, testToken, f.tempDir, func(ctx context.Context) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		synced <- len(f.manager.Domains())
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodDelete, "/v1/apps/api-cancel", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.handler.ServeHTTP(response, req) }()
	wait(t, f.runtime.downEntered, "deletion")
	cancel()
	select {
	case <-synced:
		t.Fatal("route sync ran before the deletion completed")
	default:
	}
	close(gate)
	wait(t, done, "delete response")
	requireStatus(t, response, http.StatusOK)
	select {
	case count := <-synced:
		if count != 0 {
			t.Fatal("deleted domain remained during sync")
		}
	default:
		t.Fatal("client cancellation skipped route synchronization")
	}
}

func wait(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func TestReleaseDaemonRejectsOlderClientsBeforeActing(t *testing.T) {
	previous := version.Version
	version.Version = "0.2.0+daemon"
	t.Cleanup(func() { version.Version = previous })
	f := newFixture(t, nil)
	withClient := func(method, path, client string, body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Authorization", "Bearer "+testToken)
		if body != nil {
			req.Header.Set("Content-Type", "application/zip")
		}
		if client != "" {
			req.Header.Set(version.ClientHeader, client)
		}
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, req)
		return response
	}
	for _, client := range []string{"", "dev", "0.1.9", "0.2.0-rc1"} {
		response := withClient(http.MethodPost, "/v1/apps/old-client", client, bytes.NewReader(bundle(t, "old.example")))
		requireStatus(t, response, http.StatusUpgradeRequired)
		if response.Header().Get(version.Header) != "0.2.0+daemon" {
			t.Errorf("client %q: missing daemon version header", client)
		}
	}
	if apps := f.manager.List(); len(apps) != 0 || f.runtime.starts != 0 {
		t.Fatalf("outdated client changed state: apps=%v starts=%d", apps, f.runtime.starts)
	}
	// Any authenticated client can learn the daemon version to explain a mismatch.
	response := withClient(http.MethodGet, "/v1/version", "", nil)
	requireStatus(t, response, http.StatusOK)
	if strings.TrimSpace(response.Body.String()) != `{"version":"0.2.0+daemon"}` {
		t.Fatalf("version body: %s", response.Body.String())
	}
	requireStatus(t, withClient(http.MethodPost, "/v1/version", "", nil), http.StatusMethodNotAllowed)
	unauthenticated := httptest.NewRecorder()
	f.handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/v1/version", nil))
	requireStatus(t, unauthenticated, http.StatusUnauthorized)
	if unauthenticated.Header().Get(version.Header) != "" {
		t.Error("daemon version exposed without authentication")
	}
	for _, client := range []string{"0.2.0", "0.2.0+other", "0.3.0"} {
		requireStatus(t, withClient(http.MethodGet, "/v1/apps", client, nil), http.StatusOK)
	}
	response = withClient(http.MethodPost, "/v1/apps/new-client", "0.2.0", bytes.NewReader(bundle(t, "new.example")))
	requireStatus(t, response, http.StatusCreated)
}

func TestBasicAuthHashesNeverLeaveTheDaemon(t *testing.T) {
	const hash = "$2a$14$O4UeNfvX7xUhP9mj6tjWIOkvvB/sTqKipfaXtgWqmJ7MZjMg/l9me"
	f := newFixture(t, nil)
	var output bytes.Buffer
	w := zip.NewWriter(&output)
	entry, err := w.Create("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(entry, "x-site-deploy:\n  domain: private.example\n  service: web\n  port: 80\n  basic_auth:\n    - user: preview\n      hash: "+hash+"\nservices:\n  web:\n    image: nginx:alpine\n")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	responses := []*httptest.ResponseRecorder{request(f.handler, http.MethodPost, "/v1/apps/private", &output)}
	requireStatus(t, responses[0], http.StatusCreated)
	responses = append(responses, request(f.handler, http.MethodGet, "/v1/apps/private", nil), request(f.handler, http.MethodGet, "/v1/apps", nil))
	for _, response := range responses {
		body := response.Body.String()
		if strings.Contains(body, "$2a$") || !strings.Contains(body, `"user":"preview"`) {
			t.Fatalf("API response must show users but never hashes: %s", body)
		}
	}
	// The manager keeps the hash for Caddy, and redaction must not alter it.
	if app, err := f.manager.Get("private"); err != nil || len(app.Spec.BasicAuth) != 1 || app.Spec.BasicAuth[0].Hash != hash {
		t.Fatalf("stored account changed: %+v %v", app.Spec.BasicAuth, err)
	}
	config := request(f.handler, http.MethodGet, "/v1/caddy?http=true", nil)
	if config.Code != http.StatusOK || !strings.Contains(config.Body.String(), `"handler": "authentication"`) || !strings.Contains(config.Body.String(), hash) {
		t.Fatalf("Caddy configuration lacks the account: %s", config.Body.String())
	}
}

type fakeImages struct {
	address  string
	listErr  error
	triggers int
	cleanups int
}

func (f *fakeImages) Address() string { return f.address }
func (f *fakeImages) List(context.Context) ([]images.Image, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []images.Image{{Reference: f.address + "/base:1", Repository: "base", Tag: "1", UsedBy: []string{"web"}}}, nil
}
func (f *fakeImages) Cleanup(context.Context) images.Report {
	f.cleanups++
	return images.Report{Deleted: []string{"base:0"}, GarbageCollected: true}
}
func (f *fakeImages) Trigger() { f.triggers++ }

func TestRegistryAndCleanupEndpoints(t *testing.T) {
	f := newFixture(t, nil)
	requireStatus(t, request(f.handler, http.MethodGet, "/v1/registry", nil), http.StatusNotFound)
	requireStatus(t, request(f.handler, http.MethodPost, "/v1/cleanup", nil), http.StatusNotFound)

	service := &fakeImages{address: "127.0.0.1:9082"}
	handler := New(f.manager, testToken, f.tempDir, nil, WithImages(service))
	response := request(handler, http.MethodGet, "/v1/registry", nil)
	requireStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), `"address":"127.0.0.1:9082"`) || !strings.Contains(response.Body.String(), `"used_by":["web"]`) {
		t.Fatalf("registry body: %s", response.Body.String())
	}
	requireStatus(t, request(handler, http.MethodPost, "/v1/registry", nil), http.StatusMethodNotAllowed)
	response = request(handler, http.MethodPost, "/v1/cleanup", nil)
	requireStatus(t, response, http.StatusOK)
	if service.cleanups != 1 || !strings.Contains(response.Body.String(), `"deleted":["base:0"]`) {
		t.Fatalf("cleanup body: %s", response.Body.String())
	}
	requireStatus(t, request(handler, http.MethodGet, "/v1/cleanup", nil), http.StatusMethodNotAllowed)
	service.listErr = errors.New("registry unreachable")
	requireStatus(t, request(handler, http.MethodGet, "/v1/registry", nil), http.StatusBadGateway)

	// Deleting an application frees its images, so it schedules a cleanup.
	deploy(t, fixture{manager: f.manager, runtime: f.runtime, tempDir: f.tempDir, handler: handler}, "web", "web.example")
	requireStatus(t, request(handler, http.MethodDelete, "/v1/apps/web", nil), http.StatusOK)
	if service.triggers != 1 {
		t.Fatalf("delete triggered %d cleanups", service.triggers)
	}
	// A prune-only daemon has cleanup but no registry.
	pruneOnly := New(f.manager, testToken, f.tempDir, nil, WithImages(&fakeImages{}))
	requireStatus(t, request(pruneOnly, http.MethodGet, "/v1/registry", nil), http.StatusNotFound)
	requireStatus(t, request(pruneOnly, http.MethodPost, "/v1/cleanup", nil), http.StatusOK)
}
