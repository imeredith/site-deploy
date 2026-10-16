package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"site-deploy/internal/manifest"
	"site-deploy/internal/model"
	"site-deploy/internal/volumes"
)

// ErrNoVolumes means the daemon runs without site-deploy volumes.
var ErrNoVolumes = errors.New("site-deploy volumes are not enabled on this daemon (serve --volumes-dir)")

// ListVolumes returns every volume with its snapshots and the deployed
// applications that mount it.
func (m *Manager) ListVolumes() ([]volumes.Volume, error) {
	if m.volumes == nil {
		return nil, ErrNoVolumes
	}
	list, err := m.volumes.List()
	if err != nil {
		return nil, err
	}
	uses, err := m.volumeUses()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if used := uses[list[i].Name]; used != nil {
			list[i].UsedBy = used
		}
	}
	return list, nil
}

// CreateVolume makes an empty volume.
func (m *Manager) CreateVolume(ctx context.Context, name string) (volumes.Volume, error) {
	if m.volumes == nil {
		return volumes.Volume{}, ErrNoVolumes
	}
	return m.volumes.Create(ctx, name)
}

// DeleteVolume removes a volume that no deployed application mounts and that
// has no snapshots left.
func (m *Manager) DeleteVolume(ctx context.Context, name string) error {
	if m.volumes == nil {
		return ErrNoVolumes
	}
	uses, err := m.volumeUses()
	if err != nil {
		return err
	}
	if used := uses[name]; len(used) > 0 {
		apps := make([]string, 0, len(used))
		for _, use := range used {
			apps = append(apps, use.App)
		}
		return fmt.Errorf("%w: %s is mounted by %s; delete those applications first", volumes.ErrInUse, name, strings.Join(apps, ", "))
	}
	return m.volumes.Delete(ctx, name)
}

// DeleteVolumeSnapshot removes app's snapshot of a volume, unless app is
// deployed and mounts it. Deleting app keeps the snapshot for its next deploy.
func (m *Manager) DeleteVolumeSnapshot(ctx context.Context, name, app string) error {
	if m.volumes == nil {
		return ErrNoVolumes
	}
	uses, err := m.volumeUses()
	if err != nil {
		return err
	}
	if slices.Contains(uses[name], volumes.Use{App: app, Snapshot: true}) {
		return fmt.Errorf("%w: %s mounts its snapshot of %s; delete %s first", volumes.ErrInUse, app, name, app)
	}
	return m.volumes.DeleteSnapshot(ctx, name, app)
}

// volumeUses maps each volume to the deployed applications that mount it or
// their snapshot of it, read from their generated Compose files. An unreadable
// file is an error, so a volume in use is never treated as unused.
func (m *Manager) volumeUses() (map[string][]volumes.Use, error) {
	uses := map[string][]volumes.Use{}
	for _, app := range m.List() {
		data, err := os.ReadFile(app.ComposeFile)
		if err != nil {
			if app.State == model.StateDeleting && os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read Compose file of %s: %w", app.Name, err)
		}
		mounts, err := manifest.HostMounts(data)
		if err != nil {
			return nil, fmt.Errorf("Compose file of %s: %w", app.Name, err)
		}
		for _, mount := range mounts {
			name, snapshotApp, ok := m.volumes.Locate(mount.Source)
			if !ok {
				continue
			}
			use := volumes.Use{App: app.Name, Snapshot: snapshotApp != ""}
			if !slices.Contains(uses[name], use) {
				uses[name] = append(uses[name], use)
			}
		}
	}
	return uses, nil
}
