package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"site-deploy/internal/model"
)

func testApp(t *testing.T, configuration string) model.App {
	t.Helper()
	directory := t.TempDir()
	file := filepath.Join(directory, "compose.generated.json")
	if configuration == "" {
		configuration = `{"services":{"web":{"image":"nginx"},"db":{"image":"postgres"}}}`
	}
	if err := os.WriteFile(file, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	return model.App{
		Name: "example", Project: "site-example", Directory: directory, ComposeFile: file,
		Spec: model.Spec{Service: "web", Port: 8080, StartTimeout: "1500ms", SuspendMode: "stop"},
	}
}

func operation(t *testing.T, invocation command, app model.App, binary, host string) []string {
	t.Helper()
	prefix := []string{"--host", host, "compose", "--ansi", "never", "--project-name", app.Project, "--project-directory", app.Directory, "-f", app.ComposeFile, "--env-file", os.DevNull}
	if invocation.binary != binary || invocation.directory != app.Directory {
		t.Fatalf("incorrect executable/directory: %#v", invocation)
	}
	if len(invocation.args) <= len(prefix) || !reflect.DeepEqual(invocation.args[:len(prefix)], prefix) {
		t.Fatalf("incorrect compose selection: %q", invocation.args)
	}
	return invocation.args[len(prefix):]
}

func TestUpUsesExplicitProjectAndRestrictedEnvironment(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "other-project")
	t.Setenv("COMPOSE_FILE", "/private/compose.yaml")
	t.Setenv("COMPOSE_PROFILES", "disabled")
	t.Setenv("DOCKER_HOST", "tcp://untrusted:2375")
	t.Setenv("APPLICATION_SECRET", "must-not-reach-compose")
	app := testApp(t, "")
	docker := New("/usr/bin/custom-docker", "unix:///run/test-docker.sock")
	var calls [][]string
	docker.run = func(ctx context.Context, invocation command) (commandResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > app.Spec.StartDuration() {
			t.Fatal("startup subprocess is not bounded by the application timeout")
		}
		for _, value := range invocation.env {
			name := strings.SplitN(value, "=", 2)[0]
			if name != "HOME" && name != "PATH" && name != "LANG" && name != "LC_ALL" {
				t.Fatalf("ambient environment leaked: %s", name)
			}
		}
		args := operation(t, invocation, app, docker.binary, docker.host)
		calls = append(calls, args)
		if args[0] == "port" {
			return commandResult{stdout: []byte("127.0.0.1:32100\n")}, nil
		}
		return commandResult{}, nil
	}
	ready := false
	docker.ready = func(ctx context.Context, address string) error {
		ready = true
		if address != "127.0.0.1:32100" {
			t.Fatalf("unexpected readiness address: %q", address)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("readiness has no deadline")
		}
		return nil
	}
	upstream, err := docker.Up(context.Background(), app)
	if err != nil || upstream != "127.0.0.1:32100" || !ready {
		t.Fatalf("Up = %q, %v; ready = %v", upstream, err, ready)
	}
	want := [][]string{
		{"up", "--detach", "--no-build", "--wait", "--wait-timeout", "2"},
		{"port", "--protocol", "tcp", "--", "web", "8080"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("commands = %q; want %q", calls, want)
	}
}

func TestNewDefaults(t *testing.T) {
	docker := New("", "")
	if docker.binary != "docker" || docker.host != "unix:///var/run/docker.sock" {
		t.Fatalf("unexpected defaults: %#v", docker)
	}
}

func TestResumeUnpausesThenReconcilesMissingContainers(t *testing.T) {
	for _, test := range []struct {
		name string
		ps   string
		want []string
	}{
		{"paused", "{\"Service\":\"db\",\"State\":\"paused\"}\n{\"Service\":\"web\",\"State\":\"running\"}\n{\"Service\":\"db\",\"State\":\"paused\"}\n", []string{"ps", "unpause", "up", "port"}},
		{"externally deleted", "[]", []string{"ps", "up", "port"}},
		{"stopped", `[{"Service":"web","State":"exited"}]`, []string{"ps", "up", "port"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := testApp(t, "")
			docker := New("", "")
			var calls []string
			docker.run = func(ctx context.Context, invocation command) (commandResult, error) {
				args := operation(t, invocation, app, docker.binary, docker.host)
				calls = append(calls, args[0])
				switch args[0] {
				case "ps":
					return commandResult{stdout: []byte(test.ps)}, nil
				case "unpause":
					if !reflect.DeepEqual(args, []string{"unpause", "--", "db"}) {
						t.Fatalf("unpause args = %q", args)
					}
				case "port":
					return commandResult{stdout: []byte("127.0.0.1:32100")}, nil
				}
				return commandResult{}, nil
			}
			docker.ready = func(context.Context, string) error { return nil }
			if _, err := docker.Resume(context.Background(), app); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, test.want) {
				t.Fatalf("operations = %q; want %q", calls, test.want)
			}
		})
	}
}

