// Package cli implements the site-deploy command-line client.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"site-deploy/internal/auth"
	"site-deploy/internal/images"
	"site-deploy/internal/model"
	"site-deploy/internal/store"
	"site-deploy/internal/version"
)

// Serve is set by the executable to avoid coupling the client to the daemon.
var Serve func(args []string, stdout, stderr io.Writer) error

const (
	maxResponseBytes = 16 << 20
	maxUploadBytes   = 100 << 20
	maxTokenBytes    = auth.MaxTokenBytes
)

type options struct {
	command, server, token, tokenFile, gateway string
	registryVia, host, hostLabel, rsh          string
	positionals, serveArgs, hostArgs, sshArgs  []string
	tokenSet, tokenFileSet, json, httpOnly     bool
	tailSet, gatewaySet, httpSet, help         bool
	registryViaSet, serverSet, rshSet          bool
	timeout                                    time.Duration
	tail                                       int
}

const usage = `Usage: site-deploy [options] COMMAND

Commands:
  host [list|add|use|remove|show|fetch-token]
                                 Manage the servers this CLI deploys to (host help for details)
  ssh [COMMAND...]               Run ssh to the selected host, with an optional remote command
  serve [options]                 Run the deployment daemon (serve --help for options)
  deploy NAME ZIP                 Upload a ZIP containing a Compose application
  list                           List applications
  inspect NAME                   Show application details as JSON
  suspend NAME                   Stop an application, preserving its volumes
  resume NAME                    Start a suspended application
  delete NAME                    Remove an application, preserving named volumes
  logs NAME [--tail N]            Show recent logs (default 100, maximum 10000)
  caddy-config [--http] [--gateway ADDR]
                                 Print complete dedicated Caddy configuration
  push IMAGE NAME:TAG [--registry-via ADDR]
                                 Push a local Docker image to the daemon's private registry
  images                         List images in the private registry and the apps using them
  cleanup                        Remove unused registry tags and Docker images now
  version                        Print the CLI version
  daemon-version                 Print CLI and daemon versions; fails if the CLI is older
  help                           Show this help

Client options (before or after the command or its arguments; before host and ssh):
  --host NAME       Configured host to use instead of the current one (SITE_DEPLOY_HOST)
  -e, --rsh COMMAND Remote shell for the SSH tunnel, like rsync's -e (default: the
                    host's, else SITE_DEPLOY_RSH, else ssh)
  --server URL      Control API, bypassing hosts (SITE_DEPLOY_SERVER; default http://127.0.0.1:9081)
  --token TOKEN     Bearer token (prefer SITE_DEPLOY_TOKEN or a token file)
  --token-file FILE Token file (SITE_DEPLOY_TOKEN_FILE; default .site-deploy/token)
  --json            Print machine-readable JSON
  --timeout DURATION
                    Total request timeout including upload and startup (default 10m)
  --help, -h        Show help

Commands reach a configured host through an SSH tunnel to its loopback control
API that lasts as long as the command; push also forwards the registry. Without
hosts, commands use --server, which over HTTP must be a literal loopback IP.
push runs the local docker CLI, which must reach the registry address the daemon
reports: run it on the server, or through a host, which forwards it.
--registry-via (SITE_DEPLOY_REGISTRY_VIA) names a loopback forward of that registry
to push through instead; the printed reference still uses the daemon's address.
A release daemon rejects CLIs older than itself: upgrade the CLI before the daemon.
Explicit --token takes precedence over --token-file; an explicit token file takes
precedence over SITE_DEPLOY_TOKEN. Daemon options are passed directly to serve.
`

// Run executes a command and returns its process exit code: 0 success, 1 runtime
// failure, or 2 invalid command usage. All output goes to the supplied writers.
func Run(args []string, stdout, stderr io.Writer) int {
	opts, err := parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "site-deploy: %s\nRun 'site-deploy help' for usage.\n", diagnostic(err.Error(), opts.token))
		return 2
	}
	if opts.command == "serve" {
		if Serve == nil {
			err = errors.New("daemon support is unavailable in this executable")
		} else {
			err = Serve(opts.serveArgs, stdout, stderr)
		}
	} else if opts.help || opts.command == "help" || opts.command == "" {
		_, err = io.WriteString(stdout, usage)
	} else if opts.command == "host" {
		err = runHost(opts, stdout, stderr)
	} else if opts.command == "ssh" {
		var code int
		if code, err = runSSH(opts, stdout, stderr); err == nil {
			return code
		}
	} else if opts.command == "version" {
		if opts.json {
			err = json.NewEncoder(stdout).Encode(map[string]string{"version": version.Version})
		} else {
			_, err = fmt.Fprintln(stdout, "site-deploy "+version.Version)
		}
	} else {
		err = runClient(&opts, stdout, stderr)
	}
	if err != nil {
		fmt.Fprintf(stderr, "site-deploy: %s\n", diagnostic(err.Error(), opts.token))
		return 1
	}
	return 0
}

