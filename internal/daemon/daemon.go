// Package daemon wires the authenticated control API and application gateway.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"site-deploy/internal/api"
	"site-deploy/internal/auth"
	"site-deploy/internal/btrfs"
	"site-deploy/internal/caddy"
	"site-deploy/internal/engine"
	"site-deploy/internal/gateway"
	"site-deploy/internal/hostpaths"
	"site-deploy/internal/images"
	"site-deploy/internal/orchestrator"
	"site-deploy/internal/version"
	"site-deploy/internal/volumes"
)

func Run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDir := flags.String("data-dir", ".site-deploy", "persistent deployment directory")
	apiAddr := flags.String("api", "127.0.0.1:9081", "loopback control API address")
	gatewayAddr := flags.String("gateway", "127.0.0.1:9080", "loopback application gateway address")
	tokenFile := flags.String("token-file", "", "authentication token file (default DATA_DIR/token; generated if absent)")
	token := flags.String("token", os.Getenv("SITE_DEPLOY_TOKEN"), "API token (prefer token file or SITE_DEPLOY_TOKEN)")
	dockerBinary := flags.String("docker-binary", "docker", "Docker CLI executable")
	dockerHost := flags.String("docker-host", "unix:///var/run/docker.sock", "local Docker Unix socket URL")
	interval := flags.Duration("idle-scan", 5*time.Second, "idle application scan interval")
	caddyAdmin := flags.String("caddy-admin", "", "dedicated Caddy admin URL; enables complete configuration replacement")
	httpOnly := flags.Bool("http", false, "configure Caddy for HTTP on port 80 instead of automatic HTTPS")
	tlsCertDir := flags.String("tls-cert-dir", "", "folder of .pem files, each a certificate chain and its key, that Caddy serves instead of obtaining certificates for the names they cover; a path as Caddy sees it")
	tlsInternal := flags.Bool("tls-internal", false, "serve HTTPS with certificates from Caddy's local CA, which browsers do not trust, instead of public ACME certificates (for hosts public challenges cannot reach)")
	registryAddr := flags.String("registry", "", "loopback address for the private image registry, such as 127.0.0.1:9082 (empty disables it)")
	registryImage := flags.String("registry-image", "registry:3", "image that runs the private registry")
	copyImage := flags.String("volume-copy-image", engine.DefaultCopyImage, "image that copies named volumes for deploy --copy-volumes-from (needs sh, find, and cp -a); it also runs the btrfs helper for --volumes-dir")
	volumesDir := flags.String("volumes-dir", "", "btrfs directory for site-deploy volumes, which bundles mount, or mount snapshots of, by name (empty disables them)")
	registryGrace := flags.Duration("registry-grace", 24*time.Hour, "keep unused registry tags this long after their push")
	pruneImages := flags.Bool("prune-images", false, "remove Docker images that no container uses (only on a host whose Docker is dedicated to site-deploy)")
	cleanupInterval := flags.Duration("cleanup-interval", time.Hour, "how often to remove unused images")
	var hostPaths hostpaths.Policy
	flags.Func("host-path", "allow bind mounts of a host directory and anything under it, as [APP,...=]PATH[:rw]; read-only unless :rw, every application unless APPs are named (repeatable)", func(value string) error {
		rule, err := hostpaths.Parse(value)
		if err == nil {
			hostPaths = append(hostPaths, rule)
		}
		return err
	})
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("serve does not accept positional arguments")
	}
	if *interval <= 0 {
		return errors.New("idle-scan must be positive")
	}
	caddyMode := caddy.HTTPS
	switch {
	case *httpOnly && *tlsInternal:
		return errors.New("http and tls-internal are mutually exclusive")
	case *httpOnly:
		caddyMode = caddy.HTTP
	case *tlsInternal:
		caddyMode = caddy.InternalHTTPS
	}
	var certFolders []string
	if *tlsCertDir != "" {
		certFolders = []string{*tlsCertDir}
		// Checked now rather than at the first sync, which exits with a less obvious error.
		if _, err := caddy.Config(nil, "127.0.0.1:9080", caddyMode, certFolders...); err != nil {
			return fmt.Errorf("tls-cert-dir: %w", err)
		}
	}
	if *cleanupInterval <= 0 || *registryGrace < 0 {
		return errors.New("cleanup-interval must be positive and registry-grace nonnegative")
	}
	if err := loopbackAddress(*apiAddr); err != nil {
		return fmt.Errorf("api: %w", err)
	}
	if err := loopbackAddress(*gatewayAddr); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	if !strings.HasPrefix(*dockerHost, "unix:///") {
		return errors.New("docker-host must be a local unix:/// socket; bind mounts use local paths")
	}
	root, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err := hostPaths.Validate(root, strings.TrimPrefix(*dockerHost, "unix://")); err != nil {
		return err
	}
	if *volumesDir != "" {
		if err := checkVolumesDir(*volumesDir, root, strings.TrimPrefix(*dockerHost, "unix://"), hostPaths); err != nil {
			return err
		}
	}
	for _, rule := range hostPaths {
		fmt.Fprintf(stderr, "Allowing host path %s\n", rule)
	}
	if *tokenFile == "" {
		*tokenFile = filepath.Join(root, "token")
	}
	if *token == "" {
		var generated bool
		*token, generated, err = loadToken(*tokenFile)
		if err != nil {
			return err
		}
		if generated {
			fmt.Fprintf(stderr, "Created API token file: %s\n", *tokenFile)
		}
	}
	if err := auth.ValidateToken(*token); err != nil {
		return err
	}
	apiListener, err := net.Listen("tcp", *apiAddr)
	if err != nil {
		return err
	}
	defer apiListener.Close()
	proxyListener, err := net.Listen("tcp", *gatewayAddr)
	if err != nil {
		return err
	}
	defer proxyListener.Close()
	docker := engine.New(*dockerBinary, *dockerHost)
	if *copyImage == "" {
		return errors.New("volume-copy-image must not be empty")
	}
	docker.CopyImage = *copyImage
	var managerOptions []orchestrator.Option
	if *volumesDir != "" {
		executable, err := helperExecutable()
		if err != nil {
			return err
		}
		subvolumes, err := engine.NewSubvolumes(docker, *volumesDir, executable)
		if err != nil {
			return fmt.Errorf("volumes-dir: %w", err)
		}
		volumeStore, err := volumes.Open(*volumesDir, subvolumes)
		if err != nil {
			return err
		}
		defer volumeStore.Close()
		managerOptions = append(managerOptions, orchestrator.WithVolumes(volumeStore))
		fmt.Fprintf(stderr, "Site-deploy volumes in %s\n", *volumesDir)
	}
	manager, err := orchestrator.New(root, docker, hostPaths, managerOptions...)
	if err != nil {
		return err
	}
	defer manager.Close()
	var imageService *images.Service
	if *registryAddr != "" || *pruneImages {
		imageService = &images.Service{
			Docker: docker, PruneDocker: *pruneImages, Grace: *registryGrace,
			References: manager.ImageReferences, Exclusive: manager.ExclusiveImages,
		}
		if *registryAddr != "" {
			imageService.Registry, err = images.NewRegistry(docker, *registryAddr, *registryImage,
				manager.ProjectPrefix()+"registry", filepath.Join(root, "registry"))
			if err != nil {
				return err
			}
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var syncMu sync.Mutex
	syncCaddy := func(ctx context.Context) error {
		if *caddyAdmin == "" {
			return nil
		}
		syncMu.Lock()
		defer syncMu.Unlock()
		config, err := caddy.Config(manager.Sites(), proxyListener.Addr().String(), caddyMode, certFolders...)
		if err != nil {
			return err
		}
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return caddy.Sync(syncCtx, *caddyAdmin, config)
	}
	if err := syncCaddy(ctx); err != nil {
		return fmt.Errorf("initial Caddy synchronization: %w", err)
	}
	var apiOptions []api.Option
	if imageService != nil {
		apiOptions = append(apiOptions, api.WithImages(imageService))
	}
	control := &http.Server{Handler: api.New(manager, *token, filepath.Join(root, "staging"), syncCaddy, apiOptions...), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	proxy := &http.Server{Handler: gateway.New(manager), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- control.Serve(apiListener) }()
	go func() { errorsCh <- proxy.Serve(proxyListener) }()
	idleDone := make(chan struct{})
	go func() { defer close(idleDone); manager.RunIdle(ctx, *interval) }()
	imagesDone := make(chan struct{})
	go func() {
		defer close(imagesDone)
		if imageService != nil {
			imageService.Run(ctx, *cleanupInterval, slog.New(slog.NewTextHandler(stderr, nil)))
		}
	}()
	caddyDone := make(chan struct{})
	go func() {
		defer close(caddyDone)
		if *caddyAdmin == "" {
			return
		}
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := syncCaddy(ctx); err != nil && ctx.Err() == nil {
					fmt.Fprintf(stderr, "Caddy synchronization will retry: %v\n", err)
				}
			}
		}
	}()
	fmt.Fprintf(stdout, "site-deploy %s\nControl API: http://%s\nApplication gateway: %s\nData directory: %s\n", version.Version, apiListener.Addr(), proxyListener.Addr(), root)
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var shutdownWG sync.WaitGroup
	for _, server := range []*http.Server{control, proxy} {
		shutdownWG.Add(1)
		go func(server *http.Server) {
			defer shutdownWG.Done()
			if server.Shutdown(shutdown) != nil {
				server.Close()
			}
		}(server)
	}
	shutdownWG.Wait()
	<-idleDone
	<-imagesDone
	<-caddyDone
	return err
}

