// Package orchestrator coordinates deployments, request leases, and idle suspension.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"site-deploy/internal/archive"
	"site-deploy/internal/engine"
	"site-deploy/internal/hostpaths"
	"site-deploy/internal/manifest"
	"site-deploy/internal/model"
	"site-deploy/internal/store"
	"site-deploy/internal/volumes"
)

var (
	ErrNotFound    = errors.New("application not found")
	ErrInvalid     = errors.New("invalid deployment")
	ErrConflict    = errors.New("application name or domain is already deployed")
	ErrBusy        = errors.New("application has active requests or a lifecycle operation in progress")
	ErrUnavailable = errors.New("application is unavailable")
)

type Lease struct {
	Upstream string
	// Context ends when the daemon breaks this request's connection, for
	// example to delete the application. It is nil for leaseless checks.
	Context context.Context
	Release func()
	// ReportFailure invalidates this specific running incarnation. The failed
	// request is never replayed; later requests reconcile after leases drain.
	ReportFailure func(error)
}

const wakeFailureCooldown = 2 * time.Second

// leaseDrainTimeout bounds how long deletion waits for broken connections to
// finish. Stopping the containers ends any that remain. Tests shorten it.
var leaseDrainTimeout = 5 * time.Second

type operation struct {
	done chan struct{}
	err  error
}
type entry struct {
	mu      sync.Mutex
	app     model.App
	active  int
	drained chan struct{}
	// leases is shared by every active lease; breakLeases cancels it.
	leases      context.Context
	breakLeases context.CancelFunc
	generation  uint64
	wakeError   error
	retryAfter  time.Time
	op          *operation
	removed     bool
}

type Manager struct {
	directory     string
	projectPrefix string
	runtime       engine.Runtime
	store         *store.Store
	hostPaths     hostpaths.Policy
	volumes       *volumes.Store // nil without site-deploy volumes
	removeFiles   func(string) error
	mu            sync.RWMutex
	apps          map[string]*entry
	domains       map[string]*entry
	ctx           context.Context
	cancel        context.CancelFunc
	lifeMu        sync.Mutex
	closed        bool
	wg            sync.WaitGroup
	// imagesMu is held shared while a project starts, which may pull images,
	// and exclusively by image cleanup, so cleanup never removes an image that
	// a starting project is about to use.
	imagesMu sync.RWMutex
}

// Option configures optional manager features.
type Option func(*Manager)

// WithVolumes enables site-deploy volumes, which bundles mount with a key.
func WithVolumes(store *volumes.Store) Option { return func(m *Manager) { m.volumes = store } }

