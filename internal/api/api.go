// Package api exposes the authenticated deployment control interface.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"site-deploy/internal/caddy"
	"site-deploy/internal/images"
	"site-deploy/internal/model"
	"site-deploy/internal/orchestrator"
	"site-deploy/internal/store"
	"site-deploy/internal/version"
	"site-deploy/internal/volumes"
)

const (
	maxUploadBytes = 100 << 20
	syncTimeout    = 15 * time.Second
	deleteTimeout  = 70 * time.Second
	// volumeDeleteTimeout bounds deleting a volume or snapshot, which may have
	// to remove every file in it.
	volumeDeleteTimeout = 10 * time.Minute
	warningHeader       = "X-Site-Deploy-Warning"
)

type handler struct {
	manager   *orchestrator.Manager
	token     string
	tokenHash [sha256.Size]byte
	tempDir   string
	onChange  func(context.Context) error
	images    Images
}

// Images is the private registry and image cleanup, as served by the API.
type Images interface {
	// Address is the registry's host:port, or "" without a registry.
	Address() string
	List(context.Context) ([]images.Image, error)
	Cleanup(context.Context) images.Report
	// Trigger schedules a cleanup soon, without waiting for it.
	Trigger()
}

// Option configures optional API features.
type Option func(*handler)

// WithImages enables the registry and cleanup endpoints.
func WithImages(service Images) Option { return func(h *handler) { h.images = service } }

