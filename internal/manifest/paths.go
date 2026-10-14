package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// localPath forbids both lexical traversal and symlinks at every component.
// Paths are checked before Docker sees them, including paths in env/config files.
func (v *validator) localPath(value any, where string, fileOnly bool) (string, error) {
	name, err := literalString(value, where)
	if err != nil {
		return "", err
	}
	if err := relativePath(name, where); err != nil {
		return "", err
	}
	clean := filepath.Clean(name)
	current := v.root
	var info os.FileInfo
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err = os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("%s source %q must exist inside the ZIP root: %w", where, name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s source %q contains a symlink", where, name)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s source %q must be a regular file or directory", where, name)
		}
	}
	if fileOnly && !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s source %q must be a regular file", where, name)
	}
	if clean == "." {
		return ".", nil
	}
	return "./" + filepath.ToSlash(clean), nil
}

func relativePath(name, where string) error {
	if path.IsAbs(name) || filepath.IsAbs(name) || strings.HasPrefix(name, "~") || strings.ContainsAny(name, "\\:$\x00") {
		return fmt.Errorf("%s source must be a relative path inside the ZIP root", where)
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".." {
			return fmt.Errorf("%s source must stay inside the ZIP root; parent traversal is not supported", where)
		}
	}
	return nil
}

func (v *validator) fileList(value any, where string, longSyntax bool) ([]any, error) {
	var values []any
	switch value := value.(type) {
	case string:
		values = []any{value}
	case []any:
		values = value
	default:
		return nil, fmt.Errorf("%s must be a path or list of paths", where)
	}
	for i, value := range values {
		location := fmt.Sprintf("%s[%d]", where, i)
		if _, ok := value.(string); ok {
			name, err := v.localPath(value, location, true)
			if err != nil {
				return nil, err
			}
			values[i] = name
			continue
		}
		if !longSyntax {
			return nil, fmt.Errorf("%s must be a file path", location)
		}
		m, err := object(value, location)
		if err != nil {
			return nil, err
		}
		if err := allowedKeys(m, location, "path required format"); err != nil {
			return nil, err
		}
		name, err := v.localPath(m["path"], location+".path", true)
		if err != nil {
			return nil, err
		}
		if required, exists := m["required"]; exists {
			if _, ok := required.(bool); !ok {
				return nil, fmt.Errorf("%s.required must be a boolean", location)
			}
		}
		if format, exists := m["format"]; exists && format != "raw" {
			return nil, fmt.Errorf("%s.format only supports raw", location)
		}
		m["path"] = name
	}
	return values, nil
}

