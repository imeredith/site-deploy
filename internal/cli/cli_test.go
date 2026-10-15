package cli

import (
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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"site-deploy/internal/version"
)

const cliTestToken = "cli-tests-0123456789abcdef0123456789abcdef"

func cliTestEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"SITE_DEPLOY_SERVER", "SITE_DEPLOY_TOKEN", "SITE_DEPLOY_TOKEN_FILE", "SITE_DEPLOY_REGISTRY_VIA",
		"SITE_DEPLOY_HOST", "SITE_DEPLOY_SSH", "SITE_DEPLOY_RSH", "SITE_DEPLOY_REGISTRY_PORT"} {
		t.Setenv(name, "")
	}
	// Never read the developer's own hosts.
	t.Setenv("SITE_DEPLOY_CONFIG", filepath.Join(t.TempDir(), "hosts.json"))
}

func cliTestRun(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLICommandRequests(t *testing.T) {
	cliTestEnvironment(t)
	zipPath := filepath.Join(t.TempDir(), "cli-command-upload.zip")
	zipData := []byte("PK\x03\x04raw-streamed-zip-payload")
	if err := os.WriteFile(zipPath, zipData, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, method, path, query, response, output string
		args                                        []string
	}{
		{"deploy", "POST", "/v1/apps/cli-example", "", `{"name":"cli-example","state":"running","spec":{"domain":"cli.example"}}`, "cli-example: running (cli.example)", []string{"deploy", "cli-example", zipPath}},
		{"inspect", "GET", "/v1/apps/cli-example", "", `{"name":"cli-example","state":"running"}`, `"name": "cli-example"`, []string{"inspect", "cli-example"}},
		{"suspend", "POST", "/v1/apps/cli-example/suspend", "", `{"name":"cli-example","state":"suspended","spec":{"domain":"cli.example"}}`, "cli-example: suspended (cli.example)", []string{"suspend", "cli-example"}},
		{"resume", "POST", "/v1/apps/cli-example/resume", "", `{"name":"cli-example","state":"running","spec":{"domain":"cli.example"}}`, "cli-example: running (cli.example)", []string{"resume", "cli-example"}},
		{"delete", "DELETE", "/v1/apps/cli-example", "", `{"deleted":"cli-example"}`, "Deleted cli-example", []string{"delete", "cli-example"}},
		{"logs-default", "GET", "/v1/apps/cli-example/logs", "tail=100", `{"logs":"one\ntwo\n"}`, "one\ntwo\n", []string{"logs", "cli-example"}},
		{"logs-tail", "GET", "/v1/apps/cli-example/logs", "tail=25", `{"logs":"latest\n"}`, "latest\n", []string{"logs", "--tail=25", "cli-example"}},
		{"caddy", "GET", "/v1/caddy", "gateway=127.0.0.1%3A9099&http=true", `{"apps":{"http":{}}}`, `"apps":`, []string{"caddy-config", "--http", "--gateway", "127.0.0.1:9099"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != test.method || r.URL.Path != test.path || r.URL.RawQuery != test.query {
					t.Errorf("request = %s %s, want %s %s?%s", r.Method, r.URL, test.method, test.path, test.query)
				}
				if r.Header.Get("Authorization") != "Bearer "+cliTestToken {
					t.Error("missing bearer token")
				}
				if test.name == "deploy" {
					body, err := io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(body, zipData) || r.ContentLength != int64(len(zipData)) || r.Header.Get("Content-Type") != "application/zip" {
						t.Errorf("ZIP upload not preserved: length=%d body=%q error=%v", r.ContentLength, body, err)
					}
				}
				io.WriteString(w, test.response)
			}))
			defer server.Close()
			args := append([]string{"--token", cliTestToken}, test.args...)
			args = append(args, "--server", server.URL)
			code, stdout, stderr := cliTestRun(args...)
			if code != 0 || stderr != "" || !strings.Contains(stdout, test.output) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestCLIListFormatsAndWarnings(t *testing.T) {
	cliTestEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Site-Deploy-Warning", "Caddy unavailable: "+cliTestToken)
		fmt.Fprintf(w, `[{"name":"zebra","state":"suspended","spec":{"domain":"zebra.localhost","idle_timeout":"5m"}},{"name":"apple","state":"error","spec":{"domain":"apple.localhost"},"error":%q}]`, cliTestToken)
	}))
	defer server.Close()
	t.Setenv("SITE_DEPLOY_SERVER", server.URL)
	t.Setenv("SITE_DEPLOY_TOKEN", cliTestToken)
	code, stdout, stderr := cliTestRun("list")
	if code != 0 || !strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "zebra.localhost") || strings.Index(stdout, "apple") > strings.Index(stdout, "zebra") {
		t.Fatalf("human list: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stderr, cliTestToken) || !strings.Contains(stderr, "Warning: Caddy unavailable: [redacted]") {
		t.Fatalf("warning not safely surfaced: %q", stderr)
	}
	code, stdout, stderr = cliTestRun("list", "--json")
	var apps []map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &apps) != nil || len(apps) != 2 || strings.Contains(stdout, cliTestToken) {
		t.Fatalf("JSON list: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestCLIRejectsRedirectsWithoutForwardingCredentials(t *testing.T) {
	cliTestEnvironment(t)
	var followed atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		io.WriteString(w, "[]")
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	code, stdout, stderr := cliTestRun("list", "--server", source.URL, "--token", cliTestToken)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "HTTP 307") || followed.Load() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q followed=%d", code, stdout, stderr, followed.Load())
	}
}

