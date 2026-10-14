# site-deploy

Go CLI + daemon that deploys ZIP'd Compose bundles to one Docker host behind Caddy, with idle suspension.
The README covers usage and `docs/architecture.md` covers internals. Read them before changing behaviour.

Servers are configured as CLI hosts (`site-deploy host list`). A gitignored `CLAUDE.local.md` may describe the ones this
checkout deploys to. Don't commit server names, addresses, domains or app names: this repository is public.

Deploying over an existing name is a conflict. Replacing an app means `delete` then `deploy` (downtime). Named volumes survive.

The `site-deploy` skill lives in `skills/site-deploy/`. When CLI commands, bundle rules or the upgrade flow change,
update `SKILL.md`, `references/bundle-format.md` and `scripts/bundle.sh` in the same change.

## Releasing / upgrading

- Bump `VERSION` (semver) for any change you install. `deploy/upgrade.sh` refuses same-or-older versions without `--force`.
- The GitHub Action (`action.yml`, README "GitHub Actions") builds the CLI from the ref workflows use, normally `main`.
  Push to GitHub before upgrading a daemon that CI deploys to, or those workflows get HTTP 426.
- `make check` (tests + vet) before releasing. For engine/Compose changes, also run the Docker integration test on
  the server (see README "Scope and validation"); it is safe there because it namespaces and cleans up its own resources.
- `deploy/upgrade.sh HOST` builds, upgrades the local CLI **first**, then installs on the host, built for its
  architecture. On the host it backs up `/var/lib/site-deploy`, switches the release symlink, and verifies the new
  version is running. It rolls back automatically if not.
- Compatibility contract: a release daemon rejects CLIs older than itself (HTTP 426). New CLIs must keep working against
  older daemons, so gate new endpoints/params on the daemon version (the API rejects unknown query params).
- Persisted-state changes: bump `store.DataFormat` and add the migration in `Store.CheckFormat`. Older daemons refuse
  newer formats, so a rollback across a format change needs the data backup restored.
