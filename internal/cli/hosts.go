package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"site-deploy/internal/auth"
)

// Host is a named server in the CLI's configuration file. Commands reach its
// daemon through an SSH tunnel the CLI opens for each command, or directly at
// Server (for example on the server itself).
type Host struct {
	SSH          string `json:"ssh,omitempty"`
	RSH          string `json:"rsh,omitempty"` // remote shell, like rsync's -e; default ssh
	Server       string `json:"server,omitempty"`
	TokenFile    string `json:"token_file"`
	APIPort      int    `json:"api_port,omitempty"`
	RegistryPort int    `json:"registry_port,omitempty"`
}

type hostConfig struct {
	Current string          `json:"current,omitempty"`
	Hosts   map[string]Host `json:"hosts"`
}

const (
	defaultAPIPort      = 9081
	defaultRegistryPort = 9082
	serverDataDir       = "/var/lib/site-deploy"
)

var hostName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// configPath is SITE_DEPLOY_CONFIG, or hosts.json in the user's config
// directory (~/.config/site-deploy on Linux).
func configPath() (string, error) {
	if path := os.Getenv("SITE_DEPLOY_CONFIG"); path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the configuration directory: %w", err)
	}
	return filepath.Join(dir, "site-deploy", "hosts.json"), nil
}