func TestSuspendAndDownPreserveVolumes(t *testing.T) {
	app := testApp(t, "")
	docker := New("", "")
	var calls [][]string
	docker.run = func(ctx context.Context, invocation command) (commandResult, error) {
		calls = append(calls, operation(t, invocation, app, docker.binary, docker.host))
		return commandResult{}, nil
	}
	for _, mode := range []string{"", "stop", "pause"} {
		app.Spec.SuspendMode = mode
		if err := docker.Suspend(context.Background(), app); err != nil {
			t.Fatal(err)
		}
	}
	if err := docker.Down(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"stop", "--timeout", "20"}, {"stop", "--timeout", "20"}, {"pause"}, {"down", "--remove-orphans", "--timeout", "20"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("commands = %q; want %q", calls, want)
	}
	app.Spec.SuspendMode = "destroy"
	if err := docker.Suspend(context.Background(), app); err == nil {
		t.Fatal("invalid suspension mode accepted")
	}
}

func TestInspectRequiresEveryServiceRunningAndHealthy(t *testing.T) {
	for _, test := range []struct {
		name          string
		ps            string
		configuration string
		suspendMode   string
		running       bool
		partial       bool
	}{
		{name: "array", ps: `[{"Service":"web","State":"running","Health":"healthy"},{"Service":"db","State":"running","Health":""}]`, running: true},
		{name: "json lines", ps: "{\"Service\":\"web\",\"State\":\"running\"}\n{\"Service\":\"db\",\"State\":\"running\",\"Health\":\"healthy\"}\n", running: true},
		{name: "empty", ps: "[]"},
		{name: "all stopped", ps: `[{"Service":"web","State":"exited"},{"Service":"db","State":"exited"}]`},
		{name: "deleted dependency", ps: `[{"Service":"web","State":"running"}]`, partial: true},
		{name: "paused dependency", ps: `[{"Service":"web","State":"running"},{"Service":"db","State":"paused"}]`, partial: true},
		{name: "exited dependency", ps: `[{"Service":"web","State":"running"},{"Service":"db","State":"exited"}]`, partial: true},
		{name: "stopped route with running dependency", ps: `[{"Service":"web","State":"exited"},{"Service":"db","State":"running"}]`, partial: true},
		{name: "intentional pause", suspendMode: "pause", ps: `[{"Service":"web","State":"paused"},{"Service":"db","State":"paused"}]`},
		{name: "unexpected pause in stop mode", ps: `[{"Service":"web","State":"paused"},{"Service":"db","State":"paused"}]`, partial: true},
		{name: "incomplete paused stack", suspendMode: "pause", ps: `[{"Service":"web","State":"paused"}]`, partial: true},
		{name: "mixed pause and running", suspendMode: "pause", ps: `[{"Service":"web","State":"paused"},{"Service":"db","State":"running"}]`, partial: true},
		{name: "completed migration", configuration: `{"services":{"web":{"depends_on":{"db":{"condition":"service_completed_successfully"}}},"db":{}}}`, ps: `[{"Service":"web","State":"running"},{"Service":"db","State":"exited","ExitCode":0}]`, running: true},
		{name: "paused with completed migration", suspendMode: "pause", configuration: `{"services":{"web":{"depends_on":{"db":{"condition":"service_completed_successfully"}}},"db":{}}}`, ps: `[{"Service":"web","State":"paused"},{"Service":"db","State":"exited","ExitCode":0}]`},
		{name: "failed migration", configuration: `{"services":{"web":{"depends_on":{"db":{"condition":"service_completed_successfully"}}},"db":{}}}`, ps: `[{"Service":"web","State":"running"},{"Service":"db","State":"exited","ExitCode":1}]`, partial: true},
		{name: "unhealthy", ps: `[{"Service":"web","State":"running","Health":"unhealthy"},{"Service":"db","State":"running"}]`, partial: true},
		{name: "health starting", ps: `[{"Service":"web","State":"running","Health":"starting"},{"Service":"db","State":"running"}]`, partial: true},
		{name: "restarting", ps: `[{"Service":"web","State":"restarting"},{"Service":"db","State":"running"}]`, partial: true},
		{name: "not substring", ps: `[{"Service":"web","State":"not running"},{"Service":"db","State":"running"}]`, partial: true},
		{name: "missing replica", configuration: `{"services":{"web":{},"db":{"deploy":{"replicas":2}}}}`, ps: `[{"Service":"web","State":"running"},{"Service":"db","State":"running"}]`, partial: true},
		{name: "disabled profile", configuration: `{"services":{"web":{},"db":{"profiles":["optional"]}}}`, ps: `[{"Service":"web","State":"running"}]`, running: true},
		{name: "ignored orphan", ps: `[{"Service":"web","State":"running"},{"Service":"db","State":"running"},{"Service":"old","State":"exited"}]`, running: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := testApp(t, test.configuration)
			if test.suspendMode != "" {
				app.Spec.SuspendMode = test.suspendMode
			}
			docker := New("", "")
			portLookups := 0
			docker.run = func(ctx context.Context, invocation command) (commandResult, error) {
				args := operation(t, invocation, app, docker.binary, docker.host)
				if args[0] == "ps" {
					if !reflect.DeepEqual(args, []string{"ps", "--all", "--format", "json", "--orphans=false"}) {
						t.Fatalf("ps args = %q", args)
					}
					return commandResult{stdout: []byte(test.ps)}, nil
				}
				if args[0] == "port" {
					portLookups++
					if !reflect.DeepEqual(args, []string{"port", "--protocol", "tcp", "--", "web", "8080"}) {
						t.Fatalf("wrong routed endpoint: %q", args)
					}
					return commandResult{stdout: []byte("127.0.0.1:32100")}, nil
				}
				t.Fatalf("unexpected operation %q", args)
				return commandResult{}, nil
			}
			status, err := docker.Inspect(context.Background(), app)
			if err != nil {
				t.Fatal(err)
			}
			if status.Running != test.running || status.Partial != test.partial || status.Suspended != (!test.running && !test.partial) {
				t.Fatalf("Inspect = %#v; running = %v, partial = %v", status, test.running, test.partial)
			}
			if test.running {
				if status.Upstream != "127.0.0.1:32100" || portLookups != 1 {
					t.Fatalf("incorrect running endpoint: %#v, port lookups = %d", status, portLookups)
				}
			} else if status.Upstream != "" || portLookups != 0 {
				t.Fatalf("inactive app has endpoint: %#v, port lookups = %d", status, portLookups)
			}
		})
	}
}