func TestCLIResponseFailures(t *testing.T) {
	cliTestEnvironment(t)
	for _, test := range []struct {
		name, want string
		handler    http.HandlerFunc
	}{
		{"api-error", "HTTP 409 (Conflict): duplicate [redacted] request", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "duplicate " + cliTestToken + "\nrequest"})
		}},
		{"invalid-json", "invalid JSON", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>not an API</html>") }},
		{"oversize", "16 MiB limit", func(w http.ResponseWriter, r *http.Request) {
			io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("x", maxResponseBytes+1)), maxResponseBytes+1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			code, stdout, stderr := cliTestRun("--server", server.URL, "list", "--token", cliTestToken)
			if code != 1 || stdout != "" || !strings.Contains(stderr, test.want) || strings.Contains(stderr, cliTestToken) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestCLIClientDeadline(t *testing.T) {
	cliTestEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	code, stdout, stderr := cliTestRun("list", "--server", server.URL, "--token", cliTestToken, "--timeout", "20ms")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "context deadline exceeded") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestCLIServerAddressValidation(t *testing.T) {
	for _, value := range []string{
		"http://127.0.0.1:9081", "http://127.0.0.2/", "http://[::1]:9081", "https://example.org", "https://example.org:443/",
	} {
		if _, err := serverURL(value); err != nil {
			t.Errorf("valid server %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{
		"http://example.org", "http://192.0.2.1:9081", "http://localhost:9081", "file:///tmp/socket", "http://user:secret@127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1/api", "http://127.0.0.1/%2f", "http://127.0.0.1?token=secret", "http://127.0.0.1?", "http://127.0.0.1#secret", "https://", "not a URL",
	} {
		if _, err := serverURL(value); err == nil {
			t.Errorf("unsafe server %q accepted", value)
		}
	}
}

func TestCLITokenSourcesAndValidation(t *testing.T) {
	cliTestEnvironment(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "cli-token")
	fileToken := "cli-file-token-0123456789abcdef0123456789"
	if err := os.WriteFile(path, []byte(fileToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, want string
		opts       options
	}{
		{"explicit-token", cliTestToken, options{token: cliTestToken, tokenSet: true, tokenFile: path, tokenFileSet: true}},
		{"explicit-file", fileToken, options{token: cliTestToken, tokenFile: path, tokenFileSet: true}},
		{"environment-token", cliTestToken, options{token: cliTestToken, tokenFile: path}},
		{"environment-file", fileToken, options{tokenFile: path}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := loadToken(test.opts)
			if err != nil || got != test.want {
				t.Errorf("token source failed: got correct token=%t error=%v", got == test.want, err)
			}
		})
	}
	for _, value := range []string{"", "short", cliTestToken + "\n", cliTestToken + " injected", strings.Repeat("x", maxTokenBytes+1), cliTestToken + "=x"} {
		if _, err := loadToken(options{token: value, tokenSet: true}); err == nil {
			t.Error("invalid token accepted")
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(options{tokenFile: path}); err == nil {
		t.Error("world-readable token accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "cli-token-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(options{tokenFile: link}); err == nil {
		t.Error("symlink token accepted")
	}
	if _, err := loadToken(options{tokenFile: filepath.Join(directory, "cli-missing")}); err == nil {
		t.Error("missing token accepted")
	}
}

func TestCLIParsingAndServePassthrough(t *testing.T) {
	cliTestEnvironment(t)
	opts, err := parse([]string{"--json", "deploy", "cli-app", "--server=http://127.0.0.2:9123", "--token", cliTestToken, "bundle.zip", "--timeout=11m"})
	if err != nil || opts.command != "deploy" || !opts.json || opts.server != "http://127.0.0.2:9123" || opts.timeout != 11*time.Minute || !reflect.DeepEqual(opts.positionals, []string{"cli-app", "bundle.zip"}) {
		t.Fatalf("mixed flag positions failed: options=%#v error=%v", opts, err)
	}
	opts, err = parse([]string{"deploy", "cli-app", "--", "--bundle.zip"})
	if err != nil || opts.positionals[1] != "--bundle.zip" {
		t.Fatalf("literal positional argument failed: %v", err)
	}
	for _, args := range [][]string{
		{"deploy", "cli-app"}, {"list", "extra"}, {"logs", "cli-app", "--tail", "0"}, {"logs", "cli-app", "--tail=10001"}, {"logs", "cli-app", "--timeout=0"}, {"list", "--tail=1"}, {"list", "--http"}, {"unknown"}, {"list", "--unknown"}, {"list", "--server"}, {"inspect", "../escape"}, {"inspect", "cli-app/logs"}, {"inspect", "UpperCase"},
	} {
		code, stdout, _ := cliTestRun(args...)
		if code != 2 || stdout != "" {
			t.Errorf("invalid command %q produced code=%d stdout=%q", args, code, stdout)
		}
	}
	oldServe := Serve
	t.Cleanup(func() { Serve = oldServe })
	var got []string
	Serve = func(args []string, stdout, stderr io.Writer) error { got = args; return nil }
	daemonData := filepath.Join(t.TempDir(), "cli-daemon-data")
	want := []string{"--token-file", "cli-daemon-token", "--gateway", "127.0.0.1:0", "--data-dir", daemonData, "--http"}
	code, _, stderr := cliTestRun(append([]string{"serve"}, want...)...)
	if code != 0 || stderr != "" || !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon args changed: code=%d got=%q stderr=%q", code, got, stderr)
	}
	code, _, _ = cliTestRun("--token-file", "cli-daemon-token", "serve", "--data-dir", daemonData)
	if code != 0 || !reflect.DeepEqual(got, []string{"--token-file", "cli-daemon-token", "--data-dir", daemonData}) {
		t.Fatalf("daemon prefix args changed: code=%d got=%q", code, got)
	}
	Serve = func([]string, io.Writer, io.Writer) error { return errors.New("daemon failed") }
	if code, _, stderr := cliTestRun("serve"); code != 1 || !strings.Contains(stderr, "daemon failed") {
		t.Errorf("daemon error not surfaced: code=%d stderr=%q", code, stderr)
	}
}

func TestCLIHelpAndVersionNeedNoCredentials(t *testing.T) {
	cliTestEnvironment(t)
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"version"}, {"--version"}, {"version", "--json"}} {
		code, stdout, stderr := cliTestRun(args...)
		if code != 0 || stdout == "" || stderr != "" {
			t.Errorf("args=%q code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
		if len(args) > 1 && !json.Valid([]byte(stdout)) {
			t.Error("version --json is not JSON")
		}
	}
}

func TestCLISendsVersionAndChecksDaemonVersion(t *testing.T) {
	cliTestEnvironment(t)
	previous := version.Version
	t.Cleanup(func() { version.Version = previous })
	daemon := "0.2.0+daemon"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/version" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s", r.Method, r.URL)
		}
		if r.Header.Get(version.ClientHeader) != version.Version {
			t.Errorf("client version header = %q, want %q", r.Header.Get(version.ClientHeader), version.Version)
		}
		fmt.Fprintf(w, `{"version":%q}`, daemon)
	}))
	defer server.Close()
	for _, test := range []struct {
		client string
		code   int
		output string
	}{
		{"0.2.0", 0, "CLI     0.2.0\nDaemon  0.2.0+daemon\n"},
		{"0.3.0", 0, "Daemon  0.2.0+daemon"},
		{"0.1.0", 1, "Daemon  0.2.0+daemon"},
		{"dev", 1, "CLI     dev"},
	} {
		version.Version = test.client
		code, stdout, stderr := cliTestRun("daemon-version", "--token", cliTestToken, "--server", server.URL)
		if code != test.code || !strings.Contains(stdout, test.output) {
			t.Errorf("client %s: code=%d stdout=%q stderr=%q", test.client, code, stdout, stderr)
		}
		if test.code != 0 && !strings.Contains(stderr, "daemon 0.2.0+daemon") {
			t.Errorf("client %s: mismatch not explained: %q", test.client, stderr)
		}
	}
	version.Version = "0.1.0"
	code, stdout, _ := cliTestRun("daemon-version", "--json", "--token", cliTestToken, "--server", server.URL)
	var result struct {
		CLI, Daemon string
		Compatible  bool
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || code != 1 || result.CLI != "0.1.0" || result.Daemon != daemon || result.Compatible {
		t.Fatalf("json: code=%d stdout=%q err=%v", code, stdout, err)
	}
	if code, _, _ := cliTestRun("daemon-version", "extra"); code != 2 {
		t.Errorf("daemon-version accepted a positional argument: code=%d", code)
	}
}

func TestCLICopyVolumesNeedsADaemonThatSupportsIt(t *testing.T) {
	cliTestEnvironment(t)
	zipPath := filepath.Join(t.TempDir(), "bundle.zip")
	if err := os.WriteFile(zipPath, []byte("PK\x03\x04zip"), 0600); err != nil {
		t.Fatal(err)
	}
	daemon, deployed := "", ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/version":
			fmt.Fprintf(w, `{"version":%q}`, daemon)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/apps/preview":
			deployed = r.URL.RawQuery
			io.WriteString(w, `{"name":"preview","state":"running","spec":{"domain":"preview.example"}}`)
		default:
			t.Errorf("request = %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()
	deploy := func() (int, string, string) {
		return cliTestRun("deploy", "preview", zipPath, "--copy-volumes-from", "main", "--token", cliTestToken, "--server", server.URL)
	}
	daemon = "0.6.1"
	if code, _, stderr := deploy(); code != 1 || !strings.Contains(stderr, "needs daemon 0.7.0 or newer, and this one is 0.6.1") || deployed != "" {
		t.Fatalf("old daemon: code=%d stderr=%q deployed=%q", code, stderr, deployed)
	}
	for _, daemon = range []string{"0.7.0", "dev"} {
		deployed = ""
		code, stdout, stderr := deploy()
		if code != 0 || deployed != "copy_volumes_from=main" || !strings.Contains(stdout, "preview: running") || !strings.Contains(stderr, "Copying named volumes from main") {
			t.Fatalf("daemon %s: code=%d stdout=%q stderr=%q deployed=%q", daemon, code, stdout, stderr, deployed)
		}
	}
	for _, args := range [][]string{
		{"list", "--copy-volumes-from", "main"},
		{"deploy", "main", zipPath, "--copy-volumes-from", "main"},
		{"deploy", "preview", zipPath, "--copy-volumes-from", "Main"},
		{"deploy", "preview", zipPath, "--copy-volumes-from"},
	} {
		if code, _, _ := cliTestRun(args...); code != 2 {
			t.Errorf("%q: code=%d, want 2", args, code)
		}
	}
}

func TestCLIListShowsAccess(t *testing.T) {
	cliTestEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"name":"open","state":"running","spec":{"domain":"open.example","idle_timeout":"5m"}},`+
			`{"name":"closed","state":"suspended","spec":{"domain":"closed.example","idle_timeout":"0","basic_auth":[{"user":"a"},{"user":"b"}]}}]`)
	}))
	defer server.Close()
	code, stdout, stderr := cliTestRun("list", "--token", cliTestToken, "--server", server.URL)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if code != 0 || len(lines) != 3 || !strings.HasSuffix(lines[0], "ACCESS") ||
		!strings.HasPrefix(lines[1], "closed") || !strings.HasSuffix(lines[1], "basic auth (2)") ||
		!strings.HasPrefix(lines[2], "open") || !strings.HasSuffix(lines[2], "public") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestCLIPushTagsPushesAndRemovesAlias(t *testing.T) {
	cliTestEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/registry" {
			t.Errorf("request = %s %s", r.Method, r.URL)
		}
		io.WriteString(w, `{"address":"127.0.0.1:9082","images":[]}`)
	}))
	defer server.Close()
	var calls []string
	pushError := error(nil)
	previous := dockerCommand
	dockerCommand = func(_ context.Context, _, _ io.Writer, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if args[1] == "push" {
			return pushError
		}
		return nil
	}
	t.Cleanup(func() { dockerCommand = previous })
	code, stdout, stderr := cliTestRun("push", "app-deps:local", "app-base:2026-10-13", "--token", cliTestToken, "--server", server.URL)
	want := []string{
		"image tag app-deps:local 127.0.0.1:9082/app-base:2026-10-13",
		"image push 127.0.0.1:9082/app-base:2026-10-13",
		"image rm 127.0.0.1:9082/app-base:2026-10-13",
	}
	if code != 0 || !reflect.DeepEqual(calls, want) || !strings.Contains(stdout, "image: 127.0.0.1:9082/app-base:2026-10-13") {
		t.Fatalf("code=%d calls=%q stdout=%q stderr=%q", code, calls, stdout, stderr)
	}
	// Pushing through a forward on another port still prints the daemon's
	// address, which is what the server pulls.
	calls = nil
	code, stdout, stderr = cliTestRun("push", "app-deps:local", "app-base:2026-10-13", "--registry-via", "127.0.0.1:41234", "--json", "--token", cliTestToken, "--server", server.URL)
	want = []string{
		"image tag app-deps:local 127.0.0.1:41234/app-base:2026-10-13",
		"image push 127.0.0.1:41234/app-base:2026-10-13",
		"image rm 127.0.0.1:41234/app-base:2026-10-13",
	}
	if code != 0 || !reflect.DeepEqual(calls, want) || strings.TrimSpace(stdout) != `{"reference":"127.0.0.1:9082/app-base:2026-10-13"}` {
		t.Fatalf("via: code=%d calls=%q stdout=%q stderr=%q", code, calls, stdout, stderr)
	}
	// A failed push still removes the temporary alias and explains the tunnel.
	calls, pushError = nil, errors.New("connection refused")
	code, _, stderr = cliTestRun("push", "app-deps:local", "app-base:2026-10-13", "--token", cliTestToken, "--server", server.URL)
	if code != 1 || len(calls) != 3 || !strings.Contains(stderr, "site-deploy-remote push") {
		t.Fatalf("failed push: code=%d calls=%q stderr=%q", code, calls, stderr)
	}
	for _, args := range [][]string{
		{"push", "img", "App-Base:1"},
		{"push", "img", "app-base"},
		{"push", "img", "app-base:"},
		{"push", "", "app-base:1"},
		{"push", "img"},
		{"push", "img", "app-base:1", "--registry-via", "192.0.2.2:9082"},
		{"push", "img", "app-base:1", "--registry-via", "localhost:9082"},
		{"push", "img", "app-base:1", "--registry-via", "127.0.0.1"},
		{"list", "--registry-via", "127.0.0.1:9082"},
	} {
		if code, _, _ := cliTestRun(args...); code != 2 {
			t.Errorf("args %q accepted (code %d)", args, code)
		}
	}
	// site-deploy-remote sets SITE_DEPLOY_REGISTRY_VIA whenever an argument is
	// "push", so other commands ignore it; push validates it like the flag.
	t.Setenv("SITE_DEPLOY_REGISTRY_VIA", "127.0.0.1:41234")
	if opts, err := parse([]string{"push", "img", "app-base:1"}); err != nil || opts.registryVia != "127.0.0.1:41234" {
		t.Errorf("push with environment: opts=%+v err=%v", opts, err)
	}
	if _, err := parse([]string{"delete", "push"}); err != nil {
		t.Errorf("delete with environment: %v", err)
	}
	t.Setenv("SITE_DEPLOY_REGISTRY_VIA", "example.com:9082")
	if _, err := parse([]string{"push", "img", "app-base:1"}); err == nil {
		t.Error("push accepted a non-loopback SITE_DEPLOY_REGISTRY_VIA")
	}
}

func TestCLIImagesAndCleanup(t *testing.T) {
	cliTestEnvironment(t)
	cleanup := `{"deleted":["base:0"],"garbage_collected":true,"pruned":["nginx:alpine"],"reclaimed":"94MB"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/registry":
			io.WriteString(w, `{"address":"127.0.0.1:9082","images":[`+
				`{"reference":"127.0.0.1:9082/base:1","digest":"sha256:0123456789abcdef0123","pushed":"2026-10-13T01:02:00Z","used_by":["blog","web"]},`+
				`{"reference":"127.0.0.1:9082/base:0","digest":"sha256:fedcba9876543210fedc"}]}`)
		case "/v1/cleanup":
			if r.Method != http.MethodPost {
				t.Errorf("cleanup method %s", r.Method)
			}
			io.WriteString(w, cleanup)
		}
	}))
	defer server.Close()
	code, stdout, stderr := cliTestRun("images", "--token", cliTestToken, "--server", server.URL)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "REFERENCE") ||
		!strings.Contains(lines[1], "sha256:0123456789ab ") || !strings.HasSuffix(lines[1], "blog,web") ||
		!strings.HasSuffix(lines[2], "-") {
		t.Fatalf("images: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, _ = cliTestRun("cleanup", "--token", cliTestToken, "--server", server.URL)
	if code != 0 || !strings.Contains(stdout, "Registry tags deleted: base:0") || !strings.Contains(stdout, "Docker images pruned: 1 (reclaimed 94MB)") {
		t.Fatalf("cleanup: code=%d stdout=%q", code, stdout)
	}
	cleanup = `{"garbage_collected":false,"errors":["registry unreachable"]}`
	for _, extra := range [][]string{nil, {"--json"}} {
		args := append([]string{"cleanup", "--token", cliTestToken, "--server", server.URL}, extra...)
		if code, _, stderr := cliTestRun(args...); code != 1 || !strings.Contains(stderr, "registry unreachable") {
			t.Errorf("cleanup errors %q: code=%d stderr=%q", extra, code, stderr)
		}
	}
}
