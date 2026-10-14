package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"site-deploy/internal/model"
)

type validator struct {
	root     string
	document map[string]any
	spec     model.Spec
	services map[string]any
	hostPath HostPathCheck
}

func (v *validator) validate() error {
	var err error
	v.services, err = object(v.document["services"], "services")
	if err != nil {
		return err
	}
	if len(v.services) == 0 {
		return fmt.Errorf("services must contain at least one image-based service")
	}
	if _, exists := v.services[v.spec.Service]; !exists {
		return fmt.Errorf("x-site-deploy.service %q does not exist", v.spec.Service)
	}
	for _, kind := range []string{"volumes", "networks", "configs", "secrets"} {
		if err := v.resources(kind); err != nil {
			return err
		}
	}
	for name, raw := range v.services {
		if !resourceName.MatchString(name) {
			return fmt.Errorf("invalid service name %q", name)
		}
		svc, err := object(raw, "services."+name)
		if err != nil {
			return err
		}
		if err := v.service(name, svc); err != nil {
			return err
		}
		delete(svc, "ports")
		if name == v.spec.Service {
			svc["ports"] = []any{map[string]any{
				"target": v.spec.Port, "host_ip": "127.0.0.1", "protocol": "tcp",
				// Leaving published absent asks Docker for an ephemeral port.
			}}
		}
	}
	return nil
}

const serviceKeys = `image command entrypoint working_dir user group_add
 environment env_file label_file labels annotations hostname domainname
 ports expose volumes configs secrets networks network_mode depends_on links
 restart init tty stdin_open read_only stop_grace_period stop_signal
 cpus cpu_count cpu_percent cpu_period cpu_quota cpu_rt_runtime cpu_rt_period
 cpu_shares cpuset mem_limit mem_reservation memswap_limit mem_swappiness
 pids_limit oom_kill_disable oom_score_adj ulimits shm_size tmpfs
 healthcheck deploy scale pull_policy platform cap_drop security_opt privileged
 dns dns_search dns_opt extra_hosts mac_address logging attach`

func (v *validator) service(name string, svc map[string]any) error {
	where := "services." + name
	if err := allowedKeys(svc, where, strings.Join(strings.Fields(serviceKeys), " ")); err != nil {
		return err
	}
	image, err := literalString(svc["image"], where+".image")
	if err != nil {
		return err
	}
	if strings.ContainsAny(image, " \t\r\n") || strings.Contains(image, "://") {
		return fmt.Errorf("%s.image must be a Docker image reference", where)
	}
	if value, exists := svc["restart"]; exists {
		policy, err := restartPolicy(value, where+".restart")
		if err != nil {
			return err
		}
		svc["restart"] = policy
	}
	if value, exists := svc["privileged"]; exists {
		if flag, ok := value.(bool); !ok || flag {
			return fmt.Errorf("%s.privileged must be false; privileged containers are not supported", where)
		}
	}
	if value, exists := svc["security_opt"]; exists {
		options, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s.security_opt must be a list", where)
		}
		for _, option := range options {
			if option != "no-new-privileges" && option != "no-new-privileges:true" && option != "no-new-privileges=true" {
				return fmt.Errorf("%s.security_opt only supports no-new-privileges", where)
			}
		}
	}
	if value, exists := svc["network_mode"]; exists {
		if value != "none" || name == v.spec.Service {
			return fmt.Errorf("%s.network_mode only supports none on non-routed services; host/container namespaces are not supported", where)
		}
	}
	if policy, exists := svc["pull_policy"]; exists {
		policy, err := literalString(policy, where+".pull_policy")
		if err != nil {
			return err
		}
		switch policy {
		case "always", "never", "missing", "if_not_present", "daily", "weekly":
		default:
			return fmt.Errorf("%s.pull_policy must be always, never, missing, daily, or weekly; builds are not supported", where)
		}
	}
	for _, key := range []string{"platform", "user", "working_dir", "hostname", "domainname", "mac_address"} {
		if value, exists := svc[key]; exists {
			if _, err := literalString(value, where+"."+key); err != nil {
				return err
			}
		}
	}
	if value, exists := svc["environment"]; exists {
		if err := environment(value, where+".environment"); err != nil {
			return err
		}
	}
	for _, field := range []string{"env_file", "label_file"} {
		if value, exists := svc[field]; exists {
			prepared, err := v.fileList(value, where+"."+field, field == "env_file")
			if err != nil {
				return err
			}
			svc[field] = prepared
		}
	}
	if value, exists := svc["volumes"]; exists {
		mounts, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s.volumes must be a list", where)
		}
		for i, mount := range mounts {
			prepared, err := v.mount(mount, fmt.Sprintf("%s.volumes[%d]", where, i))
			if err != nil {
				return err
			}
			mounts[i] = prepared
		}
	}
	for _, kind := range []string{"configs", "secrets"} {
		if value, exists := svc[kind]; exists {
			if err := v.attachments(value, kind, where+"."+kind); err != nil {
				return err
			}
		}
	}
	if value, exists := svc["networks"]; exists {
		if err := v.networkAttachments(value, where+".networks"); err != nil {
			return err
		}
	}
	if value, exists := svc["depends_on"]; exists {
		if err := v.dependencies(value, where+".depends_on"); err != nil {
			return err
		}
	}
	if value, exists := svc["links"]; exists {
		links, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s.links must be a list", where)
		}
		for _, value := range links {
			link, err := literalString(value, where+".links")
			if err != nil {
				return err
			}
			if _, exists := v.services[strings.SplitN(link, ":", 2)[0]]; !exists {
				return fmt.Errorf("%s.links references an unknown service", where)
			}
		}
	}
	if value, exists := svc["healthcheck"]; exists {
		m, err := object(value, where+".healthcheck")
		if err != nil {
			return err
		}
		if err := allowedKeys(m, where+".healthcheck", "test interval timeout start_period start_interval retries disable"); err != nil {
			return err
		}
	}
	if value, exists := svc["deploy"]; exists {
		if err := deploy(value, where+".deploy", name == v.spec.Service); err != nil {
			return err
		}
	}
	if value, exists := svc["scale"]; exists && name == v.spec.Service {
		if value != 1 {
			return fmt.Errorf("%s.scale must be 1 for the routed service", where)
		}
	}
	if value, exists := svc["logging"]; exists {
		m, err := object(value, where+".logging")
		if err != nil {
			return err
		}
		if err := allowedKeys(m, where+".logging", "driver options"); err != nil {
			return err
		}
		if driver, exists := m["driver"]; exists {
			if driver != "local" && driver != "json-file" && driver != "journald" && driver != "none" {
				return fmt.Errorf("%s.logging.driver only supports local, json-file, journald, or none", where)
			}
		}
	}
	return nil
}