func TestLoopbackAddressRejectsUnsafePublications(t *testing.T) {
	for _, output := range []string{"", "0.0.0.0:80", "192.0.2.1:80", "[::1]:80", "localhost:80", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:garbage", "127.0.0.1:8080\n0.0.0.0:80", "127.0.0.1:8080\n127.0.0.1:9090"} {
		t.Run(output, func(t *testing.T) {
			if address, err := loopbackAddress([]byte(output)); err == nil {
				t.Fatalf("unsafe address accepted: %q", address)
			}
		})
	}
	if address, err := loopbackAddress([]byte(" 127.0.0.1:8080\r\n")); err != nil || address != "127.0.0.1:8080" {
		t.Fatalf("valid address = %q, %v", address, err)
	}
}

func TestLifecycleReviewInspectRetainsPartialStatusForBrokenPublication(t *testing.T) {
	app := testApp(t, "")
	docker := New("", "")
	docker.run = func(ctx context.Context, invocation command) (commandResult, error) {
		args := operation(t, invocation, app, docker.binary, docker.host)
		switch args[0] {
		case "ps":
			return commandResult{stdout: []byte(`[{"Service":"web","State":"running"},{"Service":"db","State":"running"}]`)}, nil
		case "port":
			return commandResult{stdout: []byte("0.0.0.0:32100")}, nil
		default:
			t.Fatalf("unexpected operation %q", args)
			return commandResult{}, nil
		}
	}
	status, err := docker.Inspect(context.Background(), app)
	if err == nil || !status.Partial || status.Running || status.Suspended {
		t.Fatalf("known running resources lost on endpoint failure: status=%+v error=%v", status, err)
	}
}

