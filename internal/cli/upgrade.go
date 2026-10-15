package cli

import (
	"archive/tar"
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"site-deploy/deploy"
	"site-deploy/internal/version"
)

// installScript unpacks the binary, install-host.sh and the systemd unit that
// arrive as a tar stream on stdin, and installs them as root, after making sure
// the binary runs on the server and reports the version line that fills %s,
// single-quoted. Its exit status is install-host.sh's.
const installScript = `set -e
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
tar -xf - -C "$dir"
built=$("$dir/site-deploy" version)
if [ "$built" != %s ]; then
	echo "the uploaded binary reports \"$built\" on this server; nothing was installed" >&2
	exit 1
fi
sudo -n bash "$dir/install-host.sh" "$dir/site-deploy"`

// readBuild reads a Go binary's build information; tests replace it.
var readBuild = buildinfo.ReadFile

// defaultVersionURL is VERSION on the repository's main branch, the latest
// release, which the GitHub Action also builds its CLI from.
// SITE_DEPLOY_VERSION_URL replaces it, for example for a fork.
const defaultVersionURL = "https://raw.githubusercontent.com/imeredith/site-deploy/main/VERSION"

// runUpgrade installs this CLI's version as the daemon of the selected host,
// over SSH, when it is newer than the installed one, so the CLI is never older
// than the daemon it upgrades. It first compares the CLI with the latest
// version in the repository. The server backs up its data, switches releases,
// and rolls back if the new daemon does not report its version
// (deploy/install-host.sh).
func runUpgrade(opts options, stdout, stderr io.Writer) error {
	if len(opts.positionals) == 1 {
		if opts.host != "" && opts.host != opts.positionals[0] {
			return fmt.Errorf("upgrade %s conflicts with --host %s", opts.positionals[0], opts.host)
		}
		opts.host = opts.positionals[0]
	}
	if _, release := version.Compare(version.Version, version.Version); !release {
		return fmt.Errorf("this CLI (%s) is not a release build; build it with make install-cli in the site-deploy checkout", version.Version)
	}
	conn, err := resolve(&opts)
	if err != nil {
		return err
	}
	if conn.ssh == nil {
		return errors.New("upgrade needs a host reached over SSH; choose one with --host NAME, or set SITE_DEPLOY_SSH")
	}
	label := conn.ssh.describe()
	if conn.name != "" {
		label = conn.name + " (" + label + ")"
	}
	fmt.Fprintf(stderr, "Host: %s\n", label)

	machine, err := remoteOutput(*conn.ssh, "uname -m")
	if err != nil {
		return err
	}
	arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[strings.TrimSpace(machine)]
	if arch == "" {
		return fmt.Errorf("unsupported server architecture %q", strings.TrimSpace(machine))
	}
	installed, err := installedVersion(*conn.ssh)
	if err != nil {
		return err
	}
	latest, latestErr := repositoryVersion()
	shown := installed
	if shown == "" {
		shown = "none"
	}
	fmt.Fprintf(stdout, "Installed  %s (linux/%s)\nThis CLI   %s\n", shown, arch, version.Version)
	if latestErr != nil {
		fmt.Fprintf(stdout, "Repository unknown\n")
		fmt.Fprintf(stderr, "Warning: could not read the latest version from the repository: %v\n", latestErr)
	} else {
		fmt.Fprintf(stdout, "Repository %s\n", latest)
	}

	// Decide before looking for the binary, which an up-to-date host doesn't need.
	var problem error
	if latestErr == nil {
		if order, _ := version.Compare(version.Version, latest); order < 0 {
			problem = fmt.Errorf("the repository has %s, newer than this CLI's %s; update the site-deploy checkout and run make install-cli first", latest, version.Version)
		} else if order > 0 {
			fmt.Fprintf(stderr, "Warning: this CLI's %s is newer than the repository's %s. Push it first if GitHub Action workflows deploy to this host: they build their CLI from the repository, and a newer daemon refuses it (HTTP 426).\n", version.Version, latest)
		}
	}
	upToDate := false
	if installed != "" && problem == nil {
		order, ok := version.Compare(version.Version, installed)
		switch {
		case !ok:
			problem = fmt.Errorf("the installed %s is not a release version, so it can't be compared", installed)
		case order == 0:
			upToDate = true
		case order < 0:
			problem = fmt.Errorf("the installed %s is newer than this CLI's %s; update the site-deploy checkout and run make install-cli first", installed, version.Version)
		}
	}
	if problem != nil && !opts.force {
		return fmt.Errorf("%w; pass --force to install %s anyway", problem, version.Version)
	}
	if upToDate && !opts.force {
		_, err := fmt.Fprintf(stdout, "%s already runs %s; nothing to install (--force reinstalls it)\n", label, installed)
		return err
	}
	binary, err := serverBinary(opts.binary, arch)
	if err != nil {
		return err
	}
	if opts.check {
		_, err := fmt.Fprintf(stdout, "Would install %s from %s\n", version.Version, binary)
		return err
	}

	files, err := installFiles(binary)
	if err != nil {
		return err
	}
	// Release versions hold only letters, digits, dots, hyphens and plus signs.
	program, args := conn.ssh.args(nil, fmt.Sprintf(installScript, "'site-deploy "+version.Version+"'"))
	cmd := exec.Command(program, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(files), stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install on %s failed (%v); its output is above", label, err)
	}
	if installed == "" && conn.name != "" {
		config, _, err := loadConfig()
		if err != nil {
			return err
		}
		host := config.Hosts[conn.name]
		if opts.rshSet {
			host.RSH = opts.rsh
		}
		return fetchToken(conn.name, host, stdout)
	}
	return nil
}