func restartPolicy(value any, where string) (string, error) {
	policy, err := literalString(value, where)
	if err != nil {
		return "", err
	}
	switch policy {
	case "always":
		// Explicitly stopped applications must stay stopped after Docker restarts.
		// unless-stopped retains crash recovery while respecting idle suspension.
		return "unless-stopped", nil
	case "no", "on-failure", "unless-stopped":
		return policy, nil
	}
	if maximum, ok := strings.CutPrefix(policy, "on-failure:"); ok && maximum != "" {
		digits := true
		for _, digit := range maximum {
			if digit < '0' || digit > '9' {
				digits = false
				break
			}
		}
		if _, err := strconv.Atoi(maximum); digits && err == nil {
			return policy, nil
		}
	}
	return "", fmt.Errorf("%s must be no, always, unless-stopped, or on-failure with an optional nonnegative retry count", where)
}

func environment(value any, where string) error {
	switch values := value.(type) {
	case map[string]any:
		for key, value := range values {
			if key == "" || strings.ContainsAny(key, "=$\x00") {
				return fmt.Errorf("%s has an invalid environment variable name", where)
			}
			switch value.(type) {
			case string, int, int64, uint64, float64:
			default:
				return fmt.Errorf("%s.%s must have an explicit string or number value; host environment inheritance is not supported", where, key)
			}
		}
	case []any:
		for _, value := range values {
			s, ok := value.(string)
			key, _, assigned := strings.Cut(s, "=")
			if !ok || !assigned || key == "" || strings.ContainsAny(key, "$\x00") {
				return fmt.Errorf("%s entries must use NAME=value; host environment inheritance is not supported", where)
			}
		}
	default:
		return fmt.Errorf("%s must be a mapping or list", where)
	}
	return nil
}