func TestContainerListingRejectsMalformedAndTruncatedOutput(t *testing.T) {
	app := testApp(t, "")
	for _, result := range []commandResult{
		{stdout: []byte("not json")},
		{stdout: []byte(`[{"Service":"web"}] trailing`)},
		{stdout: []byte(`[]`), stdoutTruncated: true},
	} {
		docker := New("", "")
		docker.run = func(context.Context, command) (commandResult, error) { return result, nil }
		if _, err := docker.Inspect(context.Background(), app); err == nil {
			t.Fatalf("invalid listing accepted: %#v", result)
		}
	}
}

func TestLogsAreBoundedAndNeverFollow(t *testing.T) {
	app := testApp(t, "")
	docker := New("", "")
	called := false
	docker.run = func(ctx context.Context, invocation command) (commandResult, error) {
		called = true
		args := operation(t, invocation, app, docker.binary, docker.host)
		if !reflect.DeepEqual(args, []string{"logs", "--no-color", "--tail", "200"}) {
			t.Fatalf("log arguments = %q", args)
		}
		return commandResult{stdout: bytes.Repeat([]byte{'x'}, maxCommandOutput), stdoutTruncated: true}, nil
	}
	logs, err := docker.Logs(context.Background(), app, 200)
	if err != nil || !called || !strings.HasSuffix(logs, "[output truncated at 1048576 bytes]\n") || len(logs) > maxCommandOutput+100 {
		t.Fatalf("Logs returned length %d, called = %v, error = %v", len(logs), called, err)
	}
	called = false
	for _, tail := range []int{-1, 0, maxLogTail + 1} {
		if _, err := docker.Logs(context.Background(), app, tail); err == nil {
			t.Fatalf("unbounded log tail accepted: %d", tail)
		}
	}
	if called {
		t.Fatal("invalid log request ran Docker")
	}
}

func TestErrorsAreContextualBoundedAndPrintable(t *testing.T) {
	app := testApp(t, "")
	docker := New("", "")
	docker.run = func(context.Context, command) (commandResult, error) {
		return commandResult{
			stdout: []byte("application secret: must-not-be-in-diagnostic"),
			stderr: []byte("\x1b[31mpermission denied\x1b[0m\r\n\x1b]8;;https://bad.example\aDocker socket\x1b]8;;\a\x00" + strings.Repeat("x", 5000)),
		}, errors.New("exit status 1")
	}
	err := docker.Suspend(context.Background(), app)
	if err == nil || !strings.Contains(err.Error(), `docker compose stop for application "example" failed`) || !strings.Contains(err.Error(), "permission denied Docker socket") {
		t.Fatalf("missing useful error context: %v", err)
	}
	if len(err.Error()) > 2300 || strings.ContainsAny(err.Error(), "\x1b\r\n\x00") || strings.Contains(err.Error(), "must-not-be-in-diagnostic") {
		t.Fatalf("unsafe diagnostic: %v", err)
	}
}

