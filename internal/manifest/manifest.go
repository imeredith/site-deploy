// Package manifest validates an uploaded Compose project and writes the only
// Compose file the runtime is allowed to execute.
package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"site-deploy/internal/model"
)

const generatedName = "compose.generated.json"
const maxManifestBytes = 4 << 20

var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}
var resourceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// HostPathCheck approves a bind mount of an absolute host path and returns the
// form to write, or explains why it isn't allowed.
type HostPathCheck func(source string, readOnly bool) (string, error)

// Prepare validates the project without invoking Docker. All uploaded port
// publications are removed. The selected service receives one ephemeral TCP
// publication bound exclusively to the host's IPv4 loopback interface.
//
// Local file references remain relative so the caller can move a staging
// directory into place before starting the project. Bind mounts of absolute
// host paths need hostPath's approval; with a nil hostPath they're rejected.
func Prepare(directory string, hostPath HostPathCheck) (model.Spec, string, error) {
	return PrepareWith(directory, Checks{HostPath: hostPath})
}

// PrepareWith is Prepare with checks for everything a bundle may mount from
// outside itself: host paths, and site-deploy volumes from x-site-deploy.
func PrepareWith(directory string, checks Checks) (model.Spec, string, error) {
	var empty model.Spec
	directory, err := filepath.Abs(directory)
	if err != nil {
		return empty, "", fmt.Errorf("resolve deployment directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return empty, "", fmt.Errorf("deployment directory must be an existing directory, not a symlink")
	}
	var filename string
	for _, name := range composeNames {
		candidate := filepath.Join(directory, name)
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return empty, "", fmt.Errorf("inspect %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return empty, "", fmt.Errorf("%s must be a regular file", name)
		}
		if filename != "" {
			return empty, "", fmt.Errorf("ZIP root must contain exactly one Compose file; found %s and %s", filepath.Base(filename), name)
		}
		if info.Size() > maxManifestBytes {
			return empty, "", fmt.Errorf("%s exceeds the %d MiB manifest limit", name, maxManifestBytes>>20)
		}
		filename = candidate
	}
	if filename == "" {
		return empty, "", fmt.Errorf("ZIP root must contain exactly one of compose.yaml, compose.yml, docker-compose.yaml, docker-compose.yml")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return empty, "", fmt.Errorf("read Compose file: %w", err)
	}
	doc, err := decode(data)
	if err != nil {
		return empty, "", err
	}
	if err := allowedKeys(doc, "Compose", "version services volumes networks configs secrets x-site-deploy"); err != nil {
		return empty, "", err
	}
	spec, err := deploymentSpec(doc["x-site-deploy"])
	if err != nil {
		return empty, "", err
	}
	var mounts []volumeMount
	if value, exists := doc["x-site-deploy"].(map[string]any)["volumes"]; exists {
		if mounts, err = volumeMounts(value, spec.Service); err != nil {
			return empty, "", err
		}
	}
	v := validator{root: directory, document: doc, spec: spec, hostPath: checks.HostPath}
	if err := v.validate(); err != nil {
		return empty, "", err
	}
	// Last, so a volume is only snapshotted for an otherwise valid bundle.
	if err := v.mountVolumes(mounts, checks.Volume); err != nil {
		return empty, "", err
	}
	delete(doc, "x-site-deploy")
	delete(doc, "version")
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return empty, "", fmt.Errorf("encode generated Compose file: %w", err)
	}
	// Write via a private temporary file, then rename. Never follow an uploaded
	// symlink that happens to use the generated filename.
	tmp, err := os.CreateTemp(directory, ".compose-generated-*")
	if err != nil {
		return empty, "", fmt.Errorf("create generated Compose file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_, writeErr := tmp.Write(append(encoded, '\n'))
	closeErr := tmp.Close()
	if writeErr != nil {
		return empty, "", fmt.Errorf("write generated Compose file: %w", writeErr)
	}
	if closeErr != nil {
		return empty, "", fmt.Errorf("close generated Compose file: %w", closeErr)
	}
	generated := filepath.Join(directory, generatedName)
	if err := os.Rename(tmpName, generated); err != nil {
		return empty, "", fmt.Errorf("install generated Compose file: %w", err)
	}
	return spec, generated, nil
}

func decode(data []byte) (map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse Compose YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("Compose YAML must contain exactly one document")
		}
		return nil, fmt.Errorf("parse Compose YAML: %w", err)
	}
	// Compose-specific reset/override tags have merge semantics we deliberately
	// do not implement. Ordinary YAML anchors and merge keys remain supported.
	if err := checkTags(&document); err != nil {
		return nil, err
	}
	var result map[string]any
	if err := document.Decode(&result); err != nil {
		return nil, fmt.Errorf("parse Compose YAML: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("Compose YAML must be a nonempty mapping")
	}
	return result, nil
}

func checkTags(n *yaml.Node) error {
	if n.Tag != "" && !strings.HasPrefix(n.Tag, "!!") {
		return fmt.Errorf("unsupported YAML tag %q at line %d", n.Tag, n.Line)
	}
	for _, child := range n.Content {
		if err := checkTags(child); err != nil {
			return err
		}
	}
	return nil
}

func deploymentSpec(value any) (model.Spec, error) {
	spec := model.Spec{IdleTimeout: "5m", StartTimeout: "60s", SuspendMode: "stop"}
	m, err := object(value, "x-site-deploy")
	if err != nil {
		return spec, err
	}
	if err := allowedKeys(m, "x-site-deploy", "domain service port idle_timeout start_timeout suspend_mode basic_auth volumes"); err != nil {
		return spec, err
	}
	for name, target := range map[string]*string{
		"domain": &spec.Domain, "service": &spec.Service,
		"idle_timeout": &spec.IdleTimeout, "start_timeout": &spec.StartTimeout,
		"suspend_mode": &spec.SuspendMode,
	} {
		if value, ok := m[name]; ok {
			text, ok := value.(string)
			// Unquoted idle_timeout: 0 is a useful, unambiguous shorthand.
			if name == "idle_timeout" && value == 0 {
				text, ok = "0", true
			}
			if !ok {
				return spec, fmt.Errorf("x-site-deploy.%s must be a string", name)
			}
			*target = text
		}
	}
	spec.Domain = strings.ToLower(strings.TrimSuffix(spec.Domain, "."))
	if err := validDomain(spec.Domain); err != nil {
		return spec, err
	}
	if !resourceName.MatchString(spec.Service) {
		return spec, fmt.Errorf("x-site-deploy.service must name a service in the Compose file")
	}
	port, ok := m["port"].(int)
	if !ok || port < 1 || port > 65535 {
		return spec, fmt.Errorf("x-site-deploy.port must be an integer from 1 to 65535")
	}
	spec.Port = port
	idle, err := time.ParseDuration(spec.IdleTimeout)
	if err != nil || idle < 0 {
		return spec, fmt.Errorf("x-site-deploy.idle_timeout must be a nonnegative duration (for example 5m, or 0 to disable suspension)")
	}
	start, err := time.ParseDuration(spec.StartTimeout)
	if err != nil || start <= 0 {
		return spec, fmt.Errorf("x-site-deploy.start_timeout must be a positive duration")
	}
	if spec.SuspendMode != "stop" && spec.SuspendMode != "pause" {
		return spec, fmt.Errorf("x-site-deploy.suspend_mode must be stop or pause")
	}
	if value, ok := m["basic_auth"]; ok {
		if spec.BasicAuth, err = basicAuth(value); err != nil {
			return spec, err
		}
	}
	return spec, nil
}

// basicAuth reads a list of {user, hash} accounts. x-site-deploy is removed
// before Compose sees the file, so hashes are taken literally: no $$ escaping.
func basicAuth(value any) ([]model.BasicAuthUser, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, fmt.Errorf("x-site-deploy.basic_auth must be a nonempty list of {user, hash} accounts; omit it for a public app")
	}
	if len(items) > model.MaxBasicAuthUsers {
		return nil, fmt.Errorf("x-site-deploy.basic_auth allows at most %d accounts", model.MaxBasicAuthUsers)
	}
	users := make([]model.BasicAuthUser, 0, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		where := fmt.Sprintf("x-site-deploy.basic_auth[%d]", i)
		m, err := object(item, where)
		if err != nil {
			return nil, err
		}
		if err := allowedKeys(m, where, "user hash"); err != nil {
			return nil, err
		}
		user, userOK := m["user"].(string)
		hash, hashOK := m["hash"].(string)
		if !userOK || !hashOK {
			return nil, fmt.Errorf("%s needs string user and hash fields", where)
		}
		account := model.BasicAuthUser{User: user, Hash: hash}
		if err := account.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if seen[user] {
			return nil, fmt.Errorf("%s: duplicate user %q", where, user)
		}
		seen[user] = true
		users = append(users, account)
	}
	return users, nil
}

func validDomain(domain string) error {
	if domain == "" || len(domain) > 253 || net.ParseIP(domain) != nil {
		return fmt.Errorf("x-site-deploy.domain must be a DNS hostname without a scheme, port, path, or wildcard")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) > 63 || !domainLabel.MatchString(label) {
			return fmt.Errorf("x-site-deploy.domain must be a DNS hostname without a scheme, port, path, or wildcard")
		}
	}
	return nil
}

func object(value any, location string) (map[string]any, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a mapping", location)
	}
	return m, nil
}

func allowedKeys(m map[string]any, location, allowed string) error {
	keys := make(map[string]bool)
	for _, key := range strings.Fields(allowed) {
		keys[key] = true
	}
	for key := range m {
		if strings.HasPrefix(key, "x-") {
			continue
		}
		if !keys[key] {
			return fmt.Errorf("%s.%s is not supported in uploaded deployments", location, key)
		}
	}
	return nil
}

func literalString(value any, location string) (string, error) {
	s, ok := value.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("%s must be a nonempty string", location)
	}
	if strings.ContainsAny(s, "$\x00") {
		return "", fmt.Errorf("%s must use an explicit value; interpolation is not supported here", location)
	}
	return s, nil
}
