// Package images runs the private image registry that bundles pull from and
// removes images that no deployed application uses.
package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Docker runs plain docker CLI commands; engine.Docker implements it.
type Docker interface {
	Command(ctx context.Context, args ...string) ([]byte, error)
}

const (
	containerLabel = "site-deploy.registry"
	storagePath    = "/var/lib/registry"
	configPath     = "/etc/distribution/config.yml"
	maxListBytes   = 4 << 20
)

var (
	// Docker reference grammar, restricted to lowercase repository components.
	repositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagPattern        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	linkPattern       = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)
)

// ValidRepository reports whether name is a repository this registry accepts.
func ValidRepository(name string) bool {
	return len(name) <= 255 && repositoryPattern.MatchString(name)
}

// ValidTag reports whether tag is a Docker image tag.
func ValidTag(tag string) bool { return tagPattern.MatchString(tag) }

// Registry is a registry:3 container published on a loopback address. Docker
// allows plain HTTP to loopback registries, so both the host and an operator's
// SSH-forwarded Docker can push and pull without TLS. It runs as the daemon's
// own user so the daemon can read push times from its storage.
type Registry struct {
	address string
	image   string
	name    string
	dataDir string
	user    string
	docker  Docker
	client  *http.Client
	// mu serializes cleanup, including the garbage collection that briefly
	// stops the container.
	mu     sync.Mutex
	lastGC time.Time
}

// NewRegistry describes the registry container. Nothing runs until Ensure.
func NewRegistry(docker Docker, address, image, name, dataDir string) (*Registry, error) {
	host, port, err := net.SplitHostPort(address)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() || port == "" {
		return nil, errors.New("registry address must be a literal loopback IP and port, such as 127.0.0.1:9082")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, errors.New("registry port must be between 1 and 65535")
	}
	if image == "" || strings.ContainsAny(image, " \t\r\n") {
		return nil, errors.New("registry image must be a Docker image reference")
	}
	if !filepath.IsAbs(dataDir) {
		return nil, errors.New("registry data directory must be absolute")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Registry{
		address: address, image: image, name: name, dataDir: dataDir,
		user:   fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		docker: docker,
		client: &http.Client{Transport: transport, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		lastGC: time.Now(),
	}, nil
}

// Address is the host:port that image references must start with.
func (r *Registry) Address() string { return r.address }

// spec identifies the container configuration, so Ensure replaces a container
// left behind by a different image, address, storage, or user.
func (r *Registry) spec() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{r.image, r.address, r.dataDir, r.user}, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// Ensure creates, replaces, or starts the registry container as needed, then
// waits until it answers. Stored images survive replacement.
func (r *Registry) Ensure(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureLocked(ctx)
}

func (r *Registry) ensureLocked(ctx context.Context) error {
	if err := os.MkdirAll(r.dataDir, 0700); err != nil {
		return fmt.Errorf("create registry storage: %w", err)
	}
	if err := os.Chmod(r.dataDir, 0700); err != nil {
		return fmt.Errorf("restrict registry storage: %w", err)
	}
	output, err := r.docker.Command(ctx, "container", "inspect", "--format",
		`{{index .Config.Labels "`+containerLabel+`"}} {{.State.Running}}`, r.name)
	switch {
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container"):
		err = r.create(ctx)
	case err != nil:
		return fmt.Errorf("inspect registry container: %w", err)
	default:
		label, running, _ := strings.Cut(strings.TrimSpace(string(output)), " ")
		if label != r.spec() {
			if _, err = r.docker.Command(ctx, "rm", "--force", r.name); err == nil {
				err = r.create(ctx)
			}
		} else if running != "true" {
			_, err = r.docker.Command(ctx, "start", r.name)
		}
	}
	if err != nil {
		return fmt.Errorf("start registry container: %w", err)
	}
	return r.waitReady(ctx)
}

func (r *Registry) create(ctx context.Context) error {
	_, err := r.docker.Command(ctx, "run", "--detach", "--name", r.name,
		"--label", containerLabel+"="+r.spec(), "--restart", "unless-stopped",
		"--user", r.user, "--publish", r.address+":5000",
		"--volume", r.dataDir+":"+storagePath, "--env", "REGISTRY_LOG_LEVEL=info",
		r.image)
	return err
}

func (r *Registry) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+r.address+"/v2/", nil)
		if err != nil {
			return err
		}
		response, err := r.client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("registry at %s did not become ready: %w", r.address, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Image is one tag stored in the registry.
type Image struct {
	Reference  string    `json:"reference"`
	Repository string    `json:"repository"`
	Tag        string    `json:"tag"`
	Digest     string    `json:"digest"`
	Pushed     time.Time `json:"pushed,omitzero"`
	UsedBy     []string  `json:"used_by,omitempty"`
}

// List returns every stored tag. references maps image references from
// deployed Compose files to the applications that use them.
func (r *Registry) List(ctx context.Context, references map[string][]string) ([]Image, error) {
	repositories, err := r.paginate(ctx, "/v2/_catalog?n=100", "repositories")
	if err != nil {
		return nil, fmt.Errorf("list registry repositories: %w", err)
	}
	tagUsers, digestUsers := r.localReferences(references)
	var images []Image
	for _, repository := range repositories {
		if !ValidRepository(repository) {
			return nil, fmt.Errorf("registry returned an invalid repository name %q", repository)
		}
		tags, err := r.paginate(ctx, "/v2/"+repository+"/tags/list?n=100", "tags")
		if err != nil {
			return nil, fmt.Errorf("list tags of %s: %w", repository, err)
		}
		for _, tag := range tags {
			if !ValidTag(tag) {
				return nil, fmt.Errorf("registry returned an invalid tag %q for %s", tag, repository)
			}
			digest, err := r.digest(ctx, repository, tag)
			if err != nil {
				return nil, err
			}
			image := Image{
				Reference: r.address + "/" + repository + ":" + tag, Repository: repository, Tag: tag,
				Digest: digest, Pushed: r.pushed(repository, tag),
			}
			image.UsedBy = union(tagUsers[repository+":"+tag], digestUsers[repository+"@"+digest])
			images = append(images, image)
		}
	}
	return images, nil
}

// localReferences keeps references into this registry, keyed by repo:tag or
// repo@digest. A reference without a tag means :latest, as in Docker.
func (r *Registry) localReferences(references map[string][]string) (tags, digests map[string][]string) {
	tags, digests = map[string][]string{}, map[string][]string{}
	for reference, apps := range references {
		name, ok := strings.CutPrefix(reference, r.address+"/")
		if !ok {
			continue
		}
		if repository, digest, found := strings.Cut(name, "@"); found {
			repository, _, _ = strings.Cut(repository, ":")
			digests[repository+"@"+digest] = union(digests[repository+"@"+digest], apps)
			continue
		}
		repository, tag := name, "latest"
		if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
			repository, tag = name[:i], name[i+1:]
		}
		tags[repository+":"+tag] = union(tags[repository+":"+tag], apps)
	}
	return tags, digests
}

// pushed reads when a tag last moved from the registry's storage. A zero time
// means unknown, which cleanup treats as recent.
func (r *Registry) pushed(repository, tag string) time.Time {
	link := filepath.Join(r.dataDir, "docker/registry/v2/repositories", repository, "_manifests/tags", tag, "current/link")
	info, err := os.Stat(link)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

// paginate follows the registry's Link headers and collects one list field.
func (r *Registry) paginate(ctx context.Context, path, field string) ([]string, error) {
	var all []string
	for path != "" {
		response, err := r.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, maxListBytes+1))
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusNotFound && field == "tags" {
			return nil, nil // repository emptied by deletion
		}
		if response.StatusCode != http.StatusOK || len(data) > maxListBytes {
			return nil, fmt.Errorf("registry returned %s for %s", response.Status, path)
		}
		var page map[string]json.RawMessage
		var items []string
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("decode registry list: %w", err)
		}
		if raw, ok := page[field]; ok && string(raw) != "null" {
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, fmt.Errorf("decode registry %s: %w", field, err)
			}
		}
		all = append(all, items...)
		path = ""
		if match := linkPattern.FindStringSubmatch(response.Header.Get("Link")); match != nil {
			next, err := url.Parse(match[1])
			if err != nil || next.IsAbs() || !strings.HasPrefix(next.Path, "/v2/") {
				return nil, errors.New("registry returned an invalid pagination link")
			}
			path = next.RequestURI()
		}
	}
	return all, nil
}