func parse(args []string) (options, error) {
	opts := options{
		server: os.Getenv("SITE_DEPLOY_SERVER"), token: os.Getenv("SITE_DEPLOY_TOKEN"),
		tokenFile: os.Getenv("SITE_DEPLOY_TOKEN_FILE"), registryVia: os.Getenv("SITE_DEPLOY_REGISTRY_VIA"),
		host: os.Getenv("SITE_DEPLOY_HOST"), timeout: 10 * time.Minute, tail: 100, gateway: "127.0.0.1:9080",
	}
	opts.serverSet = opts.server != ""
	if opts.server == "" {
		opts.server = "http://127.0.0.1:9081"
	}
	if opts.tokenFile == "" {
		opts.tokenFile = ".site-deploy/token"
	}
	literal := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !literal && arg == "--" {
			literal = true
			continue
		}
		if literal || !strings.HasPrefix(arg, "-") || arg == "-" {
			if opts.command != "" {
				opts.positionals = append(opts.positionals, arg)
				continue
			}
			opts.command = arg
			if arg == "serve" {
				// In particular, --token-file and --gateway belong to the daemon
				// here. Preserve every argument, including those before serve.
				opts.serveArgs = append(append([]string{}, args[:i]...), args[i+1:]...)
				return opts, nil
			}
			// Everything after these belongs to them: host options, or the
			// remote command for ssh.
			if arg == "host" || arg == "ssh" {
				rest := args[i+1:]
				if arg == "ssh" && len(rest) > 0 && rest[0] == "--" {
					rest = rest[1:]
				}
				opts.hostArgs, opts.sshArgs = rest, rest
				return opts, opts.validHost()
			}
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name == "-e" {
			name = "--rsh"
		}
		switch name {
		case "--json", "--http", "--help", "-h", "--version":
			enabled := true
			if hasValue {
				var err error
				enabled, err = strconv.ParseBool(value)
				if err != nil {
					return opts, fmt.Errorf("%s expects true or false", name)
				}
			}
			switch name {
			case "--json":
				opts.json = enabled
			case "--http":
				opts.httpOnly, opts.httpSet = enabled, true
			case "--version":
				if enabled {
					opts.command = "version"
				}
			default:
				opts.help = enabled
			}
		case "--server", "--token", "--token-file", "--timeout", "--tail", "--gateway", "--registry-via", "--host", "--rsh":
			if !hasValue {
				i++
				if i == len(args) {
					return opts, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			switch name {
			case "--server":
				opts.server, opts.serverSet = value, true
			case "--host":
				opts.host = value
			case "--rsh":
				opts.rsh, opts.rshSet = value, true
			case "--token":
				opts.token, opts.tokenSet = value, true
			case "--token-file":
				opts.tokenFile, opts.tokenFileSet = value, true
			case "--gateway":
				opts.gateway, opts.gatewaySet = value, true
			case "--registry-via":
				opts.registryVia, opts.registryViaSet = value, true
			case "--tail":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > 10000 {
					return opts, errors.New("--tail must be between 1 and 10000")
				}
				opts.tail, opts.tailSet = n, true
			case "--timeout":
				d, err := time.ParseDuration(value)
				if err != nil || d <= 0 {
					return opts, errors.New("--timeout must be a positive duration, such as 10m")
				}
				opts.timeout = d
			}
		default:
			return opts, fmt.Errorf("unknown option %q", name)
		}
	}
	if opts.help || opts.command == "help" || opts.command == "" {
		return opts, nil
	}
	if err := opts.validHost(); err != nil {
		return opts, err
	}
	want := 0
	switch opts.command {
	case "deploy", "push":
		want = 2
	case "inspect", "suspend", "resume", "delete", "logs":
		want = 1
	case "list", "caddy-config", "version", "daemon-version", "images", "cleanup":
	default:
		return opts, fmt.Errorf("unknown command %q", opts.command)
	}
	if len(opts.positionals) != want {
		return opts, fmt.Errorf("%s expects %d positional argument(s)", opts.command, want)
	}
	if (opts.tailSet && opts.command != "logs") || ((opts.gatewaySet || opts.httpSet) && opts.command != "caddy-config") {
		return opts, errors.New("--tail is only valid for logs; --http and --gateway are only valid for caddy-config")
	}
	if opts.registryViaSet && opts.command != "push" {
		return opts, errors.New("--registry-via is only valid for push")
	}
	if opts.command == "push" {
		repository, tag, found := strings.Cut(opts.positionals[1], ":")
		if opts.positionals[0] == "" || !found || !images.ValidRepository(repository) || !images.ValidTag(tag) {
			return opts, errors.New("push expects a local image and NAME:TAG, such as app-base:2026-10-13 (lowercase name; explicit tag)")
		}
		if opts.registryVia != "" && !loopbackHostPort(opts.registryVia) {
			return opts, errors.New("--registry-via must be a literal loopback IP and port, such as 127.0.0.1:41234")
		}
		return opts, nil
	}
	if want > 0 && !store.ValidName(opts.positionals[0]) {
		return opts, errors.New("application name must start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens")
	}
	return opts, nil
}

func (opts options) validHost() error {
	if opts.host != "" && !hostName.MatchString(opts.host) {
		return fmt.Errorf("host name %q must be 1-48 lowercase letters, digits or hyphens", opts.host)
	}
	return nil
}

func runClient(opts *options, stdout, stderr io.Writer) error {
	conn, err := resolve(opts)
	if err != nil {
		return err
	}
	if conn.server != "" {
		opts.server = conn.server
		if conn.name != "" {
			opts.hostLabel = conn.name + " (" + conn.server + ")"
		}
	}
	if _, err := serverURL(opts.server); err != nil && conn.ssh == nil {
		return err
	}
	opts.token, err = loadToken(*opts)
	if err != nil {
		return err
	}
	if conn.ssh != nil {
		opts.hostLabel = conn.ssh.describe()
		if conn.name != "" {
			opts.hostLabel = conn.name + " (" + opts.hostLabel + ")"
		}
		// Say where changes go, since the current host can be switched.
		switch opts.command {
		case "deploy", "delete", "suspend", "resume", "push", "cleanup":
			fmt.Fprintf(stderr, "Host: %s\n", opts.hostLabel)
		}
		t, err := openTunnel(*conn.ssh, opts.command == "push")
		if err != nil {
			return err
		}
		defer t.Close()
		opts.server = t.server
		if opts.command == "push" && opts.registryVia == "" {
			opts.registryVia = t.registry
		}
	}
	endpoint, err := serverURL(opts.server)
	if err != nil {
		return err
	}
	method, path := http.MethodGet, "/v1/apps"
	query := url.Values{}
	var body io.Reader
	var size int64
	if len(opts.positionals) > 0 && opts.command != "push" {
		path += "/" + url.PathEscape(opts.positionals[0])
	}
	switch opts.command {
	case "deploy":
		filePath := opts.positionals[1]
		info, err := os.Stat(filePath)
		if err != nil {
			return fmt.Errorf("open ZIP: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxUploadBytes {
			return errors.New("ZIP must be a nonempty regular file no larger than 100 MiB")
		}
		file, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("open ZIP: %w", err)
		}
		defer file.Close()
		body, size, method = io.NewSectionReader(file, 0, info.Size()), info.Size(), http.MethodPost
	case "suspend", "resume":
		path, method = path+"/"+opts.command, http.MethodPost
	case "delete":
		method = http.MethodDelete
	case "logs":
		path += "/logs"
		query.Set("tail", strconv.Itoa(opts.tail))
	case "caddy-config":
		path = "/v1/caddy"
		query.Set("http", strconv.FormatBool(opts.httpOnly))
		query.Set("gateway", opts.gateway)
	case "daemon-version":
		path = "/v1/version"
	case "images", "push":
		path = "/v1/registry"
	case "cleanup":
		path, method = "/v1/cleanup", http.MethodPost
	}
	endpoint.Path, endpoint.RawQuery = path, query.Encode()
	request, err := http.NewRequest(method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("create API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+opts.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set(version.ClientHeader, version.Version)
	if opts.command == "deploy" {
		request.ContentLength = size
		request.Header.Set("Content-Type", "application/zip")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: opts.timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("control API request: %w", err)
	}
	defer response.Body.Close()
	if warning := response.Header.Get("X-Site-Deploy-Warning"); warning != "" {
		fmt.Fprintf(stderr, "Warning: %s\n", diagnostic(warning, opts.token))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read control API response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return errors.New("control API response exceeds the 16 MiB limit")
	}
	data = bytes.ReplaceAll(data, []byte(opts.token), []byte("[redacted]"))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var result struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &result)
		message := fmt.Sprintf("control API returned HTTP %d (%s)", response.StatusCode, http.StatusText(response.StatusCode))
		if result.Error != "" {
			message += ": " + diagnostic(result.Error, opts.token)
		}
		return errors.New(message)
	}
	if !json.Valid(data) {
		return errors.New("control API returned invalid JSON")
	}
	if opts.command == "push" {
		return pushImage(*opts, data, stdout, stderr)
	}
	return printResult(stdout, *opts, data)
}

// dockerCommand runs the local docker CLI; tests replace it.
var dockerCommand = func(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// pushImage tags a local image under the registry's address and pushes it with
// the local Docker, which must reach that address: on the server, or through a
// forward named by --registry-via, as site-deploy-remote sets up. Registries
// store images by repository name, not by the address used to push them, so the
// printed reference always uses the daemon's address. Only the temporary
// registry alias is removed afterwards. Docker's progress goes to stderr.
func pushImage(opts options, data []byte, stdout, stderr io.Writer) error {
	var registry struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(data, &registry); err != nil || !loopbackHostPort(registry.Address) {
		return errors.New("control API returned an invalid registry address")
	}
	via := registry.Address
	if opts.registryVia != "" {
		via = opts.registryVia
	}
	source, target := opts.positionals[0], via+"/"+opts.positionals[1]
	reference := registry.Address + "/" + opts.positionals[1]
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	if source != target {
		if err := dockerCommand(ctx, stderr, stderr, "image", "tag", source, target); err != nil {
			return fmt.Errorf("docker image tag %s: %w", source, err)
		}
		defer dockerCommand(context.Background(), io.Discard, io.Discard, "image", "rm", target)
	}
	if err := dockerCommand(ctx, stderr, stderr, "image", "push", target); err != nil {
		return fmt.Errorf("docker image push %s: %w; Docker must reach %s (use site-deploy-remote push, or run push on the server)", target, err, via)
	}
	if opts.json {
		return json.NewEncoder(stdout).Encode(map[string]string{"reference": reference})
	}
	_, err := fmt.Fprintf(stdout, "Pushed %s\nReference it in compose.yaml as:  image: %s\n", reference, reference)
	return err
}

func loopbackHostPort(address string) bool {
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback() && port != ""
}

func serverURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errors.New("--server must be an HTTP(S) URL without credentials, a path, query, or fragment")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("--server port must be between 1 and 65535")
		}
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("HTTP --server must use a literal loopback IP; use HTTPS or an SSH tunnel for remote control")
		}
	}
	return u, nil
}