// repositoryVersion reads the latest release version from the repository.
func repositoryVersion() (string, error) {
	address := os.Getenv("SITE_DEPLOY_VERSION_URL")
	if address == "" {
		address = defaultVersionURL
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get(address)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned HTTP %d", address, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64))
	if err != nil {
		return "", err
	}
	latest := strings.TrimSpace(string(data))
	if _, ok := version.Compare(latest, latest); !ok {
		return "", fmt.Errorf("%s does not hold a version: %q", address, diagnostic(latest, ""))
	}
	return latest, nil
}

// installedVersion is the server's site-deploy version: the running daemon's,
// as the daemon's own binary reports it with the server's token, else the
// installed binary's when the daemon doesn't answer. It is empty when the
// server has no site-deploy.
func installedVersion(target sshTarget) (string, error) {
	output, err := remoteOutput(target, "if [ ! -e /usr/local/bin/site-deploy ]; then echo '{}'; else "+
		"sudo -n /usr/local/bin/site-deploy daemon-version --json --token-file "+serverDataDir+"/token 2>/dev/null || "+
		"/usr/local/bin/site-deploy version --json; fi")
	if err != nil {
		return "", err
	}
	// daemon-version prints its JSON before failing for an older binary, so
	// the first object is the answer.
	var reported struct {
		Daemon  string `json:"daemon"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&reported); err != nil {
		return "", fmt.Errorf("read the installed site-deploy version: %w", err)
	}
	if reported.Daemon != "" {
		return diagnostic(reported.Daemon, ""), nil
	}
	return diagnostic(reported.Version, ""), nil
}

// remoteOutput runs a command on the target and returns its standard output.
func remoteOutput(target sshTarget, command string) (string, error) {
	program, args := target.args(nil, command)
	cmd := exec.Command(program, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return string(output), fmt.Errorf("run %q on %s: %v: %s", command, target.destination, err, sshError(stderr.String()))
	}
	return string(output), nil
}

// serverBinary finds a linux/arch build of this CLI's version: the file named
// by --binary, else this executable when it is one, else the build that make
// install-cli puts in ../lib/site-deploy/linux-ARCH/ beside it.
func serverBinary(explicit, arch string) (string, error) {
	if explicit != "" {
		return explicit, checkBinary(explicit, arch)
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", err
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		return self, checkBinary(self, arch)
	}
	installed := filepath.Join(filepath.Dir(self), "..", "lib", "site-deploy", "linux-"+arch, "site-deploy")
	if _, err := os.Stat(installed); err != nil {
		return "", fmt.Errorf("no linux/%s build of site-deploy beside this CLI (%s); run make install-cli in the site-deploy checkout, or pass --binary FILE", arch, installed)
	}
	return installed, checkBinary(installed, arch)
}

// checkBinary makes sure a file is a linux/arch build of this CLI's version.
// Go records the target in every binary's build information. Builds with
// -trimpath leave out -ldflags, which stamps the version, so the stamped
// string itself is looked for; the server confirms it by running the binary.
func checkBinary(path, arch string) error {
	info, err := readBuild(path)
	if err != nil {
		return fmt.Errorf("%s is not a site-deploy build: %w", path, err)
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != "linux" || settings["GOARCH"] != arch {
		return fmt.Errorf("%s is built for %s/%s, and the server needs linux/%s", path, settings["GOOS"], settings["GOARCH"], arch)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(version.Version)) {
		return fmt.Errorf("%s is not version %s, this CLI's; rebuild it with make install-cli", path, version.Version)
	}
	return nil
}

// installFiles is the tar stream that installScript unpacks.
func installFiles(binary string) ([]byte, error) {
	program, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	for _, file := range []struct {
		name string
		mode int64
		data []byte
	}{
		{"site-deploy", 0755, program},
		{"install-host.sh", 0755, deploy.InstallHost},
		{"site-deploy.service", 0644, deploy.Service},
	} {
		if err := archive.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := archive.Write(file.data); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
