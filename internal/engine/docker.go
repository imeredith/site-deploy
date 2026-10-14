package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"site-deploy/internal/model"
)

const (
	maxCommandOutput = 1 << 20
	maxComposeConfig = 8 << 20
	maxLogTail       = 10000
)

// Docker runs a local Docker Compose installation. Its subprocesses receive an
// explicit Docker host and project, rather than inheriting Compose settings.
type Docker struct {
	binary string
	host   string
	run    commandRunner
	ready  func(context.Context, string) error
}

type command struct {
	binary    string
	args      []string
	directory string
	env       []string
}

type commandResult struct {
	stdout          []byte
	stderr          []byte
	stdoutTruncated bool
	stderrTruncated bool
}

type commandRunner func(context.Context, command) (commandResult, error)

var _ Runtime = (*Docker)(nil)

// New constructs a runtime using docker and the local Unix socket by default.
func New(binary, host string) *Docker {
	if binary == "" {
		binary = "docker"
	}
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	return &Docker{binary: binary, host: host, run: runCommand, ready: waitTCP}
}

func (d *Docker) Up(ctx context.Context, app model.App) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, app.Spec.StartDuration())
	defer cancel()
	return d.start(ctx, app)
}

func (d *Docker) start(ctx context.Context, app model.App) (string, error) {
	timeout := app.Spec.StartDuration()
	seconds := timeout / time.Second
	if timeout%time.Second != 0 {
		seconds++
	}
	if _, err := d.compose(ctx, app, "up", "--detach", "--no-build", "--wait", "--wait-timeout", strconv.FormatInt(int64(seconds), 10)); err != nil {
		return "", err
	}
	upstream, err := d.port(ctx, app)
	if err != nil {
		return "", err
	}
	if err := d.ready(ctx, upstream); err != nil {
		return "", fmt.Errorf("application %q did not become reachable: %w", app.Name, err)
	}
	return upstream, nil
}

// Resume reconciles the whole Compose project, including containers removed
// outside the orchestrator. A paused project must be unpaused before up --wait.
func (d *Docker) Resume(ctx context.Context, app model.App) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, app.Spec.StartDuration())
	defer cancel()
	containers, err := d.containers(ctx, app)
	if err != nil {
		return "", err
	}
	paused := make(map[string]bool)
	for _, container := range containers {
		if container.State == "paused" && container.Service != "" {
			paused[container.Service] = true
		}
	}
	if len(paused) > 0 {
		services := make([]string, 0, len(paused))
		for service := range paused {
			services = append(services, service)
		}
		sort.Strings(services)
		args := append([]string{"unpause", "--"}, services...)
		if _, err := d.compose(ctx, app, args...); err != nil {
			return "", err
		}
	}
	return d.start(ctx, app)
}

func (d *Docker) Suspend(ctx context.Context, app model.App) error {
	var args []string
	switch app.Spec.SuspendMode {
	case "", "stop":
		args = []string{"stop", "--timeout", "20"}
	case "pause":
		args = []string{"pause"}
	default:
		return fmt.Errorf("unsupported suspension mode %q", app.Spec.SuspendMode)
	}
	_, err := d.compose(ctx, app, args...)
	return err
}

// Down removes containers and networks. Deliberately never request volume
// deletion: a future deployment with the same project can reuse named volumes.
func (d *Docker) Down(ctx context.Context, app model.App) error {
	_, err := d.compose(ctx, app, "down", "--remove-orphans", "--timeout", "20")
	return err
}

func (d *Docker) Inspect(ctx context.Context, app model.App) (Status, error) {
	expected, err := expectedServices(app)
	if err != nil {
		return Status{}, err
	}
	containers, err := d.containers(ctx, app)
	if err != nil {
		return Status{}, err
	}
	counts := make(map[string]int)
	ready, active := true, false
	paused := app.Spec.SuspendMode == "pause"
	for _, container := range containers {
		requirement := expected[container.Service]
		if requirement.count == 0 {
			continue
		}
		completed := requirement.completed && container.State == "exited" && container.ExitCode != nil && *container.ExitCode == 0
		if !completed {
			switch container.State {
			case "running", "paused", "restarting":
				active = true
			}
			if container.State != "running" || (container.Health != "" && container.Health != "healthy") {
				ready = false
			}
			if container.State != "paused" {
				paused = false
			}
		}
		counts[container.Service]++
	}
	for service, requirement := range expected {
		if counts[service] < requirement.count {
			ready, paused = false, false
		}
	}
	if !ready {
		if active && !paused {
			return Status{Partial: true}, nil
		}
		return Status{Suspended: true}, nil
	}
	upstream, err := d.port(ctx, app)
	if err != nil {
		return Status{Partial: true}, err
	}
	return Status{Running: true, Upstream: upstream}, nil
}