func (v *validator) mount(value any, where string) (map[string]any, error) {
	var m map[string]any
	var err error
	if text, ok := value.(string); ok {
		m, err = shortMount(text, where)
	} else {
		m, err = object(value, where)
	}
	if err != nil {
		return nil, err
	}
	if err := allowedKeys(m, where, "type source target read_only consistency bind volume tmpfs"); err != nil {
		return nil, err
	}
	target, err := literalString(m["target"], where+".target")
	if err != nil {
		return nil, err
	}
	if !path.IsAbs(target) || strings.Contains(target, "\\") {
		return nil, fmt.Errorf("%s.target must be an absolute container path", where)
	}
	if value, exists := m["read_only"]; exists {
		if _, ok := value.(bool); !ok {
			return nil, fmt.Errorf("%s.read_only must be a boolean", where)
		}
	}
	if consistency, exists := m["consistency"]; exists && consistency != "consistent" && consistency != "cached" && consistency != "delegated" {
		return nil, fmt.Errorf("%s.consistency must be consistent, cached, or delegated", where)
	}
	switch m["type"] {
	case "bind":
		if _, exists := m["volume"]; exists {
			return nil, fmt.Errorf("%s.volume cannot be used with a bind mount", where)
		}
		if _, exists := m["tmpfs"]; exists {
			return nil, fmt.Errorf("%s.tmpfs cannot be used with a bind mount", where)
		}
		source, err := v.bindSource(m, where)
		if err != nil {
			return nil, err
		}
		m["source"] = source
		bind := map[string]any{}
		if value, exists := m["bind"]; exists {
			bind, err = object(value, where+".bind")
			if err != nil {
				return nil, err
			}
			if err := allowedKeys(bind, where+".bind", "create_host_path propagation"); err != nil {
				return nil, err
			}
			if value, exists := bind["create_host_path"]; exists {
				if _, ok := value.(bool); !ok {
					return nil, fmt.Errorf("%s.bind.create_host_path must be a boolean", where)
				}
			}
			if propagation, exists := bind["propagation"]; exists && propagation != "rprivate" && propagation != "private" {
				return nil, fmt.Errorf("%s.bind.propagation may only be private or rprivate", where)
			}
		}
		bind["create_host_path"] = false
		m["bind"] = bind
	case "volume":
		if _, exists := m["bind"]; exists {
			return nil, fmt.Errorf("%s.bind cannot be used with a named volume", where)
		}
		if _, exists := m["tmpfs"]; exists {
			return nil, fmt.Errorf("%s.tmpfs cannot be used with a named volume", where)
		}
		if value, exists := m["source"]; exists {
			source, err := literalString(value, where+".source")
			if err != nil {
				return nil, err
			}
			if err := v.resourceExists("volumes", source, where); err != nil {
				return nil, err
			}
		}
		if value, exists := m["volume"]; exists {
			volume, err := object(value, where+".volume")
			if err != nil {
				return nil, err
			}
			if err := allowedKeys(volume, where+".volume", "nocopy subpath"); err != nil {
				return nil, err
			}
			if value, exists := volume["nocopy"]; exists {
				if _, ok := value.(bool); !ok {
					return nil, fmt.Errorf("%s.volume.nocopy must be a boolean", where)
				}
			}
			if value, exists := volume["subpath"]; exists {
				subpath, err := literalString(value, where+".volume.subpath")
				if err != nil {
					return nil, err
				}
				if err := relativePath(subpath, where+".volume.subpath"); err != nil {
					return nil, err
				}
			}
		}
	case "tmpfs":
		for _, key := range []string{"source", "bind", "volume"} {
			if _, exists := m[key]; exists {
				return nil, fmt.Errorf("%s.%s cannot be used with tmpfs", where, key)
			}
		}
		if value, exists := m["tmpfs"]; exists {
			tmpfs, err := object(value, where+".tmpfs")
			if err != nil {
				return nil, err
			}
			if err := allowedKeys(tmpfs, where+".tmpfs", "size mode"); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("%s.type must be bind, volume, or tmpfs", where)
	}
	return m, nil
}

// bindSource keeps bundle-relative sources inside the ZIP root and passes
// absolute host paths to the operator's allowlist.
func (v *validator) bindSource(m map[string]any, where string) (string, error) {
	source, err := literalString(m["source"], where+".source")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(source, "/") {
		return v.localPath(source, where+".source", false)
	}
	if v.hostPath == nil {
		return "", fmt.Errorf("%s.source: host path %s is not allowed; this server allows no host paths", where, source)
	}
	readOnly, _ := m["read_only"].(bool)
	source, err = v.hostPath(source, readOnly)
	if err != nil {
		return "", fmt.Errorf("%s.source: %w", where, err)
	}
	return source, nil
}

// HostMount is a bind mount of an absolute host path in a generated Compose file.
type HostMount struct {
	Service, Source string
	ReadOnly        bool
}

// HostMounts lists the host-path bind mounts in a Compose file written by
// Prepare, so they can be checked again before each start.
func HostMounts(data []byte) ([]HostMount, error) {
	var document struct {
		Services map[string]struct {
			Volumes []any `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode Compose file: %w", err)
	}
	var mounts []HostMount
	for name, service := range document.Services {
		for i, value := range service.Volumes {
			where := fmt.Sprintf("services.%s.volumes[%d]", name, i)
			var m map[string]any
			var err error
			if text, ok := value.(string); ok {
				m, err = shortMount(text, where)
			} else {
				m, err = object(value, where)
			}
			if err != nil {
				return nil, err
			}
			source, _ := m["source"].(string)
			if m["type"] != "bind" || !strings.HasPrefix(source, "/") {
				continue
			}
			readOnly, _ := m["read_only"].(bool)
			mounts = append(mounts, HostMount{Service: name, Source: source, ReadOnly: readOnly})
		}
	}
	sort.Slice(mounts, func(i, j int) bool {
		if mounts[i].Service != mounts[j].Service {
			return mounts[i].Service < mounts[j].Service
		}
		return mounts[i].Source < mounts[j].Source
	})
	return mounts, nil
}

func shortMount(text, where string) (map[string]any, error) {
	if strings.ContainsAny(text, "$\x00\\") {
		return nil, fmt.Errorf("%s must use explicit Unix paths; interpolation is not supported here", where)
	}
	parts := strings.Split(text, ":")
	if len(parts) > 3 || len(parts) == 0 {
		return nil, fmt.Errorf("%s must use source:target[:ro] mount syntax", where)
	}
	m := map[string]any{"type": "volume"}
	if len(parts) == 1 {
		m["target"] = parts[0]
		return m, nil
	}
	if parts[0] == "" {
		return nil, fmt.Errorf("%s.source must not be empty", where)
	}
	m["source"], m["target"] = parts[0], parts[1]
	if strings.HasPrefix(parts[0], ".") || strings.HasPrefix(parts[0], "/") || strings.HasPrefix(parts[0], "~") {
		m["type"] = "bind"
	}
	if len(parts) == 3 {
		for _, option := range strings.Split(parts[2], ",") {
			switch option {
			case "ro":
				m["read_only"] = true
			case "rw":
				m["read_only"] = false
			case "nocopy":
				if m["type"] != "volume" {
					return nil, fmt.Errorf("%s: nocopy only applies to volumes", where)
				}
				m["volume"] = map[string]any{"nocopy": true}
			default:
				return nil, fmt.Errorf("%s has unsupported mount option %q", where, option)
			}
		}
	}
	return m, nil
}
