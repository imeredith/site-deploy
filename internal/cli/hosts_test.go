package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestMain keeps every test away from the developer's own hosts, and lets the
// test binary stand in for ssh: run with SITE_DEPLOY_FAKE_SSH=1, it forwards
// each -L like ssh -N does, or prints its destination and remote command.
func TestMain(m *testing.M) {
	if os.Getenv("SITE_DEPLOY_FAKE_SSH") == "1" {
		fakeSSH(os.Args[1:])
		return
	}
	dir, err := os.MkdirTemp("", "site-deploy-cli-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("SITE_DEPLOY_CONFIG", filepath.Join(dir, "hosts.json"))
	for _, name := range []string{"SITE_DEPLOY_HOST", "SITE_DEPLOY_SSH", "SITE_DEPLOY_RSH"} {
		os.Unsetenv(name)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func fakeSSH(args []string) {
	var forwards, rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-L":
			forwards = append(forwards, args[i+1])
			i++
		case "-o", "-F":
			i++
		case "--":
			rest, i = args[i+1:], len(args)
		}
	}
	if message := os.Getenv("SITE_DEPLOY_FAKE_SSH_FAIL"); message != "" {
		fmt.Fprintln(os.Stderr, message)
		os.Exit(255)
	}
	if len(forwards) == 0 {
		if output := os.Getenv("SITE_DEPLOY_FAKE_SSH_OUTPUT"); output != "" {
			fmt.Println(output)
		} else {
			fmt.Print(strings.Join(rest, "|"))
		}
		os.Exit(3)
	}
	for _, spec := range forwards {
		parts := strings.Split(spec, ":") // 127.0.0.1:LOCAL:127.0.0.1:REMOTE
		listener, err := net.Listen("tcp", parts[0]+":"+parts[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "bind: Address already in use")
			os.Exit(255)
		}
		go func(target string) {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					upstream, err := net.Dial("tcp", target)
					if err != nil {
						return
					}
					defer upstream.Close()
					go io.Copy(upstream, conn)
					io.Copy(conn, upstream)
				}()
			}
		}(parts[2] + ":" + parts[3])
	}
	select {}
}

