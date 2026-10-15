# site-deploy

Go CLI + daemon that deploys ZIP'd Compose bundles to one Docker host behind Caddy, with idle suspension.
The README covers usage and `docs/architecture.md` covers internals. Read them before changing behaviour.

Servers are configured as CLI hosts (`site-deploy host list`). A gitignored `CLAUDE.local.md` may describe the ones this
checkout deploys to. Don't commit server names, addresses, domains or app names: this repository is public.

Deploying over an existing name is a conflict. Replacing an app means `delete` then `deploy` (downtime). Named volumes survive.

The `site-deploy` skill lives in `skills/site-deploy/`. When CLI commands, bundle rules or the upgrade flow change,
update `SKILL.md`, `references/bundle-format.md` and `scripts/bundle.sh` in the same change.

## Releasing / upgrading

- Bump `VERSION` (semver) for any change you install. `site-deploy upgrade` installs only a newer version than the host's,
  and refuses a CLI older than the repository's `VERSION` on `main`, unless `--force`.
- The GitHub Action (`action.yml`, README "GitHub Actions") builds the CLI from the ref workflows use, normally `main`.
  Push to GitHub before upgrading a daemon that CI deploys to, or those workflows get HTTP 426. `upgrade` warns when the
  CLI is newer than the repository.
- `make check` (tests + vet) before releasing. For engine/Compose changes, also run the Docker integration test on
  the server (see README "Scope and validation"); it is safe there because it namespaces and cleans up its own resources.
- `make upgrade HOST=NAME` runs `make install-cli` (the CLI plus Linux server builds for amd64 and arm64), then
  `site-deploy upgrade NAME`, which installs the CLI's own version on the host, for its architecture, so the CLI is
  always upgraded first. On the host `deploy/install-host.sh` (embedded in the CLI) backs up `/var/lib/site-deploy`,
  switches the release symlink, and verifies the new version is running. It rolls back automatically if not.
  `site-deploy upgrade NAME --check` reports the versions without changing anything.
- Compatibility contract: a release daemon rejects CLIs older than itself (HTTP 426). New CLIs must keep working against
  older daemons, so gate new endpoints/params on the daemon version (the API rejects unknown query params).
- Persisted-state changes: bump `store.DataFormat` and add the migration in `Store.CheckFormat`. Older daemons refuse
  newer formats, so a rollback across a format change needs the data backup restored.
