package cli

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sshTarget is a daemon reached through SSH. The control API and registry
// listen only on the server's loopback interface, so every command forwards
// local ports to them for as long as it runs.
type sshTarget struct {
	destination  string
	rsh          string   // remote shell as configured, like rsync's -e; empty means ssh
	command      []string // rsh split into the program and its leading arguments
	apiPort      int
	registryPort int
}

func (t sshTarget) describe() string {
	if t.rsh != "" && t.rsh != "ssh" {
		return fmt.Sprintf("%s (-e %s)", t.destination, t.rsh)
	}
	return t.destination
}

// splitCommand splits a remote shell command the way rsync splits -e: at
// whitespace, with single quotes, double quotes and backslashes keeping spaces
// inside an argument. No other shell syntax applies.
func splitCommand(command string) ([]string, error) {
	var args []string
	var current strings.Builder
	inArg, escaped := false, false
	var quote rune
	for _, r := range command {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case quote == '"' && r == '"':
			quote = 0
		case r == '\\' && quote != '\'':
			escaped, inArg = true, true
		case quote == '"':
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote, inArg = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inArg {
				args = append(args, current.String())
				current.Reset()
				inArg = false
			}
		default:
			current.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("remote shell %q has an unterminated quote or escape", command)
	}
	if inArg {
		args = append(args, current.String())
	}
	return args, nil
}

// withRSH sets the remote shell, splitting it into a command.
func (t *sshTarget) withRSH(rsh string) error {
	command := []string{"ssh"}
	if strings.TrimSpace(rsh) != "" {
		var err error
		if command, err = splitCommand(rsh); err != nil {
			return err
		}
	}
	t.rsh, t.command = rsh, command
	return nil
}

// args returns the program and its arguments for running ssh options, then the
// destination, then extra (a remote command).
func (t sshTarget) args(options []string, extra ...string) (string, []string) {
	command := t.command
	if len(command) == 0 {
		command = []string{"ssh"}
	}
	args := append(append([]string{}, command[1:]...), options...)
	args = append(append(args, "--", t.destination), extra...)
	return command[0], args
}

type tunnel struct {
	server, registry string
	cmd              *exec.Cmd
	done             chan struct{}
	stderr           *lockedBuffer
}

const tunnelSetupTimeout = 30 * time.Second

// openTunnel runs ssh in the background with forwards from free loopback
// ports to the daemon's control API and, when asked, its registry. The kernel
// picks each port, which is closed just before ssh binds it; if another
// program takes it in between, ssh exits (ExitOnForwardFailure) and a new
// attempt picks again. Other failures, such as authentication, aren't retried.
func openTunnel(target sshTarget, registry bool) (*tunnel, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var t *tunnel
		t, err = startTunnel(target, registry)
		if err == nil {
			return t, nil
		}
		if !strings.Contains(err.Error(), "forwarding") && !strings.Contains(err.Error(), "Address already in use") {
			break
		}
	}
	return nil, err
}

func startTunnel(target sshTarget, registry bool) (*tunnel, error) {
	apiPort, err := freePort()
	if err != nil {
		return nil, err
	}
	forwards := []string{"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", apiPort, target.apiPort)}
	t := &tunnel{server: "http://127.0.0.1:" + strconv.Itoa(apiPort), done: make(chan struct{}), stderr: &lockedBuffer{}}
	addresses := []string{"127.0.0.1:" + strconv.Itoa(apiPort)}
	if registry {
		registryPort, err := freePort()
		if err != nil {
			return nil, err
		}
		t.registry = "127.0.0.1:" + strconv.Itoa(registryPort)
		forwards = append(forwards, "-L", fmt.Sprintf("%s:127.0.0.1:%d", t.registry, target.registryPort))
		addresses = append(addresses, t.registry)
	}
	options := append([]string{"-N", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15"}, forwards...)
	program, args := target.args(options)
	t.cmd = exec.Command(program, args...)
	// Prompts for passwords or passphrases use the terminal directly.
	t.cmd.Stderr = t.stderr
	stopWithParent(t.cmd)
	if err := t.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", program, err)
	}
	go func() {
		t.cmd.Wait()
		close(t.done)
	}()
	deadline := time.Now().Add(tunnelSetupTimeout)
	for {
		select {
		case <-t.done:
			return nil, fmt.Errorf("SSH tunnel to %s failed: %s", target.describe(), sshError(t.stderr.String()))
		default:
		}
		if listening(addresses...) {
			return t, nil
		}
		if time.Now().After(deadline) {
			t.Close()
			return nil, fmt.Errorf("SSH tunnel to %s was not ready after %s: %s", target.describe(), tunnelSetupTimeout, sshError(t.stderr.String()))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Close stops ssh, which closes the forwards.
func (t *tunnel) Close() {
	t.cmd.Process.Signal(os.Interrupt)
	select {
	case <-t.done:
	case <-time.After(3 * time.Second):
		t.cmd.Process.Kill()
		<-t.done
	}
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free local port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func listening(addresses ...string) bool {
	for _, address := range addresses {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
	}
	return true
}

func sshError(stderr string) string {
	stderr = diagnostic(stderr, "")
	if stderr == "" {
		return "ssh exited without a message"
	}
	return stderr
}

// lockedBuffer collects ssh's stderr while the tunnel is checked.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