func readConfig(t *testing.T) hostConfig {
	t.Helper()
	config, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestHostCommandsManageConfiguration(t *testing.T) {
	cliTestEnvironment(t)
	config := os.Getenv("SITE_DEPLOY_CONFIG")
	run := func(args ...string) string {
		t.Helper()
		code, stdout, stderr := cliTestRun(args...)
		if code != 0 {
			t.Fatalf("%q: exit %d: %s", args, code, stderr)
		}
		return stdout
	}
	if out := run("host"); !strings.Contains(out, "No hosts configured") {
		t.Fatalf("empty list: %q", out)
	}
	if out := run("host", "add", "home", "deploy@192.0.2.10", "--token-file", "/tokens/home"); !strings.Contains(out, "Current host: home") {
		t.Fatalf("first add: %q", out)
	}
	run("host", "add", "cloud", "ubuntu@203.0.113.10", "-e", "proxyssh -q", "--registry-port=9092")
	run("host", "add", "local", "--server", "http://127.0.0.1:9081")
	got := readConfig(t)
	cloud := got.Hosts["cloud"]
	if got.Current != "home" || len(got.Hosts) != 3 || cloud.SSH != "ubuntu@203.0.113.10" || cloud.RSH != "proxyssh -q" ||
		cloud.RegistryPort != 9092 || cloud.TokenFile != filepath.Join(filepath.Dir(config), "tokens", "cloud") || got.Hosts["local"].Server == "" {
		t.Fatalf("configuration: %+v", got)
	}
	info, err := os.Stat(config)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("configuration file mode: %v %v", info.Mode(), err)
	}
	out := run("host", "list")
	if !strings.Contains(out, "*        home   deploy@192.0.2.10") || !strings.Contains(out, "cloud  ubuntu@203.0.113.10 (-e proxyssh -q)") {
		t.Fatalf("list:\n%s", out)
	}
	run("host", "use", "cloud")
	var shown struct {
		Name    string `json:"name"`
		Current bool   `json:"current"`
		Host    Host   `json:"host"`
	}
	if err := json.Unmarshal([]byte(run("host", "show")), &shown); err != nil || shown.Name != "cloud" || !shown.Current || shown.Host.RegistryPort != 9092 {
		t.Fatalf("show: %+v %v", shown, err)
	}
	if err := json.Unmarshal([]byte(run("--host", "home", "host", "show")), &shown); err != nil || shown.Name != "home" || shown.Current {
		t.Fatalf("show --host: %+v %v", shown, err)
	}
	run("host", "remove", "cloud")
	if got := readConfig(t); got.Current != "" || len(got.Hosts) != 2 {
		t.Fatalf("after remove: %+v", got)
	}
	for _, args := range [][]string{
		{"host", "add", "home", "x@y"},
		{"host", "add", "Bad", "x@y"},
		{"host", "add", "two", "x@y", "--server", "http://127.0.0.1:1"},
		{"host", "add", "none"},
		{"host", "add", "dash", "-oProxyCommand=x"},
		{"host", "add", "port", "x@y", "--api-port", "0"},
		{"host", "add", "http", "--server", "http://example.com"},
		{"host", "use", "missing"},
		{"host", "remove"},
		{"host", "list", "--use"},
		{"host", "bogus"},
		{"host", "add", "x", "x@y", "--unknown"},
		{"host", "add", "quote", "x@y", "-e", "ssh -F 'unterminated"},
		{"host", "add", "local2", "--server", "http://127.0.0.1:1", "-e", "proxyssh"},
		{"--host", "Bad", "list"},
	} {
		if code, _, _ := cliTestRun(args...); code == 0 {
			t.Errorf("%q accepted", args)
		}
	}
}

func TestHostSelection(t *testing.T) {
	cliTestEnvironment(t)
	if code, _, stderr := cliTestRun("host", "add", "first", "a@first", "--token-file", "/first"); code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := cliTestRun("host", "add", "second", "b@second", "--rsh", "proxyssh"); code != 0 {
		t.Fatal(stderr)
	}
	pick := func(args ...string) (connection, options) {
		t.Helper()
		opts, err := parse(append(args, "list"))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := resolve(&opts)
		if err != nil {
			t.Fatal(err)
		}
		return conn, opts
	}
	if conn, opts := pick(); conn.name != "first" || conn.ssh.destination != "a@first" || opts.tokenFile != "/first" {
		t.Errorf("current: %+v %+v", conn, opts)
	}
	if conn, _ := pick("--host", "second"); conn.name != "second" || conn.ssh.command[0] != "proxyssh" || conn.ssh.apiPort != 9081 || conn.ssh.registryPort != 9082 {
		t.Errorf("--host: %+v", conn)
	}
	if _, opts := pick("--token-file", "/explicit"); opts.tokenFile != "/explicit" {
		t.Errorf("explicit token file lost: %+v", opts)
	}
	if conn, _ := pick("--server", "http://127.0.0.1:1"); conn.ssh != nil || conn.server != "http://127.0.0.1:1" {
		t.Errorf("--server: %+v", conn)
	}
	// The remote shell: -e beats the host's, which beats SITE_DEPLOY_RSH.
	if conn, _ := pick("-e", "ssh -p 2222"); !reflect.DeepEqual(conn.ssh.command, []string{"ssh", "-p", "2222"}) {
		t.Errorf("-e: %+v", conn.ssh)
	}
	if conn, _ := pick("--host", "second", "--rsh=ssh"); !reflect.DeepEqual(conn.ssh.command, []string{"ssh"}) {
		t.Errorf("--rsh over the host's: %+v", conn.ssh)
	}
	t.Setenv("SITE_DEPLOY_RSH", "rsh-default")
	if conn, _ := pick(); conn.ssh.command[0] != "rsh-default" {
		t.Errorf("SITE_DEPLOY_RSH for a host without one: %+v", conn.ssh)
	}
	if conn, _ := pick("--host", "second"); conn.ssh.command[0] != "proxyssh" {
		t.Errorf("SITE_DEPLOY_RSH beat the host's: %+v", conn.ssh)
	}
	t.Setenv("SITE_DEPLOY_SSH", "c@env")
	t.Setenv("SITE_DEPLOY_RSH", "proxyssh -v -F '/ci/ssh config'")
	t.Setenv("SITE_DEPLOY_REGISTRY_PORT", "9999")
	if conn, _ := pick(); conn.name != "" || conn.ssh.destination != "c@env" ||
		!reflect.DeepEqual(conn.ssh.command, []string{"proxyssh", "-v", "-F", "/ci/ssh config"}) || conn.ssh.registryPort != 9999 {
		t.Errorf("SITE_DEPLOY_SSH: %+v", conn.ssh)
	}
	if conn, _ := pick("--host", "first"); conn.name != "first" {
		t.Errorf("--host should beat SITE_DEPLOY_SSH: %+v", conn)
	}
	t.Setenv("SITE_DEPLOY_SSH", "")
	t.Setenv("SITE_DEPLOY_HOST", "second")
	if conn, _ := pick(); conn.name != "second" {
		t.Errorf("SITE_DEPLOY_HOST: %+v", conn)
	}
	t.Setenv("SITE_DEPLOY_HOST", "missing")
	opts, _ := parse([]string{"list"})
	if _, err := resolve(&opts); err == nil || !strings.Contains(err.Error(), `unknown host "missing"`) {
		t.Errorf("unknown host: %v", err)
	}
}

