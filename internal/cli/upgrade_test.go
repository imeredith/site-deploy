package cli

import (
	"archive/tar"
	"bytes"
	"debug/buildinfo"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"site-deploy/deploy"
	"site-deploy/internal/version"
)

// fakeServer is a remote shell standing in for ssh to a server: it answers
// uname, the version probe and fetch-token's sudo cat, and saves an upload.
const fakeServer = `#!/bin/sh
shift 2 # -- DESTINATION
case "$1" in
"uname -m") echo "$FAKE_MACHINE" ;;
*daemon-version*) if [ -n "$FAKE_INSTALLED" ]; then echo "{\"daemon\":\"$FAKE_INSTALLED\"}"; else echo '{}'; fi ;;
*"tar -xf -"*) cat >"$FAKE_UPLOAD"; printf '%s\n' "$1" >"$FAKE_UPLOAD.script"; echo "site-deploy is running" ;;
sudo) echo "$FAKE_TOKEN" ;;
*) echo "unexpected command: $*" >&2; exit 9 ;;
esac
`

type upgradeFixture struct {
	t      *testing.T
	upload string
	binary string
}

func newUpgradeFixture(t *testing.T, cli, installed, repository string) *upgradeFixture {
	t.Helper()
	cliTestEnvironment(t)
	previous, previousRead := version.Version, readBuild
	t.Cleanup(func() { version.Version, readBuild = previous, previousRead })
	version.Version = cli
	dir := t.TempDir()
	f := &upgradeFixture{t: t, upload: filepath.Join(dir, "upload.tar"), binary: filepath.Join(dir, "site-deploy-arm64")}
	if err := os.WriteFile(f.binary, []byte("arm64 program "+cli), 0755); err != nil {
		t.Fatal(err)
	}
	readBuild = func(path string) (*buildinfo.BuildInfo, error) {
		if path != f.binary {
			return nil, fmt.Errorf("not a Go binary")
		}
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "arm64"},
		}}, nil
	}
	shell := filepath.Join(dir, "fake-ssh")
	if err := os.WriteFile(shell, []byte(fakeServer), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_MACHINE", "aarch64")
	t.Setenv("FAKE_INSTALLED", installed)
	t.Setenv("FAKE_UPLOAD", f.upload)
	t.Setenv("FAKE_TOKEN", cliTestToken)
	if repository == "" {
		t.Setenv("SITE_DEPLOY_VERSION_URL", "http://127.0.0.1:1/VERSION")
	} else {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, repository+"\n")
		}))
		t.Cleanup(server.Close)
		t.Setenv("SITE_DEPLOY_VERSION_URL", server.URL)
	}
	if code, _, stderr := cliTestRun("host", "add", "box", "deploy@box", "-e", shell, "--token-file", filepath.Join(dir, "token")); code != 0 {
		t.Fatal(stderr)
	}
	return f
}

func (f *upgradeFixture) run(args ...string) (int, string, string) {
	return cliTestRun(append([]string{"upgrade", "box", "--binary", f.binary}, args...)...)
}

// uploaded returns the files the server received, or nil.
func (f *upgradeFixture) uploaded() map[string]string {
	data, err := os.ReadFile(f.upload)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	files := map[string]string{}
	archive := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			f.t.Fatal(err)
		}
		body, _ := io.ReadAll(archive)
		files[fmt.Sprintf("%s %o", header.Name, header.Mode)] = string(body)
	}
}

func TestUpgradeInstallsOnlyANewerVersion(t *testing.T) {
	f := newUpgradeFixture(t, "0.8.0+abc", "0.8.0+def", "0.8.0")
	code, stdout, stderr := f.run()
	if code != 0 || !strings.Contains(stdout, "already runs 0.8.0+def; nothing to install") || f.uploaded() != nil {
		t.Fatalf("same version: %d %q %q", code, stdout, stderr)
	}
	t.Setenv("FAKE_INSTALLED", "0.9.0")
	if code, stdout, stderr := f.run(); code != 1 || !strings.Contains(stderr, "the installed 0.9.0 is newer than this CLI's") || f.uploaded() != nil {
		t.Fatalf("newer installed: %d %q %q", code, stdout, stderr)
	}
	t.Setenv("FAKE_INSTALLED", "0.7.0")
	code, stdout, stderr = f.run()
	if code != 0 || !strings.Contains(stdout, "Installed  0.7.0 (linux/arm64)\nThis CLI   0.8.0+abc\nRepository 0.8.0") || !strings.Contains(stdout, "site-deploy is running") {
		t.Fatalf("upgrade: %d %q %q", code, stdout, stderr)
	}
	want := map[string]string{
		"site-deploy 755":         "arm64 program 0.8.0+abc",
		"install-host.sh 755":     string(deploy.InstallHost),
		"site-deploy.service 644": string(deploy.Service),
	}
	if got := f.uploaded(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("uploaded %v", got)
	}
	// The server runs the binary and stops unless it reports this version.
	script, err := os.ReadFile(f.upload + ".script")
	if err != nil || !strings.Contains(string(script), `if [ "$built" != 'site-deploy 0.8.0+abc' ]; then`) {
		t.Fatalf("install script %q %v", script, err)
	}
}