// checkVolumesDir creates the volumes directory if needed, then checks that it
// is a real btrfs directory that no host-path rule reaches, so its volumes,
// snapshots and keys can only be mounted the way site-deploy allows.
func checkVolumesDir(dir, dataDir, dockerSocket string, hostPaths hostpaths.Policy) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || strings.ContainsAny(dir, ",\"\n:$\\") {
		return fmt.Errorf("volumes-dir %q must be a clean absolute path without ',', ':', '$', quotes or '\\'", dir)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("volumes-dir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("volumes-dir: %w", err)
	}
	if resolved != dir {
		return fmt.Errorf("volumes-dir %s goes through a symlink; use %s", dir, resolved)
	}
	data, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	if overlaps(dir, data) {
		return fmt.Errorf("volumes-dir %s overlaps the data directory %s", dir, data)
	}
	if socketDir, err := filepath.EvalSymlinks(filepath.Dir(dockerSocket)); err == nil && overlaps(dir, socketDir) {
		return fmt.Errorf("volumes-dir %s overlaps the Docker socket's directory %s", dir, socketDir)
	}
	if rule, found := hostPaths.Overlapping(dir); found {
		return fmt.Errorf("host path %s overlaps volumes-dir %s, which would let apps mount other apps' snapshots and volume metadata", rule.Path, dir)
	}
	onBtrfs, err := btrfs.OnBtrfs(dir)
	if err != nil {
		return fmt.Errorf("volumes-dir: %w", err)
	}
	if !onBtrfs {
		return fmt.Errorf("volumes-dir %s is not on a btrfs filesystem", dir)
	}
	return nil
}