// loadConfig returns an empty configuration when the file doesn't exist.
func loadConfig() (hostConfig, string, error) {
	config := hostConfig{Hosts: map[string]Host{}}
	path, err := configPath()
	if err != nil {
		return config, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, path, nil
	}
	if err != nil {
		return config, path, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if config.Hosts == nil {
		config.Hosts = map[string]Host{}
	}
	return config, path, nil
}

func saveConfig(path string, config hostConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hosts-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// connection says how one command reaches the daemon.
type connection struct {
	name   string     // configured host, if one was used
	server string     // direct control API URL; empty when ssh is set
	ssh    *sshTarget // tunnelled target
}

// resolve picks the daemon for a command: an explicit --server or
// SITE_DEPLOY_SERVER, else the host named by --host or SITE_DEPLOY_HOST, else
// an SSH target in SITE_DEPLOY_SSH (as site-deploy-remote and the GitHub
// Action set), else the current host, else the local default server. A host's
// token file applies unless --token or --token-file is given. The remote shell
// is -e/--rsh, else the host's, else SITE_DEPLOY_RSH, else ssh.
func resolve(opts *options) (connection, error) {
	conn, err := resolveTarget(opts)
	if err != nil || conn.ssh == nil {
		return conn, err
	}
	rsh := conn.ssh.rsh
	if opts.rshSet {
		rsh = opts.rsh
	} else if rsh == "" {
		rsh = os.Getenv("SITE_DEPLOY_RSH")
	}
	return conn, conn.ssh.withRSH(rsh)
}

func resolveTarget(opts *options) (connection, error) {
	if opts.serverSet {
		return connection{server: opts.server}, nil
	}
	if opts.host == "" {
		if destination := os.Getenv("SITE_DEPLOY_SSH"); destination != "" {
			return environmentTarget(destination)
		}
	}
	config, path, err := loadConfig()
	if err != nil {
		return connection{}, err
	}
	name := opts.host
	if name == "" {
		name = config.Current
	}
	if name == "" {
		return connection{server: opts.server}, nil
	}
	host, ok := config.Hosts[name]
	if !ok {
		return connection{}, fmt.Errorf("unknown host %q in %s; see site-deploy host list", name, path)
	}
	if host.TokenFile != "" && !opts.tokenSet && !opts.tokenFileSet {
		opts.tokenFile, opts.tokenFileSet = host.TokenFile, true
	}
	if host.Server != "" {
		return connection{name: name, server: host.Server}, nil
	}
	return connection{name: name, ssh: host.target()}, nil
}

func (h Host) target() *sshTarget {
	t := &sshTarget{destination: h.SSH, rsh: h.RSH, apiPort: h.APIPort, registryPort: h.RegistryPort}
	if t.apiPort == 0 {
		t.apiPort = defaultAPIPort
	}
	if t.registryPort == 0 {
		t.registryPort = defaultRegistryPort
	}
	return t
}

func environmentTarget(destination string) (connection, error) {
	t := &sshTarget{destination: destination, apiPort: defaultAPIPort, registryPort: defaultRegistryPort}
	if value := os.Getenv("SITE_DEPLOY_REGISTRY_PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return connection{}, errors.New("SITE_DEPLOY_REGISTRY_PORT must be a port number")
		}
		t.registryPort = port
	}
	return connection{ssh: t}, nil
}

const hostUsage = `Usage: site-deploy host [list]
       site-deploy host add NAME SSH_DESTINATION [-e|--rsh COMMAND] [--token-file FILE]
                                [--api-port N] [--registry-port N] [--use]
       site-deploy host add NAME --server URL [--token-file FILE] [--use]
       site-deploy host use NAME
       site-deploy host remove NAME
       site-deploy host show [NAME]
       site-deploy host fetch-token [NAME]

Commands run against the current host, or the one --host NAME or SITE_DEPLOY_HOST
names, through an SSH tunnel opened for that command. SSH_DESTINATION is anything
ssh accepts, such as deploy@server or an ssh_config alias. -e/--rsh sets the remote
shell for the host, like rsync's -e (for example -e "ssh -p 2222"); it must accept
ssh's arguments. A command's own -e overrides it; SITE_DEPLOY_RSH is the default.
The token file defaults to tokens/NAME next to the configuration file;
fetch-token copies the daemon's token into it with sudo on the server.
The first host added becomes current.
`

func runHost(opts options, stdout, stderr io.Writer) error {
	args := opts.hostArgs
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	flags := map[string]string{}
	var positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") && arg != "-e" && !strings.HasPrefix(arg, "-e=") {
			positionals = append(positionals, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name == "--rsh" {
			name = "-e"
		}
		switch name {
		case "--use", "--json", "--help":
			if hasValue {
				return fmt.Errorf("%s takes no value", name)
			}
			flags[name] = "true"
		case "-e", "--token-file", "--api-port", "--registry-port", "--server":
			if !hasValue {
				i++
				if i == len(args) {
					return fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			flags[name] = value
		default:
			return fmt.Errorf("unknown host option %q", name)
		}
	}
	if flags["--help"] != "" || sub == "help" {
		_, err := io.WriteString(stdout, hostUsage)
		return err
	}
	allowed := map[string]string{"add": "-e --token-file --api-port --registry-port --server --use", "list": "--json", "show": "--json"}
	for name := range flags {
		if !strings.Contains(" "+allowed[sub]+" ", " "+name+" ") {
			return fmt.Errorf("host %s does not take %s", sub, name)
		}
	}
	config, path, err := loadConfig()
	if err != nil {
		return err
	}
	want := map[string][2]int{"list": {0, 0}, "add": {1, 2}, "use": {1, 1}, "remove": {1, 1}, "show": {0, 1}, "fetch-token": {0, 1}}
	limits, known := want[sub]
	if !known {
		return fmt.Errorf("unknown host command %q; see site-deploy host help", sub)
	}
	if len(positionals) < limits[0] || len(positionals) > limits[1] {
		return fmt.Errorf("wrong number of arguments for host %s; see site-deploy host help", sub)
	}
	// show and fetch-token default to the selected host.
	name := opts.host
	if name == "" {
		name = config.Current
	}
	if len(positionals) > 0 {
		name = positionals[0]
	}
	if sub != "list" && !hostName.MatchString(name) {
		return fmt.Errorf("host name %q must be 1-48 lowercase letters, digits or hyphens", name)
	}
	host, exists := config.Hosts[name]
	if !exists && sub != "add" && sub != "list" {
		return fmt.Errorf("unknown host %q; see site-deploy host list", name)
	}
	switch sub {
	case "list":
		return listHosts(stdout, config, flags["--json"] != "")
	case "add":
		if exists {
			return fmt.Errorf("host %q already exists; remove it first", name)
		}
		host, err = newHost(name, path, positionals[1:], flags)
		if err != nil {
			return err
		}
		config.Hosts[name] = host
		if config.Current == "" || flags["--use"] != "" {
			config.Current = name
		}
	case "use":
		config.Current = name
	case "remove":
		delete(config.Hosts, name)
		if config.Current == name {
			config.Current = ""
		}
	case "show":
		data, err := json.MarshalIndent(map[string]any{"name": name, "current": name == config.Current, "host": host}, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(data))
		return err
	case "fetch-token":
		if opts.rshSet {
			host.RSH = opts.rsh
		}
		return fetchToken(name, host, stdout)
	}
	if err := saveConfig(path, config); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	switch sub {
	case "add":
		fmt.Fprintf(stdout, "Added host %s (%s)\n", name, describeHost(host))
	case "remove":
		fmt.Fprintf(stdout, "Removed host %s\n", name)
	}
	if config.Current == name && sub != "remove" {
		fmt.Fprintf(stdout, "Current host: %s\n", name)
	}
	return nil
}

func newHost(name, configFile string, destination []string, flags map[string]string) (Host, error) {
	var host Host
	switch {
	case flags["--server"] != "" && len(destination) == 0:
		if _, err := serverURL(flags["--server"]); err != nil {
			return host, err
		}
		if flags["-e"] != "" || flags["--api-port"] != "" || flags["--registry-port"] != "" {
			return host, errors.New("--server hosts take no SSH options")
		}
		host.Server = flags["--server"]
	case flags["--server"] == "" && len(destination) == 1:
		if destination[0] == "" || strings.HasPrefix(destination[0], "-") {
			return host, errors.New("SSH_DESTINATION must not be empty or start with '-'")
		}
		host.SSH = destination[0]
		host.RSH = strings.TrimSpace(flags["-e"])
		if err := (&sshTarget{}).withRSH(host.RSH); err != nil {
			return host, err
		}
	default:
		return host, errors.New("host add needs either SSH_DESTINATION or --server URL")
	}
	for flag, port := range map[string]*int{"--api-port": &host.APIPort, "--registry-port": &host.RegistryPort} {
		if value := flags[flag]; value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 65535 {
				return host, fmt.Errorf("%s must be a port number", flag)
			}
			*port = n
		}
	}
	host.TokenFile = flags["--token-file"]
	if host.TokenFile == "" {
		host.TokenFile = filepath.Join(filepath.Dir(configFile), "tokens", name)
	}
	tokenFile, err := filepath.Abs(host.TokenFile)
	if err != nil {
		return host, err
	}
	host.TokenFile = tokenFile
	return host, nil
}

func describeHost(host Host) string {
	if host.Server != "" {
		return host.Server
	}
	return host.target().describe()
}

// command returns the host's remote shell, split, falling back like resolve.
func (h Host) command() (*sshTarget, error) {
	t := h.target()
	rsh := t.rsh
	if rsh == "" {
		rsh = os.Getenv("SITE_DEPLOY_RSH")
	}
	return t, t.withRSH(rsh)
}

func listHosts(stdout io.Writer, config hostConfig, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(stdout).Encode(config)
	}
	if len(config.Hosts) == 0 {
		_, err := fmt.Fprintln(stdout, "No hosts configured; add one with: site-deploy host add NAME SSH_DESTINATION")
		return err
	}
	names := make([]string, 0, len(config.Hosts))
	for name := range config.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	table := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "CURRENT\tNAME\tTARGET")
	for _, name := range names {
		marker := ""
		if name == config.Current {
			marker = "*"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\n", marker, name, describeHost(config.Hosts[name]))
	}
	return table.Flush()
}

// fetchToken copies the daemon's token from a standard install over SSH.
func fetchToken(name string, host Host, stdout io.Writer) error {
	if host.SSH == "" {
		return fmt.Errorf("host %s is not reached over SSH", name)
	}
	target, err := host.command()
	if err != nil {
		return err
	}
	program, args := target.args(nil, "sudo", "cat", serverDataDir+"/token")
	cmd := exec.Command(program, args...)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("read %s/token on %s: %w", serverDataDir, name, err)
	}
	token := strings.TrimSpace(string(output))
	if err := auth.ValidateToken(token); err != nil {
		return fmt.Errorf("token from %s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(host.TokenFile), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(host.TokenFile), ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), host.TokenFile); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Saved the token of %s to %s\n", name, host.TokenFile)
	return err
}

// runSSH runs the selected host's ssh with the remaining arguments as the
// remote command (none for a shell), and returns its exit status.
func runSSH(opts options, stdout, stderr io.Writer) (int, error) {
	conn, err := resolve(&opts)
	if err != nil {
		return 1, err
	}
	if conn.ssh == nil {
		return 1, errors.New("the selected daemon is not reached over SSH; choose a host with --host NAME")
	}
	program, args := conn.ssh.args(nil, opts.sshArgs...)
	cmd := exec.Command(program, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 1, fmt.Errorf("run %s: %w", program, err)
	}
	return 0, nil
}