func (d *Docker) Logs(ctx context.Context, app model.App, tail int) (string, error) {
	if tail < 1 || tail > maxLogTail {
		return "", fmt.Errorf("log tail must be between 1 and %d", maxLogTail)
	}
	result, err := d.compose(ctx, app, "logs", "--no-color", "--tail", strconv.Itoa(tail))
	if err != nil {
		return "", err
	}
	output := append(result.stdout, result.stderr...)
	truncated := result.stdoutTruncated || result.stderrTruncated || len(output) > maxCommandOutput
	if len(output) > maxCommandOutput {
		output = output[:maxCommandOutput]
	}
	if truncated {
		output = append(output, []byte("\n[output truncated at 1048576 bytes]\n")...)
	}
	return string(output), nil
}

func (d *Docker) compose(ctx context.Context, app model.App, args ...string) (commandResult, error) {
	if err := ctx.Err(); err != nil {
		return commandResult{}, err
	}
	base := []string{
		"--host", d.host, "compose", "--ansi", "never",
		"--project-name", app.Project, "--project-directory", app.Directory,
		"-f", app.ComposeFile, "--env-file", os.DevNull,
	}
	result, err := d.run(ctx, command{
		binary: d.binary, args: append(base, args...),
		directory: app.Directory, env: dockerEnvironment(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return commandResult{}, fmt.Errorf("docker compose %s for application %q: %w", args[0], app.Name, ctx.Err())
		}
		detail := sanitizeDiagnostic(result.stderr)
		if detail != "" {
			detail = ": " + detail
		}
		return commandResult{}, fmt.Errorf("docker compose %s for application %q failed: %w%s", args[0], app.Name, err, detail)
	}
	return result, nil
}

// Command runs a plain docker CLI command, such as image or container
// management, with the same controlled environment and Docker host as Compose
// operations. It returns stdout; failures include a sanitized stderr excerpt.
func (d *Docker) Command(ctx context.Context, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := d.run(ctx, command{
		binary: d.binary, args: append([]string{"--host", d.host}, args...),
		env: dockerEnvironment(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("docker %s: %w", args[0], ctx.Err())
		}
		detail := sanitizeDiagnostic(result.stderr)
		if detail != "" {
			detail = ": " + detail
		}
		return nil, fmt.Errorf("docker %s failed: %w%s", args[0], err, detail)
	}
	if result.stdoutTruncated {
		return nil, fmt.Errorf("docker %s output exceeds the %d byte limit", args[0], maxCommandOutput)
	}
	return result.stdout, nil
}

// Structural interpolation is rejected by manifest validation. Do not give Compose
// ambient server secrets or host COMPOSE_* / DOCKER_* control settings. HOME and
// PATH are retained so Docker can use the operator's registry credential helpers.
func dockerEnvironment() []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{"PATH=" + path, "LANG=C", "LC_ALL=C"}
	if home, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+home)
	}
	return env
}

func runCommand(ctx context.Context, invocation command) (commandResult, error) {
	cmd := exec.CommandContext(ctx, invocation.binary, invocation.args...)
	cmd.Dir = invocation.directory
	cmd.Env = invocation.env
	// Docker launches Compose as a child process. Cancel the whole process group
	// so a timed-out startup cannot keep creating containers in the background.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// Closing inherited output pipes also bounds cancellation when a CLI plugin
	// leaves a child with stdout/stderr open after Docker exits.
	cmd.WaitDelay = time.Second
	stdout := &cappedBuffer{limit: maxCommandOutput}
	stderr := &cappedBuffer{limit: maxCommandOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	return commandResult{
		stdout: stdout.bytes, stderr: stderr.bytes,
		stdoutTruncated: stdout.truncated, stderrTruncated: stderr.truncated,
	}, err
}

// Write acknowledges discarded bytes, avoiding both unbounded memory and a
// broken pipe that could interrupt an otherwise successful Docker operation.
type cappedBuffer struct {
	bytes     []byte
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	size := len(p)
	remaining := b.limit - len(b.bytes)
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.bytes = append(b.bytes, p...)
	return size, nil
}

type composeContainer struct {
	Service  string
	State    string
	Health   string
	ExitCode *int
}

func (d *Docker) containers(ctx context.Context, app model.App) ([]composeContainer, error) {
	result, err := d.compose(ctx, app, "ps", "--all", "--format", "json", "--orphans=false")
	if err != nil {
		return nil, err
	}
	if result.stdoutTruncated {
		return nil, errors.New("docker compose container listing exceeds output limit")
	}
	containers, err := parseContainers(result.stdout)
	if err != nil {
		return nil, fmt.Errorf("decode docker compose container listing: %w", err)
	}
	return containers, nil
}

func parseContainers(output []byte) ([]composeContainer, error) {
	output = bytes.TrimSpace(output)
	if len(output) == 0 {
		return nil, nil
	}
	// Older Compose versions emit a JSON array; current releases emit JSON Lines.
	if output[0] == '[' {
		var containers []composeContainer
		if err := json.Unmarshal(output, &containers); err != nil {
			return nil, err
		}
		return containers, nil
	}
	var containers []composeContainer
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var container composeContainer
		if err := decoder.Decode(&container); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		containers = append(containers, container)
	}
	return containers, nil
}