func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") || a == "/" || b == "/"
}

// helperExecutable is this program's path, for the btrfs helper container,
// which runs it without the host's shared libraries.
func helperExecutable() (string, error) {
	info, ok := debug.ReadBuildInfo()
	static := false
	if ok {
		for _, setting := range info.Settings {
			if setting.Key == "CGO_ENABLED" && setting.Value == "0" {
				static = true
			}
		}
	}
	if !static {
		return "", errors.New("volumes-dir runs this executable in a helper container, so it must be built statically: CGO_ENABLED=0, as make build does")
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find this executable for the btrfs helper: %w", err)
	}
	return filepath.EvalSymlinks(executable)
}

func loopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("must bind a literal loopback IP; use an SSH tunnel for remote control")
	}
	return nil
}

func loadToken(path string) (string, bool, error) {
	read := func() (string, bool, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return "", false, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return "", false, errors.New("token file must be a regular file readable only by its owner (chmod 600)")
		}
		if info.Size() > auth.MaxTokenBytes+2 {
			return "", false, errors.New("token file exceeds the maximum token size")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false, err
		}
		return strings.TrimSpace(string(data)), false, nil
	}
	if _, err := os.Lstat(path); err == nil {
		return read()
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", false, err
	}
	token := hex.EncodeToString(entropy[:])
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return read()
	}
	if err != nil {
		return "", false, err
	}
	if _, err = f.WriteString(token + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return "", false, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return "", false, err
	}
	if err = f.Close(); err != nil {
		return "", false, err
	}
	return token, true, nil
}