// New loads saved applications and reconciles them with Docker. hostPaths is
// the allowlist for bind mounts of host paths; a running application whose
// mounts it no longer allows is stopped.
func New(dataDir string, runtime engine.Runtime, hostPaths hostpaths.Policy, options ...Option) (*Manager, error) {
	if runtime == nil {
		return nil, errors.New("Docker runtime is required")
	}
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	legacy, err := store.HasLegacyLayout(dir)
	if err != nil {
		return nil, err
	}
	disk, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{directory: dir, runtime: runtime, store: disk, hostPaths: hostPaths, removeFiles: os.RemoveAll, apps: map[string]*entry{}, domains: map[string]*entry{}, ctx: ctx, cancel: cancel}
	for _, option := range options {
		option(m)
	}
	fail := func(err error) (*Manager, error) { cancel(); disk.Close(); return nil, err }
	if err := disk.CheckFormat(); err != nil {
		return fail(err)
	}
	m.projectPrefix, err = disk.ProjectPrefix(legacy)
	if err != nil {
		return fail(err)
	}
	for _, sub := range []string{"apps", "staging", "compose"} {
		if err := privateStorageDirectory(filepath.Join(dir, sub)); err != nil {
			return fail(err)
		}
	}
	apps, err := disk.Load()
	if err != nil {
		return fail(err)
	}
	for _, app := range apps {
		if app.Directory != filepath.Join(dir, "apps", app.Name) || app.Project != m.projectPrefix+app.Name {
			return fail(fmt.Errorf("state for %s refers to an unexpected project or directory", app.Name))
		}
		previousCompose := app.ComposeFile
		if err := m.prepareStoredCompose(&app); err != nil {
			return fail(fmt.Errorf("state for %s: %w", app.Name, err))
		}
		if _, exists := m.domains[app.Spec.Domain]; exists {
			return fail(fmt.Errorf("duplicate persisted domain %q", app.Spec.Domain))
		}
		if app.State == model.StateDeleting {
			// A durable deletion marker survives missing or partly removed bundle
			// files. Retry delete skips Docker, whose resources are already gone.
			e := &entry{app: app}
			m.apps[app.Name] = e
			m.domains[app.Spec.Domain] = e
			continue
		}
		probe, stop := context.WithTimeout(ctx, 15*time.Second)
		status, inspectErr := runtime.Inspect(probe, app)
		stop()
		app.Error = ""
		switch {
		case status.Partial:
			cleanupErr := m.stopPartial(app)
			if cleanupErr != nil {
				app.State = model.StateError
				app.Error = fmt.Sprintf("stop partially running project: %v", cleanupErr)
			} else {
				app.State = model.StateSuspended
				app.Upstream = ""
			}
		case inspectErr != nil:
			app.State = model.StateError
			app.Error = inspectErr.Error()
		case status.Running:
			app.State = model.StateRunning
			app.Upstream = status.Upstream
			if err := m.checkHostPaths(app); err != nil {
				app.State = model.StateError
				app.Error = err.Error()
				app.Upstream = ""
				if stopErr := m.stopPartial(app); stopErr != nil {
					app.Error += fmt.Sprintf("; stop: %v", stopErr)
				}
			}
		default:
			app.State = model.StateSuspended
			app.Upstream = ""
		}
		app.LastActivity = time.Now().UTC()
		app.UpdatedAt = app.LastActivity
		if err := disk.Save(app); err != nil {
			return fail(err)
		}
		if previousCompose != app.ComposeFile {
			// The record now names the protected copy. A container may still hold
			// this old inode; it is no longer used for lifecycle operations.
			_ = os.Remove(previousCompose)
		}
		e := &entry{app: app}
		m.apps[app.Name] = e
		m.domains[app.Spec.Domain] = e
	}
	return m, nil
}

func (m *Manager) launch(fn func()) bool {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	go func() { defer m.wg.Done(); fn() }()
	return true
}

func (m *Manager) Close() error {
	m.lifeMu.Lock()
	m.closed = true
	m.cancel()
	m.lifeMu.Unlock()
	m.wg.Wait()
	return m.store.Close()
}

func (m *Manager) lookup(name string) (*entry, error) {
	m.mu.RLock()
	e := m.apps[name]
	m.mu.RUnlock()
	if e == nil {
		return nil, ErrNotFound
	}
	return e, nil
}

func snapshot(e *entry) model.App { e.mu.Lock(); defer e.mu.Unlock(); return e.app }

func (m *Manager) List() []model.App {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.apps))
	for _, e := range m.apps {
		entries = append(entries, e)
	}
	m.mu.RUnlock()
	apps := make([]model.App, 0, len(entries))
	for _, e := range entries {
		apps = append(apps, snapshot(e))
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return apps
}

func (m *Manager) Get(name string) (model.App, error) {
	e, err := m.lookup(name)
	if err != nil {
		return model.App{}, err
	}
	return snapshot(e), nil
}

func (m *Manager) Domains() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	domains := make([]string, 0, len(m.domains))
	for host := range m.domains {
		domains = append(domains, host)
	}
	sort.Strings(domains)
	return domains
}

// ProjectPrefix is this data directory's Docker project namespace.
func (m *Manager) ProjectPrefix() string { return m.projectPrefix }

// ExclusiveImages runs fn only while no project is starting and reports
// whether it ran. Image cleanup uses it to avoid removing an image between a
// pull and the container that needs it.
func (m *Manager) ExclusiveImages(fn func()) bool {
	if !m.imagesMu.TryLock() {
		return false
	}
	defer m.imagesMu.Unlock()
	fn()
	return true
}