func deploy(value any, where string, routed bool) error {
	m, err := object(value, where)
	if err != nil {
		return err
	}
	if _, exists := m["restart_policy"]; exists {
		return fmt.Errorf("%s.restart_policy is not supported; use the service restart field so suspension survives Docker restarts", where)
	}
	if err := allowedKeys(m, where, "resources replicas labels mode"); err != nil {
		return err
	}
	if mode, exists := m["mode"]; exists && mode != "replicated" {
		return fmt.Errorf("%s.mode only supports replicated on a single host", where)
	}
	if value, exists := m["replicas"]; exists && routed && value != 1 {
		return fmt.Errorf("%s.replicas must be 1 for the routed service", where)
	}
	if value, exists := m["resources"]; exists {
		resources, err := object(value, where+".resources")
		if err != nil {
			return err
		}
		if err := allowedKeys(resources, where+".resources", "limits reservations"); err != nil {
			return err
		}
		for kind, value := range resources {
			if strings.HasPrefix(kind, "x-") {
				continue
			}
			limit, err := object(value, where+".resources."+kind)
			if err != nil {
				return err
			}
			keys := "cpus memory"
			if kind == "limits" {
				keys += " pids"
			}
			if err := allowedKeys(limit, where+".resources."+kind, keys); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *validator) dependencies(value any, where string) error {
	check := func(name string) error {
		if _, exists := v.services[name]; !exists {
			return fmt.Errorf("%s references unknown service %q", where, name)
		}
		return nil
	}
	switch values := value.(type) {
	case []any:
		for _, value := range values {
			name, err := literalString(value, where)
			if err != nil {
				return err
			}
			if err := check(name); err != nil {
				return err
			}
		}
	case map[string]any:
		for name, value := range values {
			if err := check(name); err != nil {
				return err
			}
			options, err := object(value, where+"."+name)
			if err != nil {
				return err
			}
			if err := allowedKeys(options, where+"."+name, "condition restart required"); err != nil {
				return err
			}
			if condition, exists := options["condition"]; exists {
				if condition != "service_started" && condition != "service_healthy" && condition != "service_completed_successfully" {
					return fmt.Errorf("%s.%s.condition is invalid", where, name)
				}
			}
		}
	default:
		return fmt.Errorf("%s must be a list or mapping", where)
	}
	return nil
}

func (v *validator) resources(kind string) error {
	value, exists := v.document[kind]
	if !exists {
		return nil
	}
	resources, err := object(value, kind)
	if err != nil {
		return err
	}
	for name, value := range resources {
		where := kind + "." + name
		if !resourceName.MatchString(name) {
			return fmt.Errorf("invalid %s resource name %q", kind, name)
		}
		if value == nil && (kind == "volumes" || kind == "networks") {
			resources[name] = map[string]any{}
			continue
		}
		m, err := object(value, where)
		if err != nil {
			return err
		}
		switch kind {
		case "volumes":
			if err := allowedKeys(m, where, "driver labels"); err != nil {
				return err
			}
			if driver, exists := m["driver"]; exists && driver != "local" {
				return fmt.Errorf("%s.driver must be local", where)
			}
		case "networks":
			if err := allowedKeys(m, where, "driver labels internal attachable enable_ipv4 enable_ipv6 ipam"); err != nil {
				return err
			}
			if driver, exists := m["driver"]; exists && driver != "bridge" {
				return fmt.Errorf("%s.driver must be bridge", where)
			}
			if value, exists := m["ipam"]; exists {
				if err := networkIPAM(value, where+".ipam"); err != nil {
					return err
				}
			}
		case "configs", "secrets":
			keys := "file"
			if kind == "configs" {
				keys += " content"
			}
			if err := allowedKeys(m, where, keys); err != nil {
				return err
			}
			if value, exists := m["file"]; exists {
				if _, exists := m["content"]; exists {
					return fmt.Errorf("%s must use either file or content", where)
				}
				file, err := v.localPath(value, where+".file", true)
				if err != nil {
					return err
				}
				m["file"] = file
			} else if _, ok := m["content"].(string); kind != "configs" || !ok {
				if kind == "configs" {
					return fmt.Errorf("%s must declare a local file or string content", where)
				}
				return fmt.Errorf("%s must declare a local file", where)
			}
		}
	}
	return nil
}

func networkIPAM(value any, where string) error {
	m, err := object(value, where)
	if err != nil {
		return err
	}
	if err := allowedKeys(m, where, "driver config"); err != nil {
		return err
	}
	if driver, exists := m["driver"]; exists && driver != "default" {
		return fmt.Errorf("%s.driver must be default", where)
	}
	if value, exists := m["config"]; exists {
		entries, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s.config must be a list", where)
		}
		for _, value := range entries {
			config, err := object(value, where+".config")
			if err != nil {
				return err
			}
			if err := allowedKeys(config, where+".config", "subnet ip_range gateway aux_addresses"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *validator) resourceExists(kind, name, where string) error {
	if !resourceName.MatchString(name) {
		return fmt.Errorf("%s has invalid %s source %q", where, kind, name)
	}
	resources, _ := v.document[kind].(map[string]any)
	if _, exists := resources[name]; !exists {
		if kind == "networks" && name == "default" {
			return nil
		}
		return fmt.Errorf("%s references undeclared %s resource %q", where, kind, name)
	}
	return nil
}

func (v *validator) attachments(value any, kind, where string) error {
	entries, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s must be a list", where)
	}
	for _, entry := range entries {
		if name, ok := entry.(string); ok {
			if err := v.resourceExists(kind, name, where); err != nil {
				return err
			}
			continue
		}
		m, err := object(entry, where)
		if err != nil {
			return err
		}
		if err := allowedKeys(m, where, "source target uid gid mode"); err != nil {
			return err
		}
		source, err := literalString(m["source"], where+".source")
		if err != nil {
			return err
		}
		if err := v.resourceExists(kind, source, where); err != nil {
			return err
		}
		if value, exists := m["target"]; exists {
			if _, err := literalString(value, where+".target"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *validator) networkAttachments(value any, where string) error {
	switch entries := value.(type) {
	case []any:
		for _, entry := range entries {
			name, err := literalString(entry, where)
			if err != nil {
				return err
			}
			if err := v.resourceExists("networks", name, where); err != nil {
				return err
			}
		}
	case map[string]any:
		for name, value := range entries {
			if err := v.resourceExists("networks", name, where); err != nil {
				return err
			}
			if value == nil {
				continue
			}
			options, err := object(value, where+"."+name)
			if err != nil {
				return err
			}
			if err := allowedKeys(options, where+"."+name, "aliases ipv4_address ipv6_address link_local_ips mac_address priority gw_priority interface_name"); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s must be a list or mapping", where)
	}
	return nil
}