// New returns a control API handler. tempDir must be an existing private
// directory. An empty token disables authenticated endpoints, not authentication.
func New(manager *orchestrator.Manager, token, tempDir string, onChange func(context.Context) error, options ...Option) http.Handler {
	h := &handler{manager: manager, token: token, tokenHash: sha256.Sum256([]byte(token)), tempDir: tempDir, onChange: onChange}
	for _, option := range options {
		option(h)
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := r.URL.Path
	if path == "/healthz" && r.URL.EscapedPath() == path {
		if !h.method(w, r, http.MethodGet) {
			return
		}
		h.json(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !strings.HasPrefix(path, "/v1/") {
		h.error(w, http.StatusNotFound, errors.New("endpoint not found"))
		return
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="site-deploy"`)
		h.error(w, http.StatusUnauthorized, errors.New("authentication required"))
		return
	}
	w.Header().Set(version.Header, version.Version)
	// No control endpoint needs URL escapes. In particular, never let an
	// escaped slash or application name select a different resource.
	if r.URL.EscapedPath() != path {
		h.error(w, http.StatusBadRequest, errors.New("escaped API paths are not allowed"))
		return
	}
	// Any CLI may ask for the daemon version so it can explain a mismatch. Every
	// other endpoint refuses older CLIs before acting, so an outdated client can
	// never apply a deployment with semantics it does not understand.
	if path == "/v1/version" {
		if h.method(w, r, http.MethodGet) {
			h.json(w, http.StatusOK, map[string]string{"version": version.Version})
		}
		return
	}
	if err := version.ClientSupported(r.Header.Get(version.ClientHeader), version.Version); err != nil {
		h.error(w, http.StatusUpgradeRequired, err)
		return
	}
	switch path {
	case "/v1/apps":
		if h.method(w, r, http.MethodGet) {
			apps := h.manager.List()
			for i := range apps {
				apps[i] = withoutSecrets(apps[i])
				apps[i].Error = h.redact(apps[i].Error)
			}
			h.json(w, http.StatusOK, apps)
		}
		return
	case "/v1/caddy":
		if h.method(w, r, http.MethodGet) {
			h.caddy(w, r)
		}
		return
	case "/v1/registry":
		if h.method(w, r, http.MethodGet) {
			h.registry(w, r)
		}
		return
	case "/v1/cleanup":
		if h.method(w, r, http.MethodPost) {
			if h.images == nil {
				h.error(w, http.StatusNotFound, errors.New("image cleanup is not enabled on this daemon (serve --registry or --prune-images)"))
				return
			}
			h.json(w, http.StatusOK, h.images.Cleanup(r.Context()))
		}
		return
	case "/v1/volumes":
		if h.method(w, r, http.MethodGet) {
			list, err := h.manager.ListVolumes()
			if err != nil {
				h.error(w, errorStatus(err), err)
				return
			}
			h.json(w, http.StatusOK, list)
		}
		return
	}
	if rest, found := strings.CutPrefix(path, "/v1/volumes/"); found {
		h.volume(w, r, strings.Split(rest, "/"))
		return
	}
	if !strings.HasPrefix(path, "/v1/apps/") {
		h.error(w, http.StatusNotFound, errors.New("endpoint not found"))
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v1/apps/"), "/")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
		h.error(w, http.StatusNotFound, errors.New("endpoint not found"))
		return
	}
	name := parts[0]
	if !store.ValidName(name) {
		h.error(w, http.StatusBadRequest, errors.New("name must start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens"))
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			app, err := h.manager.Get(name)
			h.app(w, http.StatusOK, app, err)
		case http.MethodPost:
			h.deploy(w, r, name)
		case http.MethodDelete:
			h.delete(w, r, name)
		default:
			h.method(w, r, http.MethodGet, http.MethodPost, http.MethodDelete)
		}
		return
	}
	switch parts[1] {
	case "suspend":
		if h.method(w, r, http.MethodPost) {
			app, err := h.manager.Suspend(r.Context(), name)
			h.app(w, http.StatusOK, app, err)
		}
	case "resume":
		if h.method(w, r, http.MethodPost) {
			app, err := h.manager.Resume(r.Context(), name)
			h.app(w, http.StatusOK, app, err)
		}
	case "logs":
		if h.method(w, r, http.MethodGet) {
			h.logs(w, r, name)
		}
	default:
		h.error(w, http.StatusNotFound, errors.New("endpoint not found"))
	}
}

func (h *handler) authorized(r *http.Request) bool {
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 || h.token == "" {
		return false
	}
	fields := strings.Fields(headers[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return false
	}
	// Hash both values first so comparison always processes a fixed length.
	provided := sha256.Sum256([]byte(fields[1]))
	return subtle.ConstantTimeCompare(h.tokenHash[:], provided[:]) == 1
}

func (h *handler) method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, method := range allowed {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	h.error(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	return false
}

func (h *handler) deploy(w http.ResponseWriter, r *http.Request, name string) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/zip" {
		h.error(w, http.StatusUnsupportedMediaType, errors.New("deployment body must use Content-Type: application/zip"))
		return
	}
	values, err := query(r, "copy_volumes_from")
	if err != nil {
		h.error(w, http.StatusBadRequest, err)
		return
	}
	opts := orchestrator.DeployOptions{CopyVolumesFrom: values.Get("copy_volumes_from")}
	if values.Has("copy_volumes_from") && !store.ValidName(opts.CopyVolumesFrom) {
		h.error(w, http.StatusBadRequest, errors.New("copy_volumes_from must be an application name"))
		return
	}
	zipPath, status, err := receiveZIP(w, r, h.tempDir, maxUploadBytes)
	if err != nil {
		h.error(w, status, err)
		return
	}
	defer os.Remove(zipPath)
	app, err := h.manager.DeployWith(r.Context(), name, zipPath, opts)
	if app.Name != "" {
		// Startup may fail or the uploader may disconnect after the app has
		// been saved. Its domain still needs to route to the wakeup gateway.
		h.sync(w)
	}
	if err != nil {
		if app.Name != "" {
			err = fmt.Errorf("deployment %q is saved; inspect it, then resume or delete it: %w", app.Name, err)
		}
		h.error(w, errorStatus(err), err)
		return
	}
	w.Header().Set("Location", "/v1/apps/"+name)
	h.app(w, http.StatusCreated, app, nil)
}

// receiveZIP streams the request to disk and returns only a closed, complete
// temporary file. The caller owns removal on success; failures always clean up.
func receiveZIP(w http.ResponseWriter, r *http.Request, directory string, limit int64) (path string, status int, err error) {
	if r.ContentLength > limit {
		return "", http.StatusRequestEntityTooLarge, fmt.Errorf("ZIP upload exceeds the %d MiB limit", limit>>20)
	}
	file, err := os.CreateTemp(directory, "site-deploy-upload-*.zip")
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("create ZIP upload: %w", err)
	}
	defer func() {
		file.Close()
		if err != nil {
			os.Remove(file.Name())
		}
	}()
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	if _, err = io.Copy(file, body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return "", http.StatusRequestEntityTooLarge, fmt.Errorf("ZIP upload exceeds the %d MiB limit", limit>>20)
		}
		var diskError *os.PathError
		if errors.As(err, &diskError) {
			return "", http.StatusInternalServerError, fmt.Errorf("write ZIP upload: %w", err)
		}
		return "", http.StatusBadRequest, fmt.Errorf("read ZIP upload: %w", err)
	}
	if err = file.Close(); err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("close ZIP upload: %w", err)
	}
	return file.Name(), http.StatusOK, nil
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request, name string) {
	if err := r.Context().Err(); err != nil {
		h.error(w, errorStatus(err), err)
		return
	}
	// Manager deletion already continues after a caller disconnects. Keep
	// waiting for that result so route synchronization observes the removal.
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	if err := h.manager.Delete(ctx, name); err != nil {
		h.error(w, errorStatus(err), err)
		return
	}
	h.sync(w)
	if h.images != nil {
		h.images.Trigger()
	}
	h.json(w, http.StatusOK, map[string]string{"deleted": name})
}

// volume serves /v1/volumes/NAME (POST creates, DELETE deletes),
// /v1/volumes/NAME/rekey (POST), and /v1/volumes/NAME/snapshots/APP (DELETE).
func (h *handler) volume(w http.ResponseWriter, r *http.Request, parts []string) {
	if _, err := query(r); err != nil {
		h.error(w, http.StatusBadRequest, err)
		return
	}
	name := parts[0]
	if !store.ValidName(name) {
		h.error(w, http.StatusBadRequest, errors.New("volume name must start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens"))
		return
	}
	// Deletion may take a while, and stopping halfway helps nobody: finish it
	// even if the caller disconnects.
	deleteContext := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(r.Context()), volumeDeleteTimeout)
	}
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodPost:
			volume, key, err := h.manager.CreateVolume(r.Context(), name)
			if err != nil {
				h.error(w, errorStatus(err), err)
				return
			}
			w.Header().Set("Location", "/v1/volumes/"+name)
			h.json(w, http.StatusCreated, struct {
				volumes.Volume
				Key string `json:"key"`
			}{volume, key})
		case http.MethodDelete:
			ctx, cancel := deleteContext()
			defer cancel()
			if err := h.manager.DeleteVolume(ctx, name); err != nil {
				h.error(w, errorStatus(err), err)
				return
			}
			h.json(w, http.StatusOK, map[string]string{"deleted": name})
		default:
			h.method(w, r, http.MethodPost, http.MethodDelete)
		}
	case len(parts) == 2 && parts[1] == "rekey":
		if h.method(w, r, http.MethodPost) {
			key, err := h.manager.RekeyVolume(name)
			if err != nil {
				h.error(w, errorStatus(err), err)
				return
			}
			h.json(w, http.StatusOK, map[string]string{"name": name, "key": key})
		}
	case len(parts) == 3 && parts[1] == "snapshots" && parts[2] != "":
		app := parts[2]
		if !store.ValidName(app) {
			h.error(w, http.StatusBadRequest, errors.New("application name must start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens"))
			return
		}
		if h.method(w, r, http.MethodDelete) {
			ctx, cancel := deleteContext()
			defer cancel()
			if err := h.manager.DeleteVolumeSnapshot(ctx, name, app); err != nil {
				h.error(w, errorStatus(err), err)
				return
			}
			h.json(w, http.StatusOK, map[string]string{"volume": name, "snapshot": app})
		}
	default:
		h.error(w, http.StatusNotFound, errors.New("endpoint not found"))
	}
}

func (h *handler) registry(w http.ResponseWriter, r *http.Request) {
	if h.images == nil || h.images.Address() == "" {
		h.error(w, http.StatusNotFound, errors.New("this daemon has no private registry (serve --registry)"))
		return
	}
	list, err := h.images.List(r.Context())
	if err != nil {
		h.error(w, http.StatusBadGateway, err)
		return
	}
	if list == nil {
		list = []images.Image{}
	}
	h.json(w, http.StatusOK, map[string]any{"address": h.images.Address(), "images": list})
}

func (h *handler) sync(w http.ResponseWriter) {
	if h.onChange == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	if err := h.onChange(ctx); err != nil {
		warning := "Caddy synchronization failed: " + h.redact(err.Error())
		warning = strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return ' '
			}
			return r
		}, warning)
		if len(warning) > 1024 {
			warning = warning[:1024]
		}
		w.Header().Set(warningHeader, warning)
	}
}

func query(r *http.Request, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errors.New("invalid query string")
	}
	for key, items := range values {
		found := false
		for _, option := range allowed {
			if option == key {
				found = true
				break
			}
		}
		if !found || len(items) != 1 {
			return nil, fmt.Errorf("unknown or repeated query parameter %q", key)
		}
	}
	return values, nil
}

func (h *handler) logs(w http.ResponseWriter, r *http.Request, name string) {
	values, err := query(r, "tail")
	if err != nil {
		h.error(w, http.StatusBadRequest, err)
		return
	}
	tail := 100
	if values.Has("tail") {
		tail, err = strconv.Atoi(values.Get("tail"))
		if err != nil || tail < 1 || tail > 10000 {
			h.error(w, http.StatusBadRequest, errors.New("tail must be between 1 and 10000"))
			return
		}
	}
	logs, err := h.manager.Logs(r.Context(), name, tail)
	if err != nil {
		h.error(w, errorStatus(err), err)
		return
	}
	h.json(w, http.StatusOK, map[string]string{"logs": h.redact(logs)})
}

func (h *handler) caddy(w http.ResponseWriter, r *http.Request) {
	values, err := query(r, "gateway", "http")
	if err != nil {
		h.error(w, http.StatusBadRequest, err)
		return
	}
	gateway := "127.0.0.1:9080"
	if values.Has("gateway") {
		gateway = values.Get("gateway")
	}
	mode := caddy.HTTPS
	if values.Has("http") {
		httpOnly, err := strconv.ParseBool(values.Get("http"))
		if err != nil {
			h.error(w, http.StatusBadRequest, errors.New("http must be true or false"))
			return
		}
		if httpOnly {
			mode = caddy.HTTP
		}
	}
	config, err := caddy.Config(h.manager.Sites(), gateway, mode)
	if err != nil {
		h.error(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(config)
}

func (h *handler) app(w http.ResponseWriter, status int, app model.App, err error) {
	if err != nil {
		h.error(w, errorStatus(err), err)
		return
	}
	app = withoutSecrets(app)
	app.Error = h.redact(app.Error)
	h.json(w, status, app)
}

// withoutSecrets drops basic auth password hashes from API output. Usernames
// stay visible so operators can see who has access. The slice is copied: app
// is a snapshot, but its BasicAuth still shares the manager's backing array.
func withoutSecrets(app model.App) model.App {
	if len(app.Spec.BasicAuth) == 0 {
		return app
	}
	users := make([]model.BasicAuthUser, len(app.Spec.BasicAuth))
	for i, account := range app.Spec.BasicAuth {
		users[i] = model.BasicAuthUser{User: account.User}
	}
	app.Spec.BasicAuth = users
	return app
}

func errorStatus(err error) int {
	switch {
	case errors.Is(err, orchestrator.ErrNotFound), errors.Is(err, orchestrator.ErrNoVolumes), errors.Is(err, volumes.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, orchestrator.ErrConflict), errors.Is(err, orchestrator.ErrBusy), errors.Is(err, volumes.ErrExists), errors.Is(err, volumes.ErrInUse):
		return http.StatusConflict
	case errors.Is(err, orchestrator.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, orchestrator.ErrInvalid), errors.Is(err, volumes.ErrKey):
		return http.StatusBadRequest
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout
	default:
		return http.StatusInternalServerError
	}
}

func (h *handler) redact(value string) string {
	if h.token != "" {
		return strings.ReplaceAll(value, h.token, "[redacted]")
	}
	return value
}

func (h *handler) error(w http.ResponseWriter, status int, err error) {
	h.json(w, status, map[string]string{"error": h.redact(err.Error())})
}

func (h *handler) json(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
