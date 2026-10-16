package manifest

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"site-deploy/internal/store"
)

// VolumeCheck returns the host path that mounts the site-deploy volume name:
// the volume itself, or with snapshot, the application's own snapshot of it.
type VolumeCheck func(name string, snapshot bool) (string, error)

// Checks approve what a bundle mounts from outside itself. A nil check refuses
// every such mount.
type Checks struct {
	HostPath HostPathCheck
	Volume   VolumeCheck
}

// volumeMount is one entry of x-site-deploy.volumes.
type volumeMount struct {
	name, service, target string
	readOnly, snapshot    bool
}

// volumeMounts reads x-site-deploy.volumes, a mapping from volume names to
// mounts, in name order. service is the default service to mount into.
func volumeMounts(value any, service string) ([]volumeMount, error) {
	m, err := object(value, "x-site-deploy.volumes")
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("x-site-deploy.volumes must map volume names to mounts; omit it to mount none")
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	mounts := make([]volumeMount, 0, len(names))
	for _, name := range names {
		where := "x-site-deploy.volumes." + name
		if !store.ValidName(name) {
			return nil, fmt.Errorf("%s: volume names start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens", where)
		}
		entry, err := object(m[name], where)
		if err != nil {
			return nil, err
		}
		if _, exists := entry["key"]; exists {
			return nil, fmt.Errorf("%s.key is no longer used: volumes are mounted by name since site-deploy 0.10.0; remove it", where)
		}
		if err := allowedKeys(entry, where, "target read_only snapshot service"); err != nil {
			return nil, err
		}
		mount := volumeMount{name: name, service: service}
		if mount.target, err = literalString(entry["target"], where+".target"); err != nil {
			return nil, err
		}
		if !path.IsAbs(mount.target) || strings.ContainsAny(mount.target, "\\:") {
			return nil, fmt.Errorf("%s.target must be an absolute container path", where)
		}
		mount.target = path.Clean(mount.target)
		if value, exists := entry["service"]; exists {
			if mount.service, err = literalString(value, where+".service"); err != nil {
				return nil, err
			}
		}
		for key, field := range map[string]*bool{"read_only": &mount.readOnly, "snapshot": &mount.snapshot} {
			if value, exists := entry[key]; exists {
				flag, ok := value.(bool)
				if !ok {
					return nil, fmt.Errorf("%s.%s must be a boolean", where, key)
				}
				*field = flag
			}
		}
		mounts = append(mounts, mount)
	}
	return mounts, nil
}

// mountVolumes adds each site-deploy volume to its service as a bind mount of
// the path that check approves. Every mount is validated before any check,
// since a check may create a snapshot.
func (v *validator) mountVolumes(mounts []volumeMount, check VolumeCheck) error {
	if len(mounts) == 0 {
		return nil
	}
	taken := map[string]bool{}
	for name, raw := range v.services {
		list, _ := raw.(map[string]any)["volumes"].([]any)
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				if target, ok := m["target"].(string); ok {
					taken[name+" "+path.Clean(target)] = true
				}
			}
		}
	}
	for _, mount := range mounts {
		where := "x-site-deploy.volumes." + mount.name
		if _, exists := v.services[mount.service]; !exists {
			return fmt.Errorf("%s.service %q does not exist", where, mount.service)
		}
		if taken[mount.service+" "+mount.target] {
			return fmt.Errorf("%s.target %s is already mounted in service %s", where, mount.target, mount.service)
		}
		taken[mount.service+" "+mount.target] = true
	}
	if check == nil {
		return fmt.Errorf("x-site-deploy.volumes: this server has no site-deploy volumes; its operator enables them with serve --volumes-dir")
	}
	for _, mount := range mounts {
		source, err := check(mount.name, mount.snapshot)
		if err != nil {
			return fmt.Errorf("x-site-deploy.volumes.%s: %w", mount.name, err)
		}
		svc := v.services[mount.service].(map[string]any)
		list, _ := svc["volumes"].([]any)
		svc["volumes"] = append(list, map[string]any{
			"type": "bind", "source": source, "target": mount.target, "read_only": mount.readOnly,
			"bind": map[string]any{"create_host_path": false},
		})
	}
	return nil
}