func loadToken(opts options) (string, error) {
	token := opts.token
	if !opts.tokenSet && (opts.tokenFileSet || token == "") {
		info, err := os.Lstat(opts.tokenFile)
		if err != nil {
			if os.IsNotExist(err) {
				return "", errors.New("token file not found; set --token-file or SITE_DEPLOY_TOKEN (serve creates .site-deploy/token by default)")
			}
			return "", fmt.Errorf("read token file: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxTokenBytes+2 {
			return "", errors.New("token file must be a regular, owner-only file (chmod 600), at most 4098 bytes")
		}
		file, err := os.Open(opts.tokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxTokenBytes+3))
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if err := auth.ValidateToken(token); err != nil {
		return "", err
	}
	return token, nil
}

func printResult(stdout io.Writer, opts options, data []byte) error {
	if opts.command == "daemon-version" {
		return printVersions(stdout, opts, data)
	}
	if opts.json || opts.command == "inspect" || opts.command == "caddy-config" {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, data, "", "  "); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, strings.TrimSpace(pretty.String())); err != nil {
			return err
		}
		if opts.command == "cleanup" {
			var report images.Report
			if json.Unmarshal(data, &report) == nil && len(report.Errors) > 0 {
				return fmt.Errorf("cleanup failed: %s", diagnostic(strings.Join(report.Errors, "; "), ""))
			}
		}
		return nil
	}
	switch opts.command {
	case "list":
		var apps []model.App
		if err := json.Unmarshal(data, &apps); err != nil {
			return errors.New("control API returned an invalid application list")
		}
		sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
		table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "NAME\tDOMAIN\tSTATE\tIDLE TIMEOUT\tACCESS")
		for _, app := range apps {
			access := "public"
			if len(app.Spec.BasicAuth) > 0 {
				access = fmt.Sprintf("basic auth (%d)", len(app.Spec.BasicAuth))
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", diagnostic(app.Name, ""), diagnostic(app.Spec.Domain, ""), diagnostic(string(app.State), ""), diagnostic(app.Spec.IdleTimeout, ""), access)
		}
		return table.Flush()
	case "images":
		var result struct {
			Images []images.Image `json:"images"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return errors.New("control API returned an invalid image list")
		}
		table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "REFERENCE\tDIGEST\tPUSHED\tUSED BY")
		for _, image := range result.Images {
			pushed, digest, users := "-", diagnostic(image.Digest, ""), "-"
			if !image.Pushed.IsZero() {
				pushed = image.Pushed.Local().Format("2006-01-02 15:04")
			}
			if len(digest) > 19 {
				digest = digest[:19]
			}
			if len(image.UsedBy) > 0 {
				users = diagnostic(strings.Join(image.UsedBy, ","), "")
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", diagnostic(image.Reference, ""), digest, pushed, users)
		}
		return table.Flush()
	case "cleanup":
		var report images.Report
		if err := json.Unmarshal(data, &report); err != nil {
			return errors.New("control API returned an invalid cleanup report")
		}
		return printCleanup(stdout, report)
	case "logs":
		var result struct {
			Logs *string `json:"logs"`
		}
		if err := json.Unmarshal(data, &result); err != nil || result.Logs == nil {
			return errors.New("control API returned invalid logs")
		}
		_, err := io.WriteString(stdout, *result.Logs)
		return err
	case "delete":
		var result struct {
			Deleted string `json:"deleted"`
		}
		if err := json.Unmarshal(data, &result); err != nil || result.Deleted == "" {
			return errors.New("control API returned an invalid deletion result")
		}
		_, err := fmt.Fprintf(stdout, "Deleted %s\n", diagnostic(result.Deleted, ""))
		return err
	default:
		var app model.App
		if err := json.Unmarshal(data, &app); err != nil || app.Name == "" {
			return errors.New("control API returned an invalid application")
		}
		_, err := fmt.Fprintf(stdout, "%s: %s (%s)\n", diagnostic(app.Name, ""), diagnostic(string(app.State), ""), diagnostic(app.Spec.Domain, ""))
		return err
	}
}

func printCleanup(stdout io.Writer, report images.Report) error {
	if report.Skipped != "" {
		fmt.Fprintf(stdout, "Skipped: %s\n", diagnostic(report.Skipped, ""))
	}
	deleted := "nothing"
	if len(report.Deleted) > 0 {
		deleted = diagnostic(strings.Join(report.Deleted, ", "), "")
	}
	fmt.Fprintf(stdout, "Registry tags deleted: %s\nRegistry garbage collected: %v\n", deleted, report.GarbageCollected)
	fmt.Fprintf(stdout, "Docker images pruned: %d", len(report.Pruned))
	if report.Reclaimed != "" {
		fmt.Fprintf(stdout, " (reclaimed %s)", diagnostic(report.Reclaimed, ""))
	}
	fmt.Fprintln(stdout)
	if len(report.Errors) > 0 {
		return fmt.Errorf("cleanup failed: %s", diagnostic(strings.Join(report.Errors, "; "), ""))
	}
	return nil
}

// printVersions reports both versions, then fails if this CLI is too old to
// control the daemon. The upgrade procedure relies on that exit status.
func printVersions(stdout io.Writer, opts options, data []byte) error {
	var result struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Version == "" {
		return errors.New("control API returned an invalid version")
	}
	daemon := diagnostic(result.Version, "")
	problem := version.ClientSupported(version.Version, daemon)
	var err error
	if opts.json {
		err = json.NewEncoder(stdout).Encode(map[string]any{"cli": version.Version, "daemon": daemon, "compatible": problem == nil})
	} else {
		if opts.hostLabel != "" {
			fmt.Fprintf(stdout, "Host    %s\n", diagnostic(opts.hostLabel, ""))
		}
		_, err = fmt.Fprintf(stdout, "CLI     %s\nDaemon  %s\n", version.Version, daemon)
	}
	if err != nil {
		return err
	}
	return problem
}

func diagnostic(value, token string) string {
	if token != "" {
		value = strings.ReplaceAll(value, token, "[redacted]")
	}
	value = strings.Map(func(c rune) rune {
		if c < 32 || c == 127 {
			return ' '
		}
		return c
	}, value)
	if len(value) > 4096 {
		value = value[:4096] + " (truncated)"
	}
	return strings.TrimSpace(value)
}