func TestStartupDeadlineIncludesReadiness(t *testing.T) {
	app := testApp(t, "")
	app.Spec.StartTimeout = "20ms"
	docker := New("", "")
	docker.run = func(ctx context.Context, invocation command) (commandResult, error) {
		args := operation(t, invocation, app, docker.binary, docker.host)
		if args[0] == "port" {
			return commandResult{stdout: []byte("127.0.0.1:32100")}, nil
		}
		return commandResult{}, nil
	}
	docker.ready = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if _, err := docker.Up(context.Background(), app); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup deadline not preserved: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	docker.run = func(context.Context, command) (commandResult, error) {
		t.Fatal("cancelled operation ran Docker")
		return commandResult{}, nil
	}
	if err := docker.Down(ctx, app); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled operation = %v", err)
	}
}

func TestTCPReadiness(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitTCP(ctx, listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := waitTCP(ctx, address); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closed endpoint readiness = %v", err)
	}
}

func TestRunCommandCapsOutputAndCancels(t *testing.T) {
	result, err := runCommand(context.Background(), command{
		binary: os.Args[0], args: []string{"-test.run=^TestDockerCommandHelper$"}, directory: t.TempDir(),
		env: []string{"SITE_DEPLOY_ENGINE_HELPER=large"},
	})
	if err == nil || len(result.stdout) != maxCommandOutput || len(result.stderr) != maxCommandOutput || !result.stdoutTruncated || !result.stderrTruncated {
		t.Fatalf("unbounded command output: stdout = %d, stderr = %d, truncation = %v/%v, err = %v", len(result.stdout), len(result.stderr), result.stdoutTruncated, result.stderrTruncated, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = runCommand(ctx, command{
		binary: os.Args[0], args: []string{"-test.run=^TestDockerCommandHelper$"}, directory: t.TempDir(),
		env: []string{"SITE_DEPLOY_ENGINE_HELPER=wait"},
	})
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("command was not cancelled promptly: elapsed = %v, error = %v", time.Since(started), err)
	}
}

func TestDockerCommandHelper(t *testing.T) {
	switch os.Getenv("SITE_DEPLOY_ENGINE_HELPER") {
	case "large":
		fmt.Fprint(os.Stdout, strings.Repeat("x", maxCommandOutput*2))
		fmt.Fprint(os.Stderr, strings.Repeat("y", maxCommandOutput*2))
		os.Exit(3)
	case "wait":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
}

func TestCommandUsesHostAndControlledEnvironment(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "elsewhere")
	t.Setenv("SITE_DEPLOY_TOKEN", "must-not-leak")
	docker := New("docker", "unix:///run/test.sock")
	var seen command
	docker.run = func(_ context.Context, invocation command) (commandResult, error) {
		seen = invocation
		return commandResult{stdout: []byte("ok\n")}, nil
	}
	output, err := docker.Command(context.Background(), "image", "prune", "--all", "--force")
	if err != nil || string(output) != "ok\n" {
		t.Fatalf("Command = %q, %v", output, err)
	}
	if strings.Join(seen.args, " ") != "--host unix:///run/test.sock image prune --all --force" {
		t.Fatalf("args = %q", seen.args)
	}
	for _, variable := range seen.env {
		if strings.HasPrefix(variable, "DOCKER_") || strings.HasPrefix(variable, "SITE_DEPLOY_") {
			t.Fatalf("leaked environment %q", variable)
		}
	}
	docker.run = func(context.Context, command) (commandResult, error) {
		return commandResult{stderr: []byte("\x1b[31mError: No such container: x\x1b[0m\n")}, errors.New("exit status 1")
	}
	if _, err := docker.Command(context.Background(), "container", "inspect", "x"); err == nil || !strings.Contains(err.Error(), "docker container failed: exit status 1: Error: No such container: x") {
		t.Fatalf("error = %v", err)
	}
	docker.run = func(context.Context, command) (commandResult, error) {
		return commandResult{stdoutTruncated: true}, nil
	}
	if _, err := docker.Command(context.Background(), "image", "ls"); err == nil {
		t.Fatal("truncated output accepted")
	}
}

func TestCopyVolumesPausesOnlyForTheCopyAndCleansUpFailures(t *testing.T) {
	source, target := testApp(t, ""), testApp(t, "")
	source.Name, source.Project = "main", "site-main"
	target.Name, target.Project = "preview", "site-preview"
	for _, test := range []struct {
		name    string
		fail    string // the first argument of the call that fails
		missing bool   // the copy image is not present yet
		want    []string
	}{
		{"copies", "", true, []string{"image inspect", "pull", "site-preview create", "site-main ps", "pause a1 b2", "run", "unpause a1 b2"}},
		{"copy fails", "run", false, []string{"image inspect", "site-preview create", "site-main ps", "pause a1 b2", "run", "unpause a1 b2", "site-preview down", "volume rm"}},
		{"pause fails", "pause", false, []string{"image inspect", "site-preview create", "site-main ps", "pause a1 b2", "unpause a1 b2", "site-preview down", "volume rm"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			docker := New("docker", "unix:///run/test-docker.sock")
			var calls []string
			var run []string
			docker.run = func(ctx context.Context, invocation command) (commandResult, error) {
				args := invocation.args[2:] // after --host HOST
				var call string
				if args[0] == "compose" {
					project := args[4]
					args = args[11:]
					call = project + " " + args[0]
				} else {
					call = strings.Join(args[:2], " ")
					switch args[0] {
					case "pause", "unpause":
						call = strings.Join(args, " ")
					case "pull", "run":
						call = args[0]
					}
				}
				calls = append(calls, call)
				if args[0] == "run" {
					run = args
				}
				if call == "image inspect" && test.missing {
					return commandResult{}, errors.New("no such image")
				}
				if args[0] == test.fail {
					return commandResult{}, errors.New("failed")
				}
				if call == "site-main ps" {
					return commandResult{stdout: []byte("a1\nb2\n")}, nil
				}
				return commandResult{}, nil
			}
			err := docker.CopyVolumes(context.Background(), source, target, []string{"data", "uploads"})
			if (err != nil) != (test.fail != "") {
				t.Fatalf("error %v", err)
			}
			if !reflect.DeepEqual(calls, test.want) {
				t.Fatalf("calls\n%q\nwant\n%q", calls, test.want)
			}
			if test.fail == "pause" {
				return
			}
			want := []string{"run", "--rm", "--network", "none", "--entrypoint", "sh",
				"--mount", "type=volume,source=site-main_data,target=/from/data,readonly",
				"--mount", "type=volume,source=site-preview_data,target=/to/data",
				"--mount", "type=volume,source=site-main_uploads,target=/from/uploads,readonly",
				"--mount", "type=volume,source=site-preview_uploads,target=/to/uploads",
				DefaultCopyImage, "-c", copyVolumesScript}
			if !reflect.DeepEqual(run, want) {
				t.Fatalf("run %q", run)
			}
		})
	}
}

func TestExistingVolumesMatchesExactNames(t *testing.T) {
	app := testApp(t, "")
	docker := New("docker", "")
	docker.run = func(_ context.Context, invocation command) (commandResult, error) {
		return commandResult{stdout: []byte("site-example_data\nsite-example_data2\nother_cache\n")}, nil
	}
	existing, err := docker.ExistingVolumes(context.Background(), app, []string{"data", "cache"})
	if err != nil || !reflect.DeepEqual(existing, []string{"site-example_data"}) {
		t.Fatalf("existing %q %v", existing, err)
	}
}
