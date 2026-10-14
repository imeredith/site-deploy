package images

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Manifest is one stored manifest and every tag that names it.
type Manifest struct {
	Repository string
	Digest     string
	Tags       []string
}

// unused groups tags by manifest, because deleting a manifest removes every tag
// that names it. A manifest is deleted only if none of its tags is used by an
// application or was pushed after cutoff; an unknown push time counts as recent.
func unused(images []Image, cutoff time.Time) []Manifest {
	type key struct{ repository, digest string }
	groups := map[key]*Manifest{}
	keep := map[key]bool{}
	var order []key
	for _, image := range images {
		k := key{image.Repository, image.Digest}
		if groups[k] == nil {
			groups[k] = &Manifest{Repository: image.Repository, Digest: image.Digest}
			order = append(order, k)
		}
		groups[k].Tags = append(groups[k].Tags, image.Tag)
		if len(image.UsedBy) > 0 || image.Pushed.IsZero() || image.Pushed.After(cutoff) {
			keep[k] = true
		}
	}
	var result []Manifest
	for _, k := range order {
		if !keep[k] {
			sort.Strings(groups[k].Tags)
			result = append(result, *groups[k])
		}
	}
	return result
}

// Report describes one cleanup run.
type Report struct {
	Skipped          string   `json:"skipped,omitempty"`
	Deleted          []string `json:"deleted,omitempty"`
	GarbageCollected bool     `json:"garbage_collected"`
	Pruned           []string `json:"pruned,omitempty"`
	Reclaimed        string   `json:"reclaimed,omitempty"`
	Errors           []string `json:"errors,omitempty"`
}

// Service exposes the registry and image cleanup to the API and daemon.
type Service struct {
	// Registry is nil when the daemon runs without a private registry.
	Registry *Registry
	Docker   Docker
	// PruneDocker removes host images that no container uses. Enable it only
	// on a host whose Docker is dedicated to site-deploy.
	PruneDocker bool
	Grace       time.Duration
	// References maps image references in deployed Compose files to the
	// applications using them.
	References func() (map[string][]string, error)
	// Exclusive runs fn only while no application is starting, so cleanup
	// never removes an image a starting project is about to use. It reports
	// false without running fn when a start is in progress.
	Exclusive   func(fn func()) bool
	triggerOnce sync.Once
	trigger     chan struct{}
}

func (s *Service) triggers() chan struct{} {
	s.triggerOnce.Do(func() { s.trigger = make(chan struct{}, 1) })
	return s.trigger
}

// Address is the registry's host:port, or "" without a registry.
func (s *Service) Address() string {
	if s.Registry == nil {
		return ""
	}
	return s.Registry.Address()
}

// List returns the registry's tags and the applications using each.
func (s *Service) List(ctx context.Context) ([]Image, error) {
	if s.Registry == nil {
		return nil, errors.New("this daemon has no private registry")
	}
	references, err := s.References()
	if err != nil {
		return nil, err
	}
	return s.Registry.List(ctx, references)
}

// Cleanup removes unused registry tags, garbage-collects the registry, and
// prunes unused Docker images, skipping the run while an application starts.
func (s *Service) Cleanup(ctx context.Context) Report {
	var report Report
	ran := s.Exclusive(func() {
		if s.Registry != nil {
			references, err := s.References()
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("read deployed image references: %v", err))
			} else {
				report.Deleted, report.GarbageCollected, err = s.Registry.Prune(ctx, references, s.Grace, time.Now())
				if err != nil {
					report.Errors = append(report.Errors, err.Error())
				}
			}
		}
		if s.PruneDocker {
			var err error
			report.Pruned, report.Reclaimed, err = pruneDocker(ctx, s.Docker)
			if err != nil {
				report.Errors = append(report.Errors, err.Error())
			}
		}
	})
	if !ran {
		report.Skipped = "an application is starting; cleanup will run again later"
	}
	return report
}

// Trigger asks Run to clean up soon, for example after a deletion freed images.
func (s *Service) Trigger() {
	select {
	case s.triggers() <- struct{}{}:
	default:
	}
}

// Run cleans up every interval and when triggered, until ctx ends. It first
// keeps trying to start the registry, which must not block serving apps.
func (s *Service) Run(ctx context.Context, interval time.Duration, log *slog.Logger) {
	trigger := s.triggers()
	if s.Registry != nil {
		for {
			err := s.Registry.Ensure(ctx)
			if err == nil {
				log.Info("private registry is ready", "address", s.Registry.Address())
				break
			}
			log.Error("private registry unavailable; retrying", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-trigger:
		}
		report := s.Cleanup(ctx)
		if len(report.Deleted)+len(report.Pruned)+len(report.Errors) > 0 || report.GarbageCollected {
			log.Info("image cleanup", "deleted", report.Deleted, "garbage_collected", report.GarbageCollected,
				"pruned", len(report.Pruned), "reclaimed", report.Reclaimed, "errors", report.Errors)
		}
	}
}

// pruneDocker removes every host image that no container, running or stopped,
// uses. Suspended applications keep their stopped containers, so their images
// stay; anything removed can be pulled again from its registry.
func pruneDocker(ctx context.Context, docker Docker) (pruned []string, reclaimed string, err error) {
	output, err := docker.Command(ctx, "image", "prune", "--all", "--force")
	if err != nil {
		return nil, "", fmt.Errorf("prune unused Docker images: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "untagged: "); ok {
			pruned = append(pruned, name)
		} else if total, ok := strings.CutPrefix(line, "Total reclaimed space: "); ok {
			reclaimed = total
		}
	}
	return pruned, reclaimed, nil
}
