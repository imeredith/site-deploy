// Package hostpaths holds the operator's allowlist of host directories that
// applications may bind-mount (serve --host-path) and checks mounts against it.
//
// Mounts are checked when a bundle is deployed and again before every start,
// because a container with write access to an allowed tree can change it
// between starts, and because the operator can change the allowlist.
package hostpaths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"site-deploy/internal/store"
)

// Rule allows bind mounts of Path or of anything beneath it.
type Rule struct {
	Path     string   // absolute and clean
	Writable bool     // read-write mounts are allowed; otherwise only read-only ones
	Apps     []string // applications the rule applies to; empty means every application
}

// Policy is the whole allowlist. A nil Policy allows no host paths.
type Policy []Rule

// Parse reads one --host-path value: [APP[,APP...]=]PATH[:ro|:rw].
func Parse(value string) (Rule, error) {
	var rule Rule
	path := value
	if !strings.HasPrefix(value, "/") {
		apps, rest, found := strings.Cut(value, "=")
		if !found {
			return rule, fmt.Errorf("host path %q must be absolute, optionally after APP[,APP...]=", value)
		}
		for _, app := range strings.Split(apps, ",") {
			if !store.ValidName(app) {
				return rule, fmt.Errorf("host path %q: %q is not a valid application name", value, app)
			}
			if !slices.Contains(rule.Apps, app) {
				rule.Apps = append(rule.Apps, app)
			}
		}
		path = rest
	}
	if trimmed, found := strings.CutSuffix(path, ":rw"); found {
		path, rule.Writable = trimmed, true
	} else {
		path = strings.TrimSuffix(path, ":ro")
	}
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\\:$\x00\n") || filepath.Clean(path) != path {
		return rule, fmt.Errorf("host path %q must be a clean absolute path without ':', '$' or '\\', optionally followed by :ro or :rw", value)
	}
	rule.Path = path
	return rule, nil
}

// Validate checks the allowlist against the filesystem when the daemon starts.
// Every path must be an existing directory named without symlinks, and none may
// expose the daemon's data directory or the directory holding the Docker socket.
func (p Policy) Validate(dataDir, dockerSocket string) error {
	data, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	socketDir := ""
	if dockerSocket != "" {
		// A missing socket is reported by Docker itself; only an existing one can leak.
		socketDir, _ = filepath.EvalSymlinks(filepath.Dir(dockerSocket))
	}
	for _, rule := range p {
		info, err := os.Stat(rule.Path)
		if err != nil {
			return fmt.Errorf("host path %s: %w", rule.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("host path %s must be a directory", rule.Path)
		}
		resolved, err := filepath.EvalSymlinks(rule.Path)
		if err != nil {
			return fmt.Errorf("host path %s: %w", rule.Path, err)
		}
		if resolved != rule.Path {
			return fmt.Errorf("host path %s goes through a symlink; use %s", rule.Path, resolved)
		}
		if within(rule.Path, data) || within(data, rule.Path) {
			return fmt.Errorf("host path %s overlaps the data directory %s", rule.Path, data)
		}
		if socketDir != "" && within(rule.Path, socketDir) {
			return fmt.Errorf("host path %s contains the Docker socket %s", rule.Path, dockerSocket)
		}
	}
	return nil
}

// Overlapping returns a rule whose path contains path or lies inside it.
func (p Policy) Overlapping(path string) (Rule, bool) {
	for _, rule := range p {
		if within(rule.Path, path) || within(path, rule.Path) {
			return rule, true
		}
	}
	return Rule{}, false
}

// Check returns the clean form of source if app may bind-mount it, read-only
// or not. The path must exist without a symlink in any component and must be a
// regular file or directory, so it can't name a device or socket.
func (p Policy) Check(app, source string, readOnly bool) (string, error) {
	if !filepath.IsAbs(source) || strings.ContainsAny(source, "\\:$\x00\n") {
		return "", fmt.Errorf("host path %q must be an absolute path without ':', '$' or '\\'", source)
	}
	if slices.Contains(strings.Split(source, "/"), "..") {
		return "", fmt.Errorf("host path %q must not contain '..'", source)
	}
	clean := filepath.Clean(source)
	allowed, writable := false, false
	for _, rule := range p {
		if within(rule.Path, clean) && (len(rule.Apps) == 0 || slices.Contains(rule.Apps, app)) {
			allowed, writable = true, writable || rule.Writable
		}
	}
	if !allowed {
		return "", fmt.Errorf("host path %s is not allowed for %s (%s)", clean, app, p.describe(app))
	}
	if !readOnly && !writable {
		return "", fmt.Errorf("host path %s may only be mounted read-only; add :ro or read_only: true", clean)
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return "", fmt.Errorf("host path %s: %w", clean, err)
	}
	if resolved != clean {
		return "", fmt.Errorf("host path %s goes through a symlink", clean)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return "", fmt.Errorf("host path %s: %w", clean, err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", fmt.Errorf("host path %s must be a regular file or directory", clean)
	}
	return clean, nil
}

// String describes a rule for logs, in the --host-path syntax.
func (r Rule) String() string {
	s := r.Path + ":ro"
	if r.Writable {
		s = r.Path + ":rw"
	}
	if len(r.Apps) > 0 {
		s = strings.Join(r.Apps, ",") + "=" + s
	}
	return s
}

func (p Policy) describe(app string) string {
	var paths []string
	for _, rule := range p {
		if len(rule.Apps) == 0 || slices.Contains(rule.Apps, app) {
			mode := "read-only"
			if rule.Writable {
				mode = "read-write"
			}
			paths = append(paths, rule.Path+" "+mode)
		}
	}
	if len(paths) == 0 {
		return "this server allows it no host paths"
	}
	return "allowed: " + strings.Join(paths, ", ")
}

// within reports whether path is root or beneath it.
func within(root, path string) bool {
	return path == root || root == "/" || strings.HasPrefix(path, root+"/")
}
