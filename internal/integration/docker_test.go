//go:build integration

// Package integration exercises real Docker and Caddy with disposable,
// uniquely named resources. Run only when explicitly opting into this test.
package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"site-deploy/internal/api"
	"site-deploy/internal/caddy"
	"site-deploy/internal/cli"
	"site-deploy/internal/engine"
	"site-deploy/internal/gateway"
	"site-deploy/internal/hostpaths"
	"site-deploy/internal/model"
	"site-deploy/internal/orchestrator"
)

func TestNamespacedDockerCaddyLifecycle(t *testing.T) {
	if os.Getenv("SITE_DEPLOY_INTEGRATION") != "1" {
		t.Skip("set SITE_DEPLOY_INTEGRATION=1 to create isolated Docker test resources")
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	namespace := "sdtest-" + hex.EncodeToString(random[:])
	t.Logf("test namespace: %s", namespace)
	root := t.TempDir()
	domain := namespace + ".localhost"
	image := os.Getenv("SITE_DEPLOY_TEST_IMAGE")
	if image == "" {
		image = "node:24-bookworm-slim"
	}
	caddyImage := os.Getenv("SITE_DEPLOY_TEST_CADDY_IMAGE")
	if caddyImage == "" {
		caddyImage = "caddy:2"
	}
	docker := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}
	if output, err := docker("info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Fatalf("Docker unavailable: %v %s", err, output)
	}
	// An allowed host directory, mounted read-only by the app below.
	hostDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "greeting"), []byte("hello from the host"), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := orchestrator.New(filepath.Join(root, "data"), engine.New("docker", ""), hostpaths.Policy{{Path: hostDir}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	prefix, err := os.ReadFile(filepath.Join(root, "data", "project-prefix"))
	if err != nil {
		t.Fatal(err)
	}
	project := strings.TrimSpace(string(prefix)) + namespace
	t.Logf("test Compose project: %s", project)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idleDone := make(chan struct{})
	idleStart := make(chan struct{})
	go func() {
		defer close(idleDone)
		select {
		case <-ctx.Done():
			return
		case <-idleStart:
		}
		m.RunIdle(ctx, 100*time.Millisecond)
	}()
	defer func() { cancel(); <-idleDone }()
	const token = "site-deploy-integration-private-token-12345678"
	control := httptest.NewServer(api.New(m, token, filepath.Join(root, "data", "staging"), nil))
	defer control.Close()
	proxy := httptest.NewServer(gateway.New(m))
	defer proxy.Close()
	// Every cleanup names one exact resource owned by this test. Never prune or
	// select containers/volumes belonging to any concurrent agent or deployment.
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		if app, err := m.Get(namespace); err == nil {
			if err := m.Delete(cleanupCtx, namespace); err != nil {
				t.Errorf("cleanup deployment: %v", err)
				if output, err := docker("compose", "--project-name", project, "--project-directory", app.Directory, "-f", app.ComposeFile, "down", "--remove-orphans", "--volumes"); err != nil {
					t.Errorf("cleanup exact test project: %v %s", err, output)
				}
			}
		}
		if output, err := docker("volume", "rm", project+"_data"); err != nil && !strings.Contains(string(output), "no such volume") {
			t.Errorf("cleanup test volume: %v %s", err, output)
		}
	}()
	appFiles := func(domain string) map[string]string {
		return map[string]string{
			"compose.yaml": fmt.Sprintf("x-site-deploy:\n  domain: %s\n  service: web\n  port: 8080\n  idle_timeout: 1s\n  start_timeout: 60s\nservices:\n  web:\n    image: %s\n    restart: always\n    command: [node, /app/server.js]\n    volumes: [\".:/app\", \"data:/data\", \"%s:/host:ro\"]\n    cpus: 0.25\n    mem_limit: 128m\nvolumes:\n  data: {}\n", domain, image, hostDir),
			"server.js": `const http = require('http'); const fs = require('fs');
fs.writeFileSync('/app/compose.generated.json', 'This application file must never be executed as Compose configuration.');
http.createServer((req,res)=>{if(req.url==='/host'){res.end(fs.readFileSync('/host/greeting','utf8'));return;}if(req.url==='/stream'){res.writeHead(200,{'Content-Type':'text/plain'});res.write('start\n');setTimeout(()=>res.end('end\n'),1800);return;}let body='';req.on('data',chunk=>body+=chunk);req.on('end',()=>{const file='/data/count';let count=fs.existsSync(file)?Number(fs.readFileSync(file,'utf8')):0;fs.writeFileSync(file,String(++count));res.setHeader('Content-Type','application/json');res.end(JSON.stringify({count,body,host:req.headers.host,uri:req.url}));});}).listen(8080,'0.0.0.0');process.on('SIGTERM',()=>process.exit(0));`,
		}
	}
	archivePath := filepath.Join(root, "upload.zip")
	writeArchive(t, archivePath, appFiles(domain))
	command := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args = append([]string{"--server", control.URL, "--token", token}, args...)
		if code := cli.Run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("CLI %s: exit %d: %s", args[4:], code, stderr.String())
		}
		return stdout.String()
	}
	command("deploy", namespace, archivePath, "--json")
	app, err := m.Get(namespace)
	if err != nil || app.State != model.StateRunning {
		t.Fatalf("deployment: %+v %v", app, err)
	}
	if !strings.HasPrefix(app.Upstream, "127.0.0.1:") {
		t.Fatalf("non-loopback publication %s", app.Upstream)
	}
	if app.Project != project || filepath.Dir(app.ComposeFile) == app.Directory {
		t.Fatalf("project/config isolation failed: %+v", app)
	}
	// startCaddy runs a namespaced Caddy with the generated configuration for
	// sites, in the host network namespace on a dynamically chosen port.
	var caddyNames []string
	defer func() {
		for _, name := range caddyNames {
			if output, err := docker("rm", "--force", "--volumes", name); err != nil && !strings.Contains(string(output), "No such container") {
				t.Errorf("cleanup Caddy: %v %s", err, output)
			}
		}
	}()
	startCaddy := func(name string, sites []model.Site) (address, container string) {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address = listener.Addr().String()
		listener.Close()
		config, err := caddy.Config(sites, strings.TrimPrefix(proxy.URL, "http://"), caddy.HTTP)
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(config, &cfg); err != nil {
			t.Fatal(err)
		}
		cfg["admin"] = map[string]any{"disabled": true}
		server := cfg["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["site-deploy"].(map[string]any)
		server["listen"] = []string{address}
		config, _ = json.Marshal(cfg)
		configPath := filepath.Join(root, name+".json")
		if err := os.WriteFile(configPath, config, 0600); err != nil {
			t.Fatal(err)
		}
		container = namespace + "-" + name
		caddyNames = append(caddyNames, container)
		if output, err := docker("run", "--detach", "--name", container, "--label", "site-deploy.test.namespace="+namespace, "--network", "host", "--mount", "type=bind,src="+configPath+",dst=/etc/caddy/test.json,readonly", caddyImage, "caddy", "run", "--config", "/etc/caddy/test.json"); err != nil {
			t.Fatalf("start namespaced Caddy: %v %s", err, output)
		}
		return address, container
	}
	caddyAddress, caddyName := startCaddy("caddy", []model.Site{{Domain: domain}})
	client := &http.Client{Timeout: 20 * time.Second}
	request := func(path, body string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, "http://"+caddyAddress+path, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Host = domain
		return client.Do(req)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := request("/", "warmup")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			output, _ := docker("logs", caddyName)
			t.Fatalf("Caddy did not become ready: %v %s", err, output)
		}
		time.Sleep(100 * time.Millisecond)
	}
	hostResponse, err := request("/host", "")
	if err != nil {
		t.Fatal(err)
	}
	greeting, _ := io.ReadAll(hostResponse.Body)
	hostResponse.Body.Close()
	if hostResponse.StatusCode != 200 || string(greeting) != "hello from the host" {
		t.Fatalf("host path mount: status %d body %q", hostResponse.StatusCode, greeting)
	}
	command("suspend", namespace)
	app, _ = m.Get(namespace)
	if app.State != model.StateSuspended {
		t.Fatal("app did not suspend")
	}
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			body := fmt.Sprintf("cold-body-%d", i)
			resp, err := request("/", body)
			if err != nil {
				failures <- err
				return
			}
			defer resp.Body.Close()
			var result struct {
				Body, Host string
				Count      int
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				failures <- err
				return
			}
			if resp.StatusCode != 200 || result.Body != body || result.Host != domain || result.Count < 2 {
				failures <- fmt.Errorf("cold request lost body/host/volume: status %d result %+v", resp.StatusCode, result)
			}
		}(i)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	query := "/?tag=one&tag=two&literal=x;y&signed=one%20two"
	queryResponse, err := request(query, "")
	if err != nil {
		t.Fatal(err)
	}
	var queryResult struct {
		URI string `json:"uri"`
	}
	decodeErr := json.NewDecoder(queryResponse.Body).Decode(&queryResult)
	queryResponse.Body.Close()
	if decodeErr != nil || queryResult.URI != query {
		t.Fatalf("query changed through Caddy/gateway: %q %v", queryResult.URI, decodeErr)
	}
	// Stop only this test's service outside the daemon. The failing request
	// invalidates cached readiness; the next request recovers without replay.
	containerIDs, err := docker("ps", "-q", "--filter", "label=com.docker.compose.project="+project, "--filter", "label=com.docker.compose.service=web")
	ids := strings.Fields(string(containerIDs))
	if err != nil || len(ids) != 1 {
		t.Fatalf("find exact test container: %v %s", err, containerIDs)
	}
	settings, err := docker("inspect", "--format", "{{.HostConfig.RestartPolicy.Name}} {{.HostConfig.Memory}} {{.HostConfig.NanoCpus}}", ids[0])
	if err != nil || strings.TrimSpace(string(settings)) != "unless-stopped 134217728 250000000" {
		t.Fatalf("restart/resource settings: %v %s", err, settings)
	}
	if output, err := docker("stop", "--time", "1", ids[0]); err != nil {
		t.Fatalf("stop exact test service: %v %s", err, output)
	}
	failedResponse, err := request("/", "failed-request")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, failedResponse.Body)
	failedResponse.Body.Close()
	if failedResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected first failed upstream response, got %d", failedResponse.StatusCode)
	}
	recoveredResponse, err := request("/", "recovered-request")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, recoveredResponse.Body)
	recoveredResponse.Body.Close()
	if recoveredResponse.StatusCode != http.StatusOK {
		t.Fatalf("next request did not repair externally stopped app: %d", recoveredResponse.StatusCode)
	}
	close(idleStart)
	stream, err := request("/stream", "")
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 6)
	if _, err := io.ReadFull(stream.Body, first); err != nil {
		t.Fatal(err)
	}
	if string(first) != "start\n" {
		t.Fatalf("stream first chunk: %q", first)
	}
	time.Sleep(1300 * time.Millisecond)
	app, _ = m.Get(namespace)
	if app.State != model.StateRunning {
		t.Fatalf("idle stopped active stream: %s", app.State)
	}
	if _, err := io.Copy(io.Discard, stream.Body); err != nil {
		t.Fatal(err)
	}
	stream.Body.Close()
	deadline = time.Now().Add(30 * time.Second)
	for {
		app, _ = m.Get(namespace)
		if app.State == model.StateSuspended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle suspension did not happen: %+v", app)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Basic auth is checked by Caddy before the gateway, so requests without
	// valid credentials must neither reach nor wake the suspended application.
	const password = "integration-secret"
	authAddress, authName := startCaddy("caddy-auth", []model.Site{{Domain: domain, BasicAuth: []model.BasicAuthUser{
		{User: "preview", Hash: "$2a$14$O4UeNfvX7xUhP9mj6tjWIOkvvB/sTqKipfaXtgWqmJ7MZjMg/l9me"},
	}}})
	authGet := func(user, pass string) int {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			req, err := http.NewRequest(http.MethodGet, "http://"+authAddress+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = domain
			if user != "" {
				req.SetBasicAuth(user, pass)
			}
			resp, err := client.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return resp.StatusCode
			}
			if time.Now().After(deadline) {
				output, _ := docker("logs", authName)
				t.Fatalf("authenticating Caddy did not answer: %v %s", err, output)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	for _, credentials := range [][2]string{{"", ""}, {"preview", "wrong"}, {"intruder", password}} {
		if status := authGet(credentials[0], credentials[1]); status != http.StatusUnauthorized {
			t.Fatalf("credentials %q: HTTP %d, want 401", credentials[0], status)
		}
	}
	if app, _ = m.Get(namespace); app.State != model.StateSuspended {
		t.Fatalf("unauthenticated requests woke the application: %s", app.State)
	}
	if status := authGet("preview", password); status != http.StatusOK {
		t.Fatalf("valid credentials: HTTP %d, want 200", status)
	}
	if app, _ = m.Get(namespace); app.State != model.StateRunning {
		t.Fatalf("authenticated request did not wake the application: %s", app.State)
	}
	command("resume", namespace)
	command("logs", namespace, "--tail", "5")
	// A second application starts with a copy of the first one's data volume,
	// which keeps serving afterwards, and the two then diverge. Requests go
	// through the gateway, which wakes applications the idle scan suspends.
	count := func(host string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, proxy.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result struct{ Count int }
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result.Count
	}
	copyName := namespace + "-copy"
	copyProject := strings.TrimSpace(string(prefix)) + copyName
	// The idle scan may be stopping the copy, which makes delete refuse.
	deleteCopy := func(ctx context.Context) error {
		for {
			err := m.Delete(ctx, copyName)
			if !errors.Is(err, orchestrator.ErrBusy) {
				return err
			}
			select {
			case <-ctx.Done():
				return err
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		if _, err := m.Get(copyName); err == nil {
			if err := deleteCopy(cleanupCtx); err != nil {
				t.Errorf("cleanup copy deployment: %v", err)
			}
		}
		if output, err := docker("volume", "rm", copyProject+"_data"); err != nil && !strings.Contains(string(output), "no such volume") {
			t.Errorf("cleanup copy volume: %v %s", err, output)
		}
	}()
	before := count(domain)
	copyArchive := filepath.Join(root, "copy.zip")
	writeArchive(t, copyArchive, appFiles(copyName+".localhost"))
	command("deploy", copyName, copyArchive, "--copy-volumes-from", namespace)
	copied, err := m.Get(copyName)
	if err != nil || copied.State != model.StateRunning {
		t.Fatalf("copy deployment: %+v %v", copied, err)
	}
	if got := count(copyName + ".localhost"); got != before+1 {
		t.Fatalf("copy counted %d, want %d", got, before+1)
	}
	if got := count(domain); got != before+1 {
		t.Fatalf("source counted %d after copy, want %d", got, before+1)
	}
	// The volume outlives delete, and a copy never replaces it.
	deleteCtx, stopDelete := context.WithTimeout(context.Background(), 60*time.Second)
	defer stopDelete()
	if err := deleteCopy(deleteCtx); err != nil {
		t.Fatalf("delete copy: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := cli.Run([]string{"--server", control.URL, "--token", token, "deploy", copyName, copyArchive, "--copy-volumes-from", namespace}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "already exists, kept from an earlier deployment") {
		t.Fatalf("copy over a kept volume: exit %d: %s", code, stderr.String())
	}
	// Deleting, as a redeploy does, breaks an open stream instead of refusing.
	open, err := request("/stream", "")
	if err != nil {
		t.Fatal(err)
	}
	opened := make([]byte, len("start\n"))
	if _, err := io.ReadFull(open.Body, opened); err != nil {
		t.Fatalf("stream before delete: %v", err)
	}
	deleteStarted := time.Now()
	command("delete", namespace)
	rest, _ := io.ReadAll(open.Body)
	open.Body.Close()
	if strings.Contains(string(rest), "end") {
		t.Fatalf("stream finished normally despite deletion after %s: %q", time.Since(deleteStarted), rest)
	}
	if _, err := m.Get(namespace); err == nil {
		t.Fatal("deleted app retained")
	}
	if output, err := docker("volume", "inspect", project+"_data"); err != nil {
		t.Fatalf("delete did not preserve named volume: %v %s", err, output)
	}
	t.Log("verified ZIP upload, writable-bundle/control isolation, project namespace, restart/resource settings, exact queries, crash recovery, Caddy proxy, concurrent cold POSTs, volume persistence, copying volumes to a new deployment, read-only host path mount, stream protection, idle stop, basic auth without wake-up, resume, logs, and deletion breaking an open stream")
}

func writeArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	for name, body := range files {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