func TestCommandsRunThroughTheTunnel(t *testing.T) {
	cliTestEnvironment(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(cliTestToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cliTestToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/apps":
			io.WriteString(w, `[{"name":"web","state":"running","spec":{"domain":"web.example","idle_timeout":"5m"}}]`)
		case "/v1/registry":
			io.WriteString(w, `{"address":"127.0.0.1:9082","images":[]}`)
		case "/v1/version":
			io.WriteString(w, `{"version":"dev"}`)
		}
	}))
	defer server.Close()
	apiPort := server.Listener.Addr().(*net.TCPAddr).Port
	registry, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	registryPort := registry.Addr().(*net.TCPAddr).Port
	t.Setenv("SITE_DEPLOY_FAKE_SSH", "1")
	add := []string{"host", "add", "fake", "deploy@fake", "-e", os.Args[0], "--token-file", tokenFile,
		"--api-port", fmt.Sprint(apiPort), "--registry-port", fmt.Sprint(registryPort)}
	if code, _, stderr := cliTestRun(add...); code != 0 {
		t.Fatal(stderr)
	}
	code, stdout, stderr := cliTestRun("list")
	if code != 0 || !strings.Contains(stdout, "web.example") {
		t.Fatalf("list: %d %q %q", code, stdout, stderr)
	}
	code, stdout, stderr = cliTestRun("daemon-version")
	if code != 0 || !strings.Contains(stdout, "Host    fake (deploy@fake (-e ") {
		t.Fatalf("daemon-version: %d %q %q", code, stdout, stderr)
	}

	// push forwards the registry too and pushes through that forward.
	var via string
	previous := dockerCommand
	dockerCommand = func(_ context.Context, _, _ io.Writer, args ...string) error {
		if args[1] == "push" {
			via = strings.Split(args[2], "/")[0]
			if !listening(via) {
				return fmt.Errorf("registry forward %s is not open", via)
			}
		}
		return nil
	}
	t.Cleanup(func() { dockerCommand = previous })
	code, stdout, stderr = cliTestRun("push", "app:local", "app:1")
	if code != 0 || !strings.Contains(stdout, "image: 127.0.0.1:9082/app:1") || !strings.Contains(stderr, "Host: fake") ||
		via == "" || via == fmt.Sprintf("127.0.0.1:%d", registryPort) {
		t.Fatalf("push: %d via=%s %q %q", code, via, stdout, stderr)
	}
	if listening(via) {
		t.Fatalf("tunnel still forwarding %s after the command", via)
	}

	// SSH failures are reported with ssh's own message.
	t.Setenv("SITE_DEPLOY_FAKE_SSH_FAIL", "Permission denied (publickey).")
	code, _, stderr = cliTestRun("list")
	if code != 1 || !strings.Contains(stderr, "SSH tunnel to deploy@fake") || !strings.Contains(stderr, "Permission denied (publickey).") {
		t.Fatalf("ssh failure: %d %q", code, stderr)
	}
}

