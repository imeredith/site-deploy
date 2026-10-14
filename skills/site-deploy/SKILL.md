---
name: site-deploy
description: Deploy and manage web apps on self-hosted site-deploy servers with the site-deploy CLI. Use this whenever the user wants to deploy, publish, host, preview, or put online a site, app, static page, or Docker container "on my server", on one of their site-deploy hosts or its domains, or "with site-deploy"; whenever they ask to list, inspect, check logs of, suspend, resume, redeploy, update, or delete something already deployed there; when packaging a Docker Compose app as a site-deploy ZIP bundle; and when upgrading or releasing site-deploy itself (VERSION bumps, deploy/upgrade.sh, "upgrade the CLI first", HTTP 426, CLI/daemon version mismatch).
---

# site-deploy

site-deploy runs ZIP bundles containing a Docker Compose app on one Docker host. Each app gets one public hostname. Idle apps are stopped to free memory, and the next request wakes them.

- **Servers** are the CLI's configured hosts: `site-deploy host list` shows them, with `*` on the current one. On each, the daemon normally runs as the `site-deploy` systemd service, with data in `/var/lib/site-deploy`.
- **Server notes:** if `~/.config/site-deploy/servers.md` exists, read it first. It's the user's own description of their servers: which domains each one serves, CPU architecture, how to reach it, and anything to record after a deploy. Follow it over the generic advice here.
- **Ask which server** when the user doesn't say and there's more than one.
- **Source of truth:** the site-deploy repo (`README.md`, `docs/architecture.md`). Read those when this skill doesn't answer something. Don't guess at behaviour.

## Access

Run every command with the local CLI (`~/.local/bin/site-deploy`) and **always pass `--host NAME`**: the user can switch the current host, so don't rely on it. For each command the CLI opens an SSH tunnel to the daemon's loopback API and closes it afterwards. Hosts and tokens live in `~/.config/site-deploy/` (`hosts.json`, `tokens/`). The commands below omit `--host` for brevity.

Start with this. In one call it confirms SSH, the token, and version compatibility:

```sh
site-deploy --host NAME daemon-version
```

If it fails:
- **"upgrade the CLI first" / HTTP 426:** the local CLI is older than the daemon. A release daemon refuses older CLIs before doing anything, so no harm was done. Run `make install-cli` in the site-deploy checkout. If the checkout's `VERSION` is itself older than the daemon, the checkout is stale. Ask the user before touching it.
- **Token file not found or wrong permissions:** run `site-deploy host fetch-token HOST`.
- **"SSH tunnel to ... failed":** the message includes ssh's error. Check `site-deploy --host HOST ssh -- true`. If the host uses a custom remote shell (`-e`, shown by `host list`), check that whatever it depends on, such as a proxy, is running.
- **`site-deploy: command not found`, or `unknown command "host"`:** run `make install-cli` in the repo.

## Commands

| Command | Notes |
|---|---|
| `site-deploy list` | Name, domain, state, idle timeout, access (`public` / `basic auth (N)`). Add `--json` for everything |
| `site-deploy inspect NAME` | Full JSON record, including `error`, `upstream`, `project` |
| `site-deploy deploy NAME bundle.zip` | Uploads, validates, starts, and waits until healthy. Default timeout is 10m (`--timeout`) |
| `site-deploy logs NAME [--tail N]` | A snapshot (default 100 lines, max 10000), not a live follow |
| `site-deploy suspend NAME` / `resume NAME` | Suspended apps also wake on their next request |
| `site-deploy delete NAME` | Removes containers, networks, and uploaded files. Named volumes are kept |
| `site-deploy push LOCAL_IMAGE NAME:TAG` | Push a local dependencies image to the private registry. Prints the `image:` line to use |
| `site-deploy images` | Registry contents, push times, and the apps using each image |
| `site-deploy cleanup` | Remove unused registry tags and Docker images now (it also runs hourly and after each delete) |
| `site-deploy caddy-config --http` | The routing config site-deploy pushes to Caddy |

Names must start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens. App states are `starting`, `running`, `stopping`, `suspended`, `deleting`, and `error`.

## Deploying an app

1. **Pick the name and domain.** Use a domain that points at the chosen server (the server notes say which; otherwise ask). Wildcard DNS usually covers one subdomain level only. Run `site-deploy list` first: an existing name or domain is a conflict, not an update.
2. **Write `compose.yaml`** with an `x-site-deploy` block. Read `references/bundle-format.md` for the rules and worked examples (static site, Node app with persistent data, app + Postgres, password-protected site, compiled app on a dependencies-only image). Keep the app and its dependencies apart:
   - **The app goes in the bundle.** Build it locally (compiled binaries, frontend builds, `node_modules`) and bind-mount it from the ZIP, read-only. A code change is then just a new bundle.
   - **Images hold only dependencies.** Use a stock image when one fits (`nginx`, `node`, `debian`). If none does (for example a program that needs Chromium), build a dependencies-only image locally, with no app code in it. Push it with `site-deploy push`, give it a new tag only when the dependencies change, and reference it as `image: 127.0.0.1:9082/NAME:TAG`.
   - **Match the server's CPU.** An image built on an amd64 workstation won't run on an arm64 server. Build for the server's platform, for example `docker buildx build --platform linux/arm64 --load -t NAME .`. Stock multi-arch images are fine. `site-deploy --host NAME ssh -- uname -m` tells you.
   - **Never copy images onto the server any other way** (`docker save | ssh docker load`, building on the host). Never use `pull_policy: never` either. Cleanup removes images that no container uses, and site-deploy can only bring back images it can pull. The server never builds images: `build:` is rejected.