var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// digest resolves a tag to the digest of the manifest or index it names.
func (r *Registry) digest(ctx context.Context, repository, tag string) (string, error) {
	response, err := r.do(ctx, http.MethodHead, "/v2/"+repository+"/manifests/"+tag, map[string]string{"Accept": manifestTypes})
	if err != nil {
		return "", err
	}
	response.Body.Close()
	digest := response.Header.Get("Docker-Content-Digest")
	if response.StatusCode != http.StatusOK || !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("resolve %s:%s: registry returned %s", repository, tag, response.Status)
	}
	return digest, nil
}

func (r *Registry) deleteManifest(ctx context.Context, repository, digest string) error {
	response, err := r.do(ctx, http.MethodDelete, "/v2/"+repository+"/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete %s@%s: registry returned %s", repository, digest, response.Status)
	}
	return nil
}

func (r *Registry) do(ctx context.Context, method, path string, headers map[string]string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://"+r.address+path, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("registry request: %w", err)
	}
	return response, nil
}

// Prune deletes every manifest whose tags are all unused and older than grace,
// then garbage-collects blobs. Collection also runs daily to drop manifests
// orphaned by re-pushed tags. It returns the removed repo:tag names.
func (r *Registry) Prune(ctx context.Context, references map[string][]string, grace time.Duration, now time.Time) (deleted []string, collected bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	images, err := r.List(ctx, references)
	if err != nil {
		return nil, false, err
	}
	for _, manifest := range unused(images, now.Add(-grace)) {
		if err := r.deleteManifest(ctx, manifest.Repository, manifest.Digest); err != nil {
			return deleted, false, err
		}
		for _, tag := range manifest.Tags {
			deleted = append(deleted, manifest.Repository+":"+tag)
		}
	}
	if len(deleted) == 0 && now.Sub(r.lastGC) < 24*time.Hour {
		return deleted, false, nil
	}
	if err := r.collect(ctx); err != nil {
		return deleted, false, err
	}
	r.lastGC = now
	return deleted, true, nil
}

// collect runs offline garbage collection: the registry must not accept pushes
// while it marks and sweeps, so the container is stopped and always restarted.
func (r *Registry) collect(ctx context.Context) (err error) {
	if _, err := r.docker.Command(ctx, "stop", "--time", "10", r.name); err != nil {
		return fmt.Errorf("stop registry for garbage collection: %w", err)
	}
	defer func() {
		restart, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		if startErr := r.ensureLocked(restart); startErr != nil {
			err = errors.Join(err, startErr)
		}
	}()
	_, err = r.docker.Command(ctx, "run", "--rm", "--user", r.user,
		"--volume", r.dataDir+":"+storagePath, r.image,
		"garbage-collect", "--delete-untagged", configPath)
	if err != nil {
		return fmt.Errorf("registry garbage collection: %w", err)
	}
	return nil
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{a, b} {
		for _, item := range list {
			if !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	return out
}