func TestSSHAndFetchToken(t *testing.T) {
	cliTestEnvironment(t)
	t.Setenv("SITE_DEPLOY_FAKE_SSH", "1")
	tokenFile := filepath.Join(t.TempDir(), "private", "token")
	if code, _, stderr := cliTestRun("host", "add", "fake", "deploy@fake", "-e", os.Args[0], "--token-file", tokenFile); code != 0 {
		t.Fatal(stderr)
	}
	// The remote command's exit status is the CLI's.
	code, stdout, stderr := cliTestRun("ssh", "--", "uname", "-a")
	if code != 3 || stdout != "deploy@fake|uname|-a" {
		t.Fatalf("ssh: %d %q %q", code, stdout, stderr)
	}
	t.Setenv("SITE_DEPLOY_FAKE_SSH_OUTPUT", cliTestToken)
	if code, stdout, stderr := cliTestRun("host", "fetch-token"); code != 1 || !strings.Contains(stderr, "exit status 3") {
		t.Fatalf("fetch-token with a failing ssh: %d %q %q", code, stdout, stderr)
	}
	if code, _, _ := cliTestRun("--server", "http://127.0.0.1:1", "ssh"); code != 1 {
		t.Error("ssh without an SSH host succeeded")
	}
}

func TestFetchTokenSavesAPrivateFile(t *testing.T) {
	cliTestEnvironment(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "ssh")
	tokenFile := filepath.Join(dir, "tokens", "fake")
	// A stand-in ssh that prints the token for any remote command.
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho "+cliTestToken+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := cliTestRun("host", "add", "fake", "deploy@fake", "--rsh", script, "--token-file", tokenFile); code != 0 {
		t.Fatal(stderr)
	}
	code, stdout, stderr := cliTestRun("host", "fetch-token", "fake")
	data, err := os.ReadFile(tokenFile)
	info, statErr := os.Stat(tokenFile)
	if code != 0 || err != nil || statErr != nil || strings.TrimSpace(string(data)) != cliTestToken || info.Mode().Perm() != 0600 {
		t.Fatalf("fetch-token: %d %q %q data=%q mode=%v", code, stdout, stderr, data, info.Mode())
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho short\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := cliTestRun("host", "fetch-token", "fake"); code != 1 {
		t.Error("accepted an invalid token")
	}
}

func TestSplitCommandLikeRsync(t *testing.T) {
	for command, want := range map[string][]string{
		"proxyssh":                        {"proxyssh"},
		"  ssh   -p 2222 ":                {"ssh", "-p", "2222"},
		`ssh -F '/a b/config' -o "X=y z"`: {"ssh", "-F", "/a b/config", "-o", "X=y z"},
		`ssh -i /a\ b`:                    {"ssh", "-i", "/a b"},
		`ssh "say \"hi\""`:                {"ssh", `say "hi"`},
		`ssh ''`:                          {"ssh", ""},
	} {
		got, err := splitCommand(command)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitCommand(%q) = %q, %v; want %q", command, got, err, want)
		}
	}
	for _, command := range []string{`ssh 'open`, `ssh "open`, `ssh \`} {
		if got, err := splitCommand(command); err == nil {
			t.Errorf("splitCommand(%q) = %q; want an error", command, got)
		}
	}
}
