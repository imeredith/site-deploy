package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"site-deploy/internal/btrfs"
)

// Subvolumes performs site-deploy volume operations in a btrfs directory. It
// runs this executable's btrfs helper as root in a container that mounts only
// that directory, because subvolumes hold files that containers wrote as other
// users: the daemon's own user can't snapshot a subvolume it doesn't own, nor
// delete one without CAP_SYS_ADMIN. The helper container runs the copy image,
// which needs nothing but to exist, since the executable is static.
type Subvolumes struct {
	docker     *Docker
	root       string
	executable string
}

// NewSubvolumes manages subvolumes in root with the helper in executable,
// which must be a static Linux build of this program.
func NewSubvolumes(docker *Docker, root, executable string) (*Subvolumes, error) {
	for _, path := range []string{root, executable} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, ",\"\n") {
			return nil, fmt.Errorf("%q must be a clean absolute path without commas or quotes", path)
		}
	}
	return &Subvolumes{docker: docker, root: root, executable: executable}, nil
}

func (s *Subvolumes) Create(ctx context.Context, path string) error {
	return s.run(ctx, "create", path)
}

func (s *Subvolumes) Snapshot(ctx context.Context, source, target string) error {
	return s.run(ctx, "snapshot", source, target)
}

func (s *Subvolumes) Delete(ctx context.Context, path string) error {
	return s.run(ctx, "delete", path)
}

// IsSubvolume checks locally: reading needs no privileges.
func (s *Subvolumes) IsSubvolume(path string) (bool, error) { return btrfs.IsSubvolume(path) }

func (s *Subvolumes) run(ctx context.Context, operation string, paths ...string) error {
	args := []string{"run", "--rm", "--network", "none", "--user", "0:0", "--entrypoint", "/site-deploy",
		"--mount", "type=bind,source=" + s.executable + ",target=/site-deploy,readonly",
		"--mount", "type=bind,source=" + s.root + ",target=" + btrfs.HelperRoot,
		s.docker.CopyImage, btrfs.HelperCommand, operation}
	for _, path := range paths {
		relative, err := filepath.Rel(s.root, path)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
			return fmt.Errorf("%s is outside the volumes directory %s", path, s.root)
		}
		args = append(args, btrfs.HelperRoot+"/"+filepath.ToSlash(relative))
	}
	if err := s.docker.ensureImage(ctx, s.docker.CopyImage); err != nil {
		return fmt.Errorf("pull %s for the btrfs helper: %w", s.docker.CopyImage, err)
	}
	if _, err := s.docker.Command(ctx, args...); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf("btrfs %s: %w", operation, err)
	}
	return nil
}