// ImageReferences maps every image named in a deployed application's
// generated Compose file to the applications using it. An unreadable file is
// an error, so cleanup never treats an image in use as unreferenced.
func (m *Manager) ImageReferences() (map[string][]string, error) {
	references := map[string][]string{}
	for _, app := range m.List() {
		data, err := os.ReadFile(app.ComposeFile)
		if err != nil {
			if app.State == model.StateDeleting && os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read Compose file of %s: %w", app.Name, err)
		}
		var document struct {
			Services map[string]struct {
				Image string `json:"image"`
			} `json:"services"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, fmt.Errorf("decode Compose file of %s: %w", app.Name, err)
		}
		for _, service := range document.Services {
			if service.Image != "" && !slices.Contains(references[service.Image], app.Name) {
				references[service.Image] = append(references[service.Image], app.Name)
			}
		}
	}
	return references, nil
}

// Sites returns every routed domain with its access control, for Caddy.
func (m *Manager) Sites() []model.Site {
	apps := m.List()
	sites := make([]model.Site, 0, len(apps))
	for _, app := range apps {
		sites = append(sites, model.Site{Domain: app.Spec.Domain, BasicAuth: app.Spec.BasicAuth})
	}
	return sites
}

// DeployOptions adjusts a deployment.
type DeployOptions struct {
	// CopyVolumesFrom names an application whose named volumes are copied into
	// the new application's volumes of the same names before its first start.
	CopyVolumesFrom string
}

// volumeSeed is a copy of named volumes that an initial start makes first.
type volumeSeed struct {
	source  *entry
	volumes []string
}

// volumeCopyTimeout bounds creating the new project and copying its volumes,
// separately from the application's start timeout.
const volumeCopyTimeout = 10 * time.Minute

func (m *Manager) Deploy(ctx context.Context, name, zipPath string) (model.App, error) {
	return m.DeployWith(ctx, name, zipPath, DeployOptions{})
}

func (m *Manager) DeployWith(ctx context.Context, name, zipPath string, opts DeployOptions) (model.App, error) {
	if !store.ValidName(name) {
		return model.App{}, fmt.Errorf("%w: name must start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens", ErrInvalid)
	}
	if err := m.ctx.Err(); err != nil {
		return model.App{}, err
	}
	if _, err := m.lookup(name); err == nil {
		return model.App{}, ErrConflict
	}
	staging, err := os.MkdirTemp(filepath.Join(m.directory, "staging"), name+"-")
	if err != nil {
		return model.App{}, err
	}
	defer os.RemoveAll(staging)
	if err := archive.Extract(zipPath, staging, archive.DefaultLimits()); err != nil {
		return model.App{}, fmt.Errorf("%w: extract ZIP: %v", ErrInvalid, err)
	}
	if _, err := os.Lstat(filepath.Join(staging, "compose.generated.json")); !errors.Is(err, os.ErrNotExist) {
		return model.App{}, fmt.Errorf("%w: compose.generated.json is reserved for generated deployment configuration", ErrInvalid)
	}
	checks := manifest.Checks{HostPath: func(source string, readOnly bool) (string, error) {
		return m.hostPaths.Check(name, source, readOnly)
	}}
	if m.volumes != nil {
		checks.Volume = func(volume string, snapshot bool) (string, error) {
			return m.volumes.Source(ctx, volume, name, snapshot)
		}
	}
	spec, generated, err := manifest.PrepareWith(staging, checks)
	if err != nil {
		return model.App{}, fmt.Errorf("%w: validate Compose: %v", ErrInvalid, err)
	}
	if err := ctx.Err(); err != nil {
		return model.App{}, err
	}
	if err := m.ctx.Err(); err != nil {
		return model.App{}, err
	}
	now := time.Now().UTC()
	dir := filepath.Join(m.directory, "apps", name)
	app := model.App{Name: name, Spec: spec, Project: m.projectPrefix + name, Directory: dir, ComposeFile: m.composePath(name), State: model.StateSuspended, CreatedAt: now, UpdatedAt: now, LastActivity: now}
	var seed *volumeSeed
	if opts.CopyVolumesFrom != "" {
		seed, err = m.planVolumeCopy(ctx, app, generated, opts.CopyVolumesFrom)
		if err != nil {
			return model.App{}, err
		}
	}
	e := &entry{app: app}
	m.mu.Lock()
	if m.apps[name] != nil || m.domains[spec.Domain] != nil {
		m.mu.Unlock()
		return model.App{}, ErrConflict
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		m.mu.Unlock()
		return model.App{}, fmt.Errorf("deployment directory %s already exists or cannot be inspected", dir)
	}
	if err := copyCompose(generated, app.ComposeFile); err != nil {
		m.mu.Unlock()
		return model.App{}, fmt.Errorf("protect generated Compose: %w", err)
	}
	if err := os.Remove(generated); err != nil {
		_ = os.Remove(app.ComposeFile)
		m.mu.Unlock()
		return model.App{}, err
	}
	if err := os.Rename(staging, dir); err != nil {
		_ = os.Remove(app.ComposeFile)
		m.mu.Unlock()
		return model.App{}, err
	}
	// Docker may run the image as an unprivileged UID. The mounted bundle root
	// must be traversable inside the container; its host parent stays private.
	if err := os.Chmod(dir, 0755); err != nil {
		os.RemoveAll(dir)
		_ = os.Remove(app.ComposeFile)
		m.mu.Unlock()
		return model.App{}, err
	}
	if err := m.store.Save(app); err != nil {
		os.RemoveAll(dir)
		_ = os.Remove(app.ComposeFile)
		m.mu.Unlock()
		return model.App{}, err
	}
	m.apps[name] = e
	m.domains[spec.Domain] = e
	e.mu.Lock()
	op, err := m.startLocked(e, true, seed)
	e.mu.Unlock()
	m.mu.Unlock()
	if err != nil {
		return snapshot(e), err
	}
	err = await(ctx, op)
	return snapshot(e), err
}

// planVolumeCopy checks that the new application target can copy the named
// volumes it shares with the application source, before anything is saved.
// generated is target's Compose file.
func (m *Manager) planVolumeCopy(ctx context.Context, target model.App, generated, source string) (*volumeSeed, error) {
	if source == target.Name {
		return nil, fmt.Errorf("%w: an application cannot copy its own volumes", ErrInvalid)
	}
	e, err := m.lookup(source)
	if err != nil {
		return nil, fmt.Errorf("%w: application %q, to copy volumes from, is not deployed", ErrInvalid, source)
	}
	data, err := os.ReadFile(generated)
	if err != nil {
		return nil, err
	}
	wanted, err := manifest.NamedVolumes(data)
	if err != nil {
		return nil, err
	}
	sourceApp := snapshot(e)
	data, err = os.ReadFile(sourceApp.ComposeFile)
	if err != nil {
		return nil, fmt.Errorf("read Compose file of %q: %w", source, err)
	}
	available, err := manifest.NamedVolumes(data)
	if err != nil {
		return nil, err
	}
	var volumes []string
	for _, volume := range wanted {
		if slices.Contains(available, volume) {
			volumes = append(volumes, volume)
		}
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("%w: %q has no named volumes with the same names as %q's", ErrInvalid, target.Name, source)
	}
	present, err := m.runtime.ExistingVolumes(ctx, sourceApp, volumes)
	if err != nil {
		return nil, err
	}
	if len(present) != len(volumes) {
		var missing []string
		for _, volume := range volumes {
			if !slices.Contains(present, sourceApp.Project+"_"+volume) {
				missing = append(missing, volume)
			}
		}
		return nil, fmt.Errorf("%w: %q's volume %s does not exist yet; start it once first", ErrInvalid, source, strings.Join(missing, ", "))
	}
	// A volume kept from an earlier deployment of the same name holds data.
	// Never replace it: whoever removes it decides that data is not needed.
	existing, err := m.runtime.ExistingVolumes(ctx, target, volumes)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return nil, fmt.Errorf("%w: volume %s already exists, kept from an earlier deployment of %q; remove it on the server (docker volume rm) to copy %q's instead", ErrConflict, strings.Join(existing, ", "), target.Name, source)
	}
	return &volumeSeed{source: e, volumes: volumes}, nil
}

// copyVolumes runs seed's copy into target. The source is held for the copy,
// so it is neither started, suspended, nor deleted meanwhile, and its requests
// wait until its containers resume. The caller must not hold imagesMu: an
// operation in progress on the source, which this waits for, may need it.
func (m *Manager) copyVolumes(target model.App, seed *volumeSeed) error {
	ctx, cancel := context.WithTimeout(m.ctx, volumeCopyTimeout)
	defer cancel()
	e := seed.source
	var op *operation
	var source model.App
	for op == nil {
		e.mu.Lock()
		if e.removed || e.app.State == model.StateDeleting {
			name := e.app.Name
			e.mu.Unlock()
			return fmt.Errorf("application %q, to copy volumes from, was deleted", name)
		}
		if busy := e.op; busy != nil {
			e.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-busy.done:
			}
			continue
		}
		op = &operation{done: make(chan struct{})}
		e.op = op
		source = e.app
		e.mu.Unlock()
	}
	m.imagesMu.RLock()
	err := m.runtime.CopyVolumes(ctx, source, target, seed.volumes)
	m.imagesMu.RUnlock()
	stuck := false
	if err != nil && source.State == model.StateRunning {
		// The copy may have left containers paused. The next request must then
		// reconcile the project, which unpauses them, rather than wait on it.
		checkCtx, cancelCheck := context.WithTimeout(m.ctx, 30*time.Second)
		status, inspectErr := m.runtime.Inspect(checkCtx, source)
		cancelCheck()
		stuck = inspectErr != nil || !status.Running
	}
	e.mu.Lock()
	if stuck {
		e.app.State = model.StateError
		e.app.Upstream = ""
		e.app.Error = fmt.Sprintf("not running after copying its volumes to %q", target.Name)
		e.app.UpdatedAt = time.Now().UTC()
		if saveErr := m.store.Save(e.app); saveErr != nil {
			slog.Warn("persist state", "app", source.Name, "error", saveErr)
		}
	}
	e.op = nil
	close(op.done)
	e.mu.Unlock()
	if err != nil {
		return fmt.Errorf("copy volumes from %q: %w", source.Name, err)
	}
	return nil
}

func await(ctx context.Context, op *operation) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-op.done:
		return op.err
	}
}

// Caller holds e.mu. Startup is owned by the daemon, so a cancelled HTTP
// request cannot abort startup needed by other waiting requests. An initial
// start copies seed's volumes, if any, first.
func (m *Manager) startLocked(e *entry, initial bool, seed *volumeSeed) (*operation, error) {
	op := &operation{done: make(chan struct{})}
	e.generation++
	e.wakeError = nil
	e.retryAfter = time.Time{}
	e.app.State = model.StateStarting
	e.app.Error = ""
	e.app.UpdatedAt = time.Now().UTC()
	if err := m.store.Save(e.app); err != nil {
		e.app.State = model.StateError
		e.app.Error = err.Error()
		return nil, err
	}
	e.op = op
	app := e.app
	if !m.launch(func() {
		// Nothing has started yet if a host path is refused, so there is
		// nothing to clean up. A failed copy cleans up after itself.
		err := m.checkHostPaths(app)
		if err == nil && initial && seed != nil {
			err = m.copyVolumes(app, seed)
		}
		m.imagesMu.RLock()
		ctx, cancel := context.WithTimeout(m.ctx, app.Spec.StartDuration())
		defer cancel()
		var upstream string
		if err == nil {
			if initial {
				upstream, err = m.runtime.Up(ctx, app)
			} else {
				upstream, err = m.runtime.Resume(ctx, app)
			}
			if err == nil {
				err = validUpstream(upstream)
			}
			if err != nil && m.ctx.Err() == nil {
				if cleanupErr := m.stopPartial(app); cleanupErr != nil {
					err = errors.Join(err, fmt.Errorf("stop partially started project: %w", cleanupErr))
				}
			}
		}
		m.imagesMu.RUnlock()
		e.mu.Lock()
		defer e.mu.Unlock()
		if err == nil {
			e.app.State = model.StateRunning
			e.app.Upstream = upstream
			e.app.LastActivity = time.Now().UTC()
		} else {
			e.app.State = model.StateError
			e.app.Error = err.Error()
			e.app.Upstream = ""
		}
		m.finishLocked(e, op, err)
		if op.err != nil {
			e.wakeError = op.err
			e.retryAfter = time.Now().Add(wakeFailureCooldown)
		}
	}) {
		e.op = nil
		e.app.State = model.StateError
		return nil, context.Canceled
	}
	return op, nil
}

// checkHostPaths checks the project's host-path bind mounts against the
// current allowlist, and its site-deploy volumes against the volumes that
// exist. Docker resolves them again at every container start, so a tree
// changed by a container since the deploy, an entry the operator has since
// removed, or a deleted volume or snapshot stops the start.
func (m *Manager) checkHostPaths(app model.App) error {
	data, err := os.ReadFile(app.ComposeFile)
	if err != nil {
		return fmt.Errorf("read Compose file: %w", err)
	}
	mounts, err := manifest.HostMounts(data)
	if err != nil {
		return err
	}
	for _, mount := range mounts {
		if m.volumes != nil && m.volumes.Contains(mount.Source) {
			if err := m.volumes.Check(app.Name, mount.Source); err != nil {
				return fmt.Errorf("service %s: %w", mount.Service, err)
			}
			continue
		}
		if _, err := m.hostPaths.Check(app.Name, mount.Source, mount.ReadOnly); err != nil {
			return fmt.Errorf("service %s: %w", mount.Service, err)
		}
	}
	return nil
}

func (m *Manager) stopPartial(app model.App) error {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	app.Spec.SuspendMode = "stop"
	return m.runtime.Suspend(ctx, app)
}

func validUpstream(upstream string) error {
	host, port, err := net.SplitHostPort(upstream)
	if err != nil || host != "127.0.0.1" || port == "" {
		return errors.New("Docker runtime returned an invalid loopback upstream")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("Docker runtime returned an invalid loopback port")
	}
	return nil
}

func (m *Manager) finishLocked(e *entry, op *operation, err error) {
	e.app.UpdatedAt = time.Now().UTC()
	if saveErr := m.store.Save(e.app); saveErr != nil {
		err = errors.Join(err, fmt.Errorf("persist state: %w", saveErr))
		if e.app.State != model.StateDeleting {
			e.app.State = model.StateError
		}
		e.app.Error = err.Error()
	}
	if err != nil {
		op.err = fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	e.op = nil
	close(op.done)
}

func (m *Manager) running(ctx context.Context, e *entry, lease bool) (*Lease, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := m.ctx.Err(); err != nil {
			return nil, err
		}
		e.mu.Lock()
		if e.removed {
			e.mu.Unlock()
			return nil, ErrNotFound
		}
		// Wait for any operation first, including a deletion in progress, so a
		// request during a redeploy consistently ends with the deletion.
		if e.op != nil {
			op := e.op
			e.mu.Unlock()
			if err := await(ctx, op); err != nil {
				return nil, err
			}
			continue
		}
		if e.app.State == model.StateDeleting {
			e.mu.Unlock()
			return nil, fmt.Errorf("%w: deletion is incomplete; retry delete", ErrUnavailable)
		}
		if e.app.State == model.StateRunning && e.app.Upstream != "" {
			upstream := e.app.Upstream
			generation := e.generation
			e.app.LastActivity = time.Now().UTC()
			var leaseContext context.Context
			if lease {
				if e.active == 0 {
					e.drained = make(chan struct{})
					e.leases, e.breakLeases = context.WithCancel(m.ctx)
				}
				e.active++
				leaseContext = e.leases
			}
			e.mu.Unlock()
			var once sync.Once
			released := false // accessed only while holding e.mu
			result := &Lease{Upstream: upstream, Context: leaseContext, Release: func() {
				if !lease {
					return
				}
				once.Do(func() {
					e.mu.Lock()
					defer e.mu.Unlock()
					released = true
					e.active--
					e.app.LastActivity = time.Now().UTC()
					if e.active == 0 {
						close(e.drained)
						e.drained = nil
						e.breakLeases()
						e.leases, e.breakLeases = nil, nil
					}
				})
			}}
			if lease {
				result.ReportFailure = func(cause error) {
					if cause == nil {
						return
					}
					e.mu.Lock()
					defer e.mu.Unlock()
					if released || e.removed || e.op != nil || e.generation != generation || e.app.State != model.StateRunning || e.app.Upstream != upstream {
						return
					}
					e.app.State = model.StateError
					e.app.Upstream = ""
					e.app.Error = "upstream connection failed: " + cause.Error()
					e.app.UpdatedAt = time.Now().UTC()
					e.wakeError = nil
					e.retryAfter = time.Time{}
					if err := m.store.Save(e.app); err != nil {
						slog.Warn("persist upstream failure", "app", e.app.Name, "error", err)
					}
				}
			}
			return result, nil
		}
		// A failed connection may coexist with an open stream. Never restart or
		// clean up its project while another request still owns a lease.
		if e.active > 0 {
			drained := e.drained
			e.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-m.ctx.Done():
				return nil, m.ctx.Err()
			case <-drained:
			}
			continue
		}
		if lease && e.wakeError != nil && time.Now().Before(e.retryAfter) {
			err := e.wakeError
			e.mu.Unlock()
			return nil, err
		}
		op, err := m.startLocked(e, false, nil)
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if err := await(ctx, op); err != nil {
			return nil, err
		}
	}
}

func (m *Manager) Acquire(ctx context.Context, host string) (*Lease, error) {
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	m.mu.RLock()
	e := m.domains[host]
	m.mu.RUnlock()
	if e == nil {
		return nil, ErrNotFound
	}
	return m.running(ctx, e, true)
}

func (m *Manager) Resume(ctx context.Context, name string) (model.App, error) {
	e, err := m.lookup(name)
	if err != nil {
		return model.App{}, err
	}
	// An explicit resume also repairs containers stopped outside the daemon.
	// Existing requests keep their lease; reconciliation waits for a later call.
	e.mu.Lock()
	if !e.removed && e.op == nil && e.active == 0 && e.app.State == model.StateRunning {
		op, startErr := m.startLocked(e, false, nil)
		e.mu.Unlock()
		if startErr != nil {
			return snapshot(e), startErr
		}
		return snapshotAfter(ctx, e, op)
	}
	e.mu.Unlock()
	_, err = m.running(ctx, e, false)
	return snapshot(e), err
}

func snapshotAfter(ctx context.Context, e *entry, op *operation) (model.App, error) {
	err := await(ctx, op)
	return snapshot(e), err
}

func (m *Manager) suspend(ctx context.Context, e *entry, idle bool) (model.App, error) {
	e.mu.Lock()
	if e.removed {
		e.mu.Unlock()
		return model.App{}, ErrNotFound
	}
	if e.app.State == model.StateDeleting {
		app := e.app
		e.mu.Unlock()
		return app, ErrBusy
	}
	if e.active > 0 || e.op != nil {
		e.mu.Unlock()
		return snapshot(e), ErrBusy
	}
	if e.app.State == model.StateSuspended {
		app := e.app
		e.mu.Unlock()
		return app, nil
	}
	if idle && (e.app.State != model.StateRunning || e.app.Spec.IdleDuration() <= 0 || time.Since(e.app.LastActivity) < e.app.Spec.IdleDuration()) {
		app := e.app
		e.mu.Unlock()
		return app, nil
	}
	op := &operation{done: make(chan struct{})}
	e.app.State = model.StateStopping
	e.app.Error = ""
	e.op = op
	app := e.app
	if !m.launch(func() {
		stopCtx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
		defer cancel()
		err := m.runtime.Suspend(stopCtx, app)
		e.mu.Lock()
		defer e.mu.Unlock()
		if err == nil {
			e.app.State = model.StateSuspended
			e.app.Upstream = ""
		} else {
			e.app.State = model.StateError
			e.app.Error = err.Error()
		}
		m.finishLocked(e, op, err)
	}) {
		e.op = nil
		e.app.State = model.StateError
		e.mu.Unlock()
		return snapshot(e), context.Canceled
	}
	e.mu.Unlock()
	err := await(ctx, op)
	return snapshot(e), err
}

func (m *Manager) Suspend(ctx context.Context, name string) (model.App, error) {
	e, err := m.lookup(name)
	if err != nil {
		return model.App{}, err
	}
	return m.suspend(ctx, e, false)
}

func (m *Manager) RunIdle(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			entries := make([]*entry, 0, len(m.apps))
			for _, e := range m.apps {
				entries = append(entries, e)
			}
			m.mu.RUnlock()
			for _, e := range entries {
				_, err := m.suspend(ctx, e, true)
				if err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrNotFound) && !errors.Is(err, context.Canceled) {
					slog.Warn("idle suspension failed", "app", snapshot(e).Name, "error", err)
				}
			}
		}
	}
}

func (m *Manager) Delete(ctx context.Context, name string) error {
	e, err := m.lookup(name)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.removed {
		e.mu.Unlock()
		return ErrNotFound
	}
	if e.op != nil {
		e.mu.Unlock()
		return ErrBusy
	}
	// Deletion is how an application is replaced, so open connections, such as
	// a browser tab's live-update WebSocket, must not block it: break them.
	// The operation set below makes new requests wait for the deletion.
	var drained chan struct{}
	if e.active > 0 {
		drained = e.drained
		e.breakLeases()
	}
	op := &operation{done: make(chan struct{})}
	e.op = op
	retryDeletion := e.app.State == model.StateDeleting
	if !retryDeletion {
		e.app.State = model.StateStopping
	}
	app := e.app
	if !m.launch(func() {
		if drained != nil {
			select {
			case <-drained:
			case <-time.After(leaseDrainTimeout):
			case <-m.ctx.Done():
			}
		}
		removeCtx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
		defer cancel()
		var err error
		if !retryDeletion {
			err = m.runtime.Down(removeCtx, app)
		}
		if err == nil {
			// Write the marker before removing any files. A later retry must not
			// need the Compose file that deletion may already have removed.
			e.mu.Lock()
			e.app.State = model.StateDeleting
			e.app.Error = ""
			e.app.UpdatedAt = time.Now().UTC()
			err = m.store.Save(e.app)
			e.mu.Unlock()
		}
		if err == nil {
			if removeErr := m.removeFiles(app.Directory); removeErr != nil {
				err = fmt.Errorf("remove deployment files (correct file ownership and retry delete): %w", removeErr)
			}
		}
		if err == nil {
			if removeErr := m.removeFiles(m.composePath(app.Name)); removeErr != nil {
				err = fmt.Errorf("remove protected Compose file (retry delete): %w", removeErr)
			} else if syncErr := syncStorageDirectory(filepath.Dir(m.composePath(app.Name))); syncErr != nil {
				err = fmt.Errorf("persist protected Compose removal (retry delete): %w", syncErr)
			}
		}
		if err == nil {
			err = m.store.Delete(app.Name)
		}
		if err != nil {
			e.mu.Lock()
			if e.app.State != model.StateDeleting {
				e.app.State = model.StateError
			}
			e.app.Error = err.Error()
			m.finishLocked(e, op, err)
			e.mu.Unlock()
			return
		}
		m.mu.Lock()
		e.mu.Lock()
		e.removed = true
		e.op = nil
		delete(m.apps, app.Name)
		delete(m.domains, app.Spec.Domain)
		close(op.done)
		e.mu.Unlock()
		m.mu.Unlock()
	}) {
		e.op = nil
		e.mu.Unlock()
		return context.Canceled
	}
	e.mu.Unlock()
	return await(ctx, op)
}

func (m *Manager) Logs(ctx context.Context, name string, tail int) (string, error) {
	app, err := m.Get(name)
	if err != nil {
		return "", err
	}
	if tail < 1 || tail > 10000 {
		return "", errors.New("tail must be between 1 and 10000")
	}
	logCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return m.runtime.Logs(logCtx, app, tail)
}