3. **Treat it as public unless protected.** Assume a deployed domain is reachable by anyone on the internet unless the server notes say otherwise. If the app is private, a preview, or shows data, check with the user, then add `basic_auth` to `x-site-deploy` (reference example 4). Caddy checks the password before the request reaches the app, so unauthenticated traffic never wakes it.
4. **Package it** with the bundled script. The Compose file must sit at the ZIP root, and zipping the folder itself instead of its contents is the classic mistake:
   ```sh
   <skill-dir>/scripts/bundle.sh ./myapp /tmp/myapp.zip
   ```
   Build bundles in a temp or scratch directory, not inside the user's repo.
5. **Deploy:** run `site-deploy deploy myapp /tmp/myapp.zip`. Success prints `myapp: running (myapp.example.com)`.
6. **Verify from outside:** run `curl -sS -o /dev/null -w '%{http_code}\n' https://myapp.example.com/`. A Caddy 404 means the domain isn't registered. A 503 means the app didn't start or crashed (check `logs`).
7. **Record it** wherever the server notes say apps are tracked, and remove the entry when the app is deleted.

If deploy fails after upload, with a message like `deployment "x" is saved; inspect it, then resume or delete it`, the record exists but startup failed. Read the error with `inspect` and `logs`, fix the bundle, then `delete` and deploy again. Use `resume` only if the cause was transient.

## Updating an app

There is no in-place update or rollback. Replacing an app means `delete` followed by `deploy` with the same name. That causes downtime and removes uploaded files, including anything the app wrote into bind mounts. **Named volumes survive** and reattach when the app name and volume names stay the same. Don't rename either if the data should carry over.

Because delete is destructive and visible to users, confirm with the user before deleting an app that's serving traffic. Keep the old ZIP, or note how to rebuild it, so you can redeploy it if the new version fails.

## Behaviour worth knowing

- **Idle suspension:** after `idle_timeout` (default `5m`) with no gateway requests, the whole Compose project stops, databases included. The next request waits for a cold start, usually about 1s for small apps. If a browser page load waits more than 1s (daemon 0.4.0+), the visitor sees a "Starting up" page with a spinner that reloads into the app when it's ready; API calls and form posts just wait. Background jobs, cron work, and direct database connections don't count as activity. Use `idle_timeout: 0` for anything that must keep running.
- **Image cleanup** (daemon 0.3.0+) runs hourly, after every `delete`, and on `site-deploy cleanup`:
  - **Registry tags:** a tag is deleted once no deployed app references it and it's older than the grace period (24h). That covers the time between a push and its deploy.
  - **Docker images on the host:** images that no container uses are removed. Suspended apps keep their containers, so their images stay.
  - **Redeploying after cleanup:** the image is pulled again, from the registry or a public registry. So delete + deploy is fine for registry images, but fatal for an image that was copied in by hand.
- **Open connections:** `delete` (daemon 0.3.1+) breaks them, including streams and WebSockets such as an open browser tab, and proceeds. So a redeploy never has to wait for users to close tabs. `suspend` still refuses with HTTP 409 while requests are open, and both refuse while a startup is in progress. Retry shortly.
- **Caddy sync warnings** (`Warning: Caddy synchronization failed`) don't undo the deploy. The daemon retries every 30s. Check Caddy on the server, for example `site-deploy --host NAME ssh -- systemctl status caddy` (or `docker logs caddy` if it runs in Docker).
- **Server logs:** run `site-deploy --host NAME ssh -- journalctl -u site-deploy -n 100 --no-pager`.

## Upgrading site-deploy itself

Do this only when the user asks for a site-deploy change or release. The procedure is in the repo README, "Versions and upgrades". In short:

1. Make and test the change in the site-deploy checkout: `make check`. If engine or Compose handling changed, also run the Docker integration test on the servers (README, "Scope and validation"). It namespaces and cleans up after itself.
2. Bump `VERSION` (semver). `deploy/upgrade.sh` refuses same-or-older versions, which keeps the "CLI ≥ daemon" rule meaningful.
3. Commit and push to GitHub first if any host is deployed to by the GitHub Action (README "GitHub Actions"). The action builds its CLI from the ref workflows use, normally `main`, so a daemon newer than that makes those workflows fail with 426.
4. Run `deploy/upgrade.sh HOST`. It upgrades the local CLI **first**, then the daemon. Before switching it backs up `/var/lib/site-deploy` and waits for the new version to report in. If it doesn't, it restores the previous release automatically. Running apps keep running, but public requests fail for the few seconds the daemon restarts.
5. Confirm with `site-deploy --host HOST daemon-version`, and record the version wherever the server notes say.

`--force` reinstalls the same version or downgrades. Ask before downgrading. If a release changed `store.DataFormat`, an older binary refuses to start, so the data backup has to be restored as well.

Compatibility rules when changing the API: newer CLIs must keep working against older daemons, and the API rejects unknown endpoints and query parameters. Gate new CLI features on the daemon version reported by `daemon-version` or `/v1/version`.

## Don't

- Don't add other sites to a Caddy that site-deploy manages (`--caddy-admin`). site-deploy replaces its entire config.
- Don't expose ports 9080, 9081, or 2019 beyond loopback, or put the token in command lines or committed files.