type serviceRequirement struct {
	count     int
	completed bool
}

func expectedServices(app model.App) (map[string]serviceRequirement, error) {
	file, err := os.Open(app.ComposeFile)
	if err != nil {
		return nil, fmt.Errorf("read generated compose configuration: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxComposeConfig+1))
	if err != nil {
		return nil, fmt.Errorf("read generated compose configuration: %w", err)
	}
	if len(data) > maxComposeConfig {
		return nil, errors.New("generated compose configuration exceeds size limit")
	}
	var config struct {
		Services map[string]struct {
			Profiles  []string        `json:"profiles"`
			Scale     *int            `json:"scale"`
			DependsOn json.RawMessage `json:"depends_on"`
			Deploy    struct {
				Replicas *int `json:"replicas"`
			} `json:"deploy"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("decode generated compose configuration: %w", err)
	}
	expected := make(map[string]serviceRequirement)
	for name, service := range config.Services {
		// No profiles are enabled by this runtime; ambient COMPOSE_PROFILES is
		// never inherited. The routed service must be in the default profile.
		if len(service.Profiles) > 0 {
			continue
		}
		count := 1
		if service.Deploy.Replicas != nil {
			count = *service.Deploy.Replicas
		}
		if service.Scale != nil {
			count = *service.Scale
		}
		if count < 0 {
			return nil, fmt.Errorf("invalid replica count for compose service %q", name)
		}
		if count > 0 {
			expected[name] = serviceRequirement{count: count}
		}
	}
	for _, service := range config.Services {
		var dependencies map[string]struct {
			Condition string `json:"condition"`
		}
		if json.Unmarshal(service.DependsOn, &dependencies) != nil {
			continue
		}
		for name, dependency := range dependencies {
			if dependency.Condition == "service_completed_successfully" && name != app.Spec.Service {
				requirement := expected[name]
				requirement.completed = true
				expected[name] = requirement
			}
		}
	}
	if expected[app.Spec.Service].count == 0 {
		return nil, fmt.Errorf("routed compose service %q is missing or disabled", app.Spec.Service)
	}
	return expected, nil
}

func (d *Docker) port(ctx context.Context, app model.App) (string, error) {
	if app.Spec.Port < 1 || app.Spec.Port > 65535 {
		return "", errors.New("invalid application port")
	}
	result, err := d.compose(ctx, app, "port", "--protocol", "tcp", "--", app.Spec.Service, strconv.Itoa(app.Spec.Port))
	if err != nil {
		return "", err
	}
	if result.stdoutTruncated {
		return "", errors.New("docker compose port output exceeds output limit")
	}
	upstream, err := loopbackAddress(result.stdout)
	if err != nil {
		return "", fmt.Errorf("unsafe or missing port binding for application %q: %w", app.Name, err)
	}
	return upstream, nil
}

func loopbackAddress(output []byte) (string, error) {
	addresses := strings.Fields(string(output))
	if len(addresses) != 1 {
		return "", errors.New("expected exactly one published TCP port")
	}
	host, port, err := net.SplitHostPort(addresses[0])
	if err != nil || host != "127.0.0.1" {
		return "", errors.New("published port must bind to 127.0.0.1")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", errors.New("published port is invalid")
	}
	return net.JoinHostPort(host, strconv.Itoa(number)), nil
}

func waitTCP(ctx context.Context, address string) error {
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	for {
		connection, err := dialer.DialContext(ctx, "tcp", address)
		if err == nil {
			connection.Close()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Docker diagnostics can contain terminal escape sequences. Keep errors short
// and printable; do not repeat the command, environment, or container logs.
func sanitizeDiagnostic(data []byte) string {
	if len(data) > 2048 {
		data = data[:2048]
	}
	var output strings.Builder
	for i := 0; i < len(data); i++ {
		if data[i] == 0x1b {
			if i+1 < len(data) && data[i+1] == '[' {
				i += 2
				for i < len(data) && (data[i] < 0x40 || data[i] > 0x7e) {
					i++
				}
			} else if i+1 < len(data) && data[i+1] == ']' {
				i += 2
				for i < len(data) && data[i] != 0x07 {
					if data[i] == 0x1b && i+1 < len(data) && data[i+1] == '\\' {
						i++
						break
					}
					i++
				}
			} else {
				i++
			}
			continue
		}
		output.WriteByte(data[i])
	}
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(output.String(), "?"))
	return strings.Join(strings.Fields(clean), " ")
}