func TestUpgradeForceReinstallsAndDowngrades(t *testing.T) {
	f := newUpgradeFixture(t, "0.8.0", "0.9.0", "0.8.0")
	if code, stdout, stderr := f.run("--force"); code != 0 || f.uploaded() == nil {
		t.Fatalf("forced downgrade: %d %q %q", code, stdout, stderr)
	}
}

func TestUpgradeChecksTheRepository(t *testing.T) {
	f := newUpgradeFixture(t, "0.8.0", "0.7.0", "0.9.0")
	if code, stdout, stderr := f.run(); code != 1 || !strings.Contains(stderr, "the repository has 0.9.0, newer than this CLI's 0.8.0") || f.uploaded() != nil {
		t.Fatalf("outdated CLI: %d %q %q", code, stdout, stderr)
	}
	f = newUpgradeFixture(t, "0.8.0", "0.7.0", "0.7.5")
	if code, stdout, stderr := f.run(); code != 0 || !strings.Contains(stderr, "newer than the repository's 0.7.5. Push it first") || f.uploaded() == nil {
		t.Fatalf("unpushed CLI: %d %q %q", code, stdout, stderr)
	}
	f = newUpgradeFixture(t, "0.8.0", "0.7.0", "")
	if code, stdout, stderr := f.run(); code != 0 || !strings.Contains(stderr, "could not read the latest version from the repository") || f.uploaded() == nil {
		t.Fatalf("unreachable repository: %d %q %q", code, stdout, stderr)
	}
}

func TestUpgradeCheckChangesNothing(t *testing.T) {
	f := newUpgradeFixture(t, "0.8.0", "0.7.0", "0.8.0")
	if code, stdout, stderr := f.run("--check"); code != 0 || !strings.Contains(stdout, "Would install 0.8.0 from "+f.binary) || f.uploaded() != nil {
		t.Fatalf("check: %d %q %q", code, stdout, stderr)
	}
}

func TestUpgradeRefusesTheWrongBinary(t *testing.T) {
	f := newUpgradeFixture(t, "0.8.0", "0.7.0", "0.8.0")
	t.Setenv("FAKE_MACHINE", "x86_64")
	if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "built for linux/arm64, and the server needs linux/amd64") {
		t.Fatalf("wrong architecture: %d %q", code, stderr)
	}
	t.Setenv("FAKE_MACHINE", "aarch64")
	if err := os.WriteFile(f.binary, []byte("arm64 program 0.7.9"), 0755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "is not version 0.8.0, this CLI's") || f.uploaded() != nil {
		t.Fatalf("wrong version: %d %q", code, stderr)
	}
	version.Version = "dev"
	if code, _, stderr := f.run(); code != 1 || !strings.Contains(stderr, "not a release build") {
		t.Fatalf("dev CLI: %d %q", code, stderr)
	}
}

func TestUpgradeFirstInstallFetchesTheToken(t *testing.T) {
	f := newUpgradeFixture(t, "0.8.0", "", "0.8.0")
	code, stdout, stderr := f.run()
	if code != 0 || !strings.Contains(stdout, "Installed  none") || !strings.Contains(stdout, "Saved the token of box") || f.uploaded() == nil {
		t.Fatalf("first install: %d %q %q", code, stdout, stderr)
	}
}

func TestUpgradeUsage(t *testing.T) {
	cliTestEnvironment(t)
	for _, args := range [][]string{
		{"upgrade", "a", "b"},
		{"upgrade", "Bad"},
		{"list", "--force"},
		{"deploy", "app", "app.zip", "--binary", "x"},
	} {
		if code, _, _ := cliTestRun(args...); code != 2 {
			t.Errorf("%q: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := cliTestRun("upgrade", "--server", "http://127.0.0.1:1"); code != 1 || !strings.Contains(stderr, "needs a host reached over SSH") && !strings.Contains(stderr, "not a release build") {
		t.Errorf("upgrade without SSH: %d %q", code, stderr)
	}
}
