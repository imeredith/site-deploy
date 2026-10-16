# site-deploy

A Go CLI and daemon for deploying ZIP bundles to one Linux Docker host. Each bundle contains a Compose file and application files that are bind-mounted into existing container images. Caddy handles public HTTP/TLS; a local gateway starts a stopped application before forwarding its first request.

Idle suspension **stops the whole Compose project to release container memory**. Named volumes survive suspension, resumption, and deletion. HTTP requests, streams, and WebSockets keep an application active until they finish.

## Run locally

Install Go 1.23 or newer, Docker Engine, the Docker Compose v2 plugin with `up --wait` support, Caddy 2, and `zip`. The daemon must have access to the local Docker Unix socket. Run these commands from the repository root:

```sh
make build
./bin/site-deploy serve --data-dir "$PWD/.site-deploy"
```

`make build` produces a static binary stamped with the release version from `VERSION` (see [Versions and upgrades](#versions-and-upgrades)). A plain `go build -o bin/site-deploy ./cmd/site-deploy` also works, but it reports version `dev`, and release daemons reject `dev` CLIs.

Keep that daemon running. On first launch it creates `.site-deploy/token`, a random 64-character API token with owner-only permissions. The control API listens on `127.0.0.1:9081`; the application gateway listens on `127.0.0.1:9080`.

In another terminal, package the **contents** of the example directory. `compose.yaml` must be at the ZIP root, without a surrounding `hello/` directory:

```sh
SITE_DEPLOY_BUNDLE_DIR="$(mktemp -d -t site-deploy-bundle.XXXXXX)"
(cd examples/hello && zip -r "$SITE_DEPLOY_BUNDLE_DIR/hello.zip" compose.yaml public)
./bin/site-deploy --token-file .site-deploy/token deploy hello "$SITE_DEPLOY_BUNDLE_DIR/hello.zip"
./bin/site-deploy --token-file .site-deploy/token list
./bin/site-deploy --token-file .site-deploy/token caddy-config --http > "$SITE_DEPLOY_BUNDLE_DIR/caddy.json"
sudo caddy run --config "$SITE_DEPLOY_BUNDLE_DIR/caddy.json"
```

Use a dedicated Caddy instance with port 80 available. The final command runs Caddy in the foreground. In another terminal:

```sh
curl --resolve hello.localhost:80:127.0.0.1 http://hello.localhost/
./bin/site-deploy --token-file .site-deploy/token suspend hello
curl --resolve hello.localhost:80:127.0.0.1 http://hello.localhost/
```

The second request starts the stopped application and then receives its response. After five minutes without an active request, the default idle scan stops it again. For a faster demonstration, change the example's `idle_timeout` before packaging. For troubleshooting without Caddy, send the same host directly to the local gateway:

```sh
curl -H 'Host: hello.localhost' http://127.0.0.1:9080/
```

## Bundle format

The ZIP root must contain exactly one of `compose.yaml`, `compose.yml`, `docker-compose.yaml`, or `docker-compose.yml`. A minimal bundle looks like [the static example](examples/hello/compose.yaml):

```yaml
x-site-deploy:
  domain: hello.localhost
  service: web
  port: 80
  idle_timeout: 5m
  start_timeout: 60s
  suspend_mode: stop

services:
  web:
    image: nginx:alpine
    volumes:
      - ./public:/usr/share/nginx/html:ro
    cpus: 0.5
    mem_limit: 128m
```

| Field | Meaning |
| --- | --- |
| `domain` | Unique hostname routed to this application; omit scheme, port, and path. |
| `service` | Compose service receiving HTTP traffic. Exactly one instance is supported. |
| `port` | HTTP port inside that service's container; listen on `0.0.0.0` inside the container. |
| `idle_timeout` | Time since the last completed request; default `5m`. Use `0` to stay running. |
| `start_timeout` | Deadline for starting the project and reaching its HTTP port; default `60s`. |
| `suspend_mode` | `stop` releases memory and is the default. Optional `pause` retains allocated memory. |
| `basic_auth` | Optional list of `{user, hash}` accounts. Caddy then requires HTTP basic authentication for this domain. See below. |
| `volumes` | Optional [site-deploy volumes](#site-deploy-volumes) to mount, each with its key. |

### Basic authentication

```yaml
x-site-deploy:
  domain: preview.example.com
  service: web
  port: 80
  basic_auth:
    - user: preview
      hash: $2a$14$O4UeNfvX7xUhP9mj6tjWIOkvvB/sTqKipfaXtgWqmJ7MZjMg/l9me
```

Caddy checks credentials before forwarding to the gateway. A request without valid credentials gets `401` from Caddy, never reaches the application, never wakes a suspended one, and doesn't count as activity.

`hash` must be a bcrypt hash (`$2a$`, `$2b$` or `$2y$`, cost 4–31). Plaintext passwords are rejected. Generate one with `caddy hash-password` (cost 14), or with `docker run --rm caddy:2-alpine caddy hash-password --plaintext 'PASSWORD'`. Write the hash as-is: `x-site-deploy` is removed before Compose reads the file, so `$` is never interpolated and must not be escaped as `$$`. Usernames start with a letter or digit and contain at most 64 letters, digits, `.`, `_`, `@`, `+` or `-`. Up to 64 accounts are allowed.

Verified credentials are cached in Caddy's memory, so pages with many assets don't pay bcrypt's cost on every request. The API and CLI show account usernames but never the hashes. The hashes are stored in the daemon's private state and sent only to Caddy. Changing the accounts means redeploying the application. Daemons older than 0.2.0 reject `basic_auth` bundles.

Relative bind sources must already exist inside the ZIP. Absolute sources are host paths, which the operator has to allow; see [Host paths](#host-paths). All uploaded `ports` publications are replaced: only the selected service receives an ephemeral host port bound to `127.0.0.1`. Other services communicate through Compose networks. `depends_on`, healthchecks, named volumes, `cpus`, `mem_limit`, and supported `deploy.resources` settings are passed to Compose. Startup waits for Compose healthchecks and a TCP connection to the routed port; add a healthcheck for application-level readiness.

Uploaded `restart: always` policies become `unless-stopped`, so Docker does not wake an idle-stopped container after an Engine restart. Other supported policies are `no`, `on-failure`, `on-failure:N`, and `unless-stopped`; `deploy.restart_policy` is rejected. See [Docker's restart-policy behavior](https://docs.docker.com/engine/containers/start-containers-automatically/). The root filename `compose.generated.json` is reserved. The generated Compose configuration is stored outside uploaded files, so a writable bundle mount cannot overwrite it.

The [Node example](examples/node/compose.yaml) binds `server.js` into `node:24-bookworm-slim`, persists a request counter in a named volume, and includes a healthcheck. Package it with:

```sh
(cd examples/node && zip -r "$SITE_DEPLOY_BUNDLE_DIR/node.zip" compose.yaml server.js)
./bin/site-deploy --token-file .site-deploy/token deploy node "$SITE_DEPLOY_BUNDLE_DIR/node.zip"
```

Regenerate and reload Caddy's configuration after adding or deleting applications, or enable automatic synchronization below. The Node example uses `node.localhost`; its `/stream` endpoint sends a 30-second response that keeps the stack running.

Only gateway HTTP traffic counts toward activity. Background jobs, scheduled work, and direct database connections are invisible to the idle timer. Set `idle_timeout: 0` for stacks that must run continuously. All services in an application, including its database and other dependencies, suspend and resume together.

A browser opening a page of a sleeping application gets a resuming page with a spinner if the application isn't ready within a second. The page reloads itself once the application is ready, or says the site couldn't start, without internal details, if startup fails. Quick wakes never show it. Every other request, such as API calls, form posts, `fetch`, and `curl`, waits for the application as before. Browsers mark page loads with `Sec-Fetch-Mode: navigate`, which they send only to HTTPS and `localhost` origins, so a page over plain HTTP waits instead. The page checks progress by sending `X-Site-Deploy-Resume` to its own URL; the gateway answers any request with that header itself, so applications never receive it.

Use explicit environment values in Compose or include a file with `env_file: ./app.env`. The bundle's `.env` is not implicitly loaded for Compose interpolation, and the daemon's environment is not passed through to applications. Use explicit image names, paths, and resource references; interpolation is rejected in those fields. Use `$$` when a Compose command needs a literal `$` for expansion inside the container.

### Host paths

By default a bundle can only mount its own files. To let applications mount host directories, the operator lists them with `serve --host-path`, repeated once per directory, in the form `[APP,...=]PATH[:rw]`:

```sh
site-deploy serve --data-dir /var/lib/site-deploy \
  --host-path /srv/media \
  --host-path notes,wiki=/srv/notes:rw
```

Here every application may mount `/srv/media`, or anything under it, read-only. Only `notes` and `wiki` may mount `/srv/notes`, and they may mount it read-write. On a systemd host, put these flags in a drop-in such as `/etc/systemd/system/site-deploy.service.d/host-paths.conf`, which upgrades leave alone. The daemon logs the allowlist when it starts.

A bundle mounts an allowed path with an absolute source:

```yaml
services:
  web:
    image: nginx:alpine
    volumes:
      - /srv/media/films:/usr/share/nginx/html/films:ro
```

The rules:

- An entry covers its directory and everything under it. Each entry is read-only unless it ends in `:rw`, and a read-only entry needs `:ro` (or `read_only: true`) on the mount. With overlapping entries, a mount is allowed if any entry that applies to the application allows it.
- The mounted path must already exist; it is never created. It must be a regular file or directory, and no part of it may be a symlink.
- Mounts are checked when the bundle is deployed and again before every start, including wake-ups. After you remove an entry and restart the daemon, apps that use it can't start, and running ones are stopped with an error.
- The daemon checks paths with its own permissions, so the `site-deploy` user must be able to reach each mounted path, even though Docker does the mounting.
- At startup the daemon refuses entries that don't exist, go through a symlink, overlap its data directory, or contain the Docker socket.

A writable host path gives the container's users, often root, write access to that directory on the host. Allow only directories that nothing else on the host trusts, and don't give one application write access to a tree that another application mounts paths inside. The checks refuse a symlink that a container plants there, but a swap between the check and Docker's mount can't be ruled out.

### Site-deploy volumes

A named volume belongs to one application. A site-deploy volume (daemon 0.10.0+) is a btrfs subvolume that exists apart from any deployment. Applications name it to mount the volume itself, or their own snapshot of it. A pull-request preview, for example, can start from a snapshot of a large dataset. Taking it is instant whatever the size, and the preview keeps its changes to itself across redeploys.

The operator enables volumes with a directory on a btrfs filesystem, then creates them:

```sh
site-deploy serve --data-dir /var/lib/site-deploy --volumes-dir /srv/site-deploy-volumes
site-deploy volume create corpus     # prints its path
```

A bundle mounts a volume by name from `x-site-deploy`, so the same bundle can be deployed as production and as previews:

```yaml
x-site-deploy:
  domain: pr12.example.com
  service: web
  port: 8080
  volumes:
    corpus:                  # the volume's name
      target: /corpus        # where it appears in the container
      read_only: true        # default false
      snapshot: true         # default false, which mounts the volume itself
      service: web           # default: the routed service
```

- **Any application can mount any volume** that exists. Creating a volume is what makes it available, and anyone who can deploy controls the server anyway. A bundle that names a missing volume fails to deploy.
- **Without `snapshot`**, the application mounts the volume itself. Its writes are seen by every application that mounts the volume, and by snapshots taken later.
- **With `snapshot: true`**, the application mounts its own writable snapshot, made on its first deploy and named after the application. Snapshots are atomic, and share disk space with the volume until either side changes. Deleting the application keeps its snapshot, so redeploying it (`delete`, then `deploy` with the same name) mounts the same snapshot, changes included. For a fresh snapshot, delete the application, then run `volume delete-snapshot corpus APP`, and deploy again.
- **Filling a volume:** `volume create` and `volume list` print its path. Its root directory is owned by root with mode `0755`. Write into it as root on the server, for example `sudo cp -a /srv/corpus/. PATH/`, or deploy an application that mounts it read-write. A container that runs as another user needs the directory `chown`ed to that user first. Snapshots keep owners and modes.
- **Every start checks the mounts again.** The volume or snapshot must still exist as a btrfs subvolume, reached without symlinks, and an application may mount only its own snapshot. Otherwise the start fails with an error, as for host paths.
- **Deleting:** `volume delete` refuses while a deployed application mounts the volume or snapshots of it remain. `volume delete-snapshot` refuses while its application is deployed.

```sh
site-deploy volume list                          # volumes, snapshots, and the apps mounting them
site-deploy volume delete-snapshot corpus pr12   # after site-deploy delete pr12
site-deploy volume delete corpus
```

Each volume is a directory in the volumes directory, holding `volume.json` (its creation time), `data` (the volume) and `snapshots/APP`. The daemon creates the volumes directory with mode `0700` if it's missing. At startup it refuses one that isn't on btrfs, goes through a symlink, or overlaps the data directory, the Docker socket's directory or a `--host-path` entry. Btrfs operations run in a short-lived container as root, because the daemon's own user can't snapshot or delete subvolumes holding files that containers wrote as other users. That container runs the `--volume-copy-image` image (`busybox:1.37`), with only the volumes directory and the daemon's executable mounted, so the executable must be static, as `make build` makes it. Back up the volumes directory separately, for example with `btrfs send`: the upgrade backups cover only the data directory.

Daemon 0.9.0 had volumes too, but each one had a key that bundles had to give. 0.10.0 refuses a `key`, so remove it from bundles; volumes made by 0.9.0 keep working. Older daemons reject bundles with `volumes`, and rolling a daemon back below 0.9.0 stops the applications that mount volumes, because it sees their mounts as host paths nobody allowed.

## CLI

Commands contact the daemon; Docker only needs to be installed on the deployment host.

```sh
./bin/site-deploy --token-file .site-deploy/token list
./bin/site-deploy --token-file .site-deploy/token inspect hello --json
./bin/site-deploy --token-file .site-deploy/token suspend hello
./bin/site-deploy --token-file .site-deploy/token resume hello
./bin/site-deploy --token-file .site-deploy/token logs hello --tail 100
./bin/site-deploy --token-file .site-deploy/token caddy-config --http
./bin/site-deploy --token-file .site-deploy/token delete hello
./bin/site-deploy --token-file .site-deploy/token deploy hello-pr12 hello.zip --copy-volumes-from hello
./bin/site-deploy --token-file .site-deploy/token daemon-version
./bin/site-deploy --token-file .site-deploy/token push my-deps:local my-deps:2026-10-13
./bin/site-deploy --token-file .site-deploy/token images
./bin/site-deploy --token-file .site-deploy/token cleanup
./bin/site-deploy --token-file .site-deploy/token volume create corpus
./bin/site-deploy --token-file .site-deploy/token volume list
```

Common options include `--host`, `--server`, `--token-file`, `--token`, and `--json`; flags can appear before or after the command (before `host` and `ssh`). From a workstation, commands normally go to a configured [host](#hosts). `SITE_DEPLOY_HOST`, `SITE_DEPLOY_SERVER`, `SITE_DEPLOY_TOKEN_FILE`, and `SITE_DEPLOY_TOKEN` also configure the client. The default token file is `.site-deploy/token`; prefer it to putting a token in shell history. `--timeout` controls the entire upload/request deadline and defaults to `10m`. Names start with a lowercase letter and contain at most 48 lowercase letters, digits, or hyphens.

Deploying an existing name or domain returns a conflict. There are no rolling updates or rollback commands. To replace a bundle, delete its deployment and deploy the new ZIP with the same name. This causes downtime. Deletion removes containers, networks, and extracted files, while named volumes are retained under the application's stable Compose project. Reusing the same data directory, application name, and volume names reconnects that data. Persistent writes belong in named volumes: uploaded bind directories are removed on deletion. Anonymous volumes are not a reliable way to reconnect data after redeployment.

### Starting with another application's data

`deploy NAME ZIP --copy-volumes-from APP` (daemon 0.7.0+) starts the new application with copies of APP's named volumes, for example a pull-request preview with a copy of production's database. Each named volume the new bundle declares that APP also declares, by the same name, is copied before the first start; the others start empty, as usual. APP is paused for as long as the copy takes, so the copy is a consistent point-in-time snapshot of every volume, like a power cut: SQLite and PostgreSQL recover from it as they do after a crash. Its requests wait meanwhile rather than fail. The copy runs in a `busybox:1.37` container (daemon `--volume-copy-image`), keeping owners, modes, and symlinks.

- **Nothing is overwritten.** If a volume the copy would fill already exists, for example one kept by deleting an earlier deployment of NAME, deploy refuses before saving anything. Remove that volume on the server (`docker volume rm PROJECT_VOLUME`; `inspect` shows the project) to take a fresh copy, or deploy without the option to keep using it.
- **Only the first start copies.** Later resumes, and redeploys without the option, use the volumes as they are, so the two applications' data diverge from then on.
- **APP must have started once**, so its volumes exist. An application never copies its own volumes.
- **If the copy fails**, deploy reports it and the application is saved in the `error` state with no volumes. Delete it and deploy again. A later wake would start it empty.
- **The copy is real data.** Everything in the volumes comes along: accounts, password hashes, API tokens, and the application's own secrets, such as signing keys stored in its database. So does anything the application does on its own with that data, such as scheduled jobs or notifications, unless its configuration turns that off for copies. If the copy runs code you haven't reviewed, such as a pull request's, give it its own signing keys: tokens it signs with APP's keys may be valid on APP.
- The CLI checks the daemon's version first, because older daemons ignore the option and would deploy without the copy.

Fresh data directories receive distinct Docker project prefixes, saved in `DATA_DIR/project-prefix`. Back up that file with the rest of the data directory. Existing installations retain their original `sd-NAME` projects to preserve volume access; those legacy installations still need distinct application names when sharing Docker. `inspect --json` shows each application's actual project name.

Manual suspension returns a busy error while requests or lifecycle operations are active. Deletion, which is also the first step of a redeploy, doesn't wait for open connections. It breaks them, including streams and WebSockets such as an open browser tab's live updates. It then waits up to 5 seconds for those requests to end before removing the containers. Requests that arrive during a deletion wait for it and then get 404, or the new deployment's response once it's redeployed. Deletion is refused only while a lifecycle operation, such as a startup, is in progress. `logs` returns a bounded snapshot rather than a live follow stream.

Failed startup stops any partially started containers and keeps the deployment available for inspection and retry. Automatic wake attempts wait two seconds after a startup failure before trying again; an explicit `resume` bypasses that cooldown. If an upstream connection fails, that request receives 503 and the next request reconciles the stack after existing requests finish. Failed requests are not replayed automatically. If deletion cannot remove files written by a container, it retains a `deleting` record: fix the reported ownership or permissions and retry `delete`, including after a daemon restart.

## Caddy and a server

Run Caddy as a dedicated host service in the same network namespace as the daemon. Generate its complete JSON configuration with:

```sh
./bin/site-deploy --token-file .site-deploy/token caddy-config > site-deploy-caddy.json
```

The default configuration uses HTTPS on port 443 with Caddy's automatic HTTPS. Set each bundle's `domain` to a DNS name pointing to the server and allow inbound ports 80 and 443 for public certificate issuance and traffic. Use `--http` only for the HTTP-on-port-80 setup shown above. The generated gateway defaults to `127.0.0.1:9080`; pass `caddy-config --gateway 127.0.0.1:PORT` if you changed it.

Install the JSON at `/etc/caddy/site-deploy.json`. Configure the dedicated Caddy service to start with `caddy run --config /etc/caddy/site-deploy.json` and reload with `caddy reload --config /etc/caddy/site-deploy.json`. Preserve Caddy's writable certificate data directory across restarts. Do not supply the Caddyfile adapter for this JSON file. When managing configuration manually, regenerate, install, and reload the file after each deploy or delete.

Alternatively, start Caddy first and enable automatic synchronization on the daemon:

```sh
./bin/site-deploy serve --data-dir "$PWD/.site-deploy" \
  --caddy-admin http://127.0.0.1:2019
```

For local HTTP development, add `--http` to this `serve` command. On a host that public ACME challenges cannot reach, such as one behind a VPN, add `--tls-internal` instead: Caddy issues certificates from its own local CA, so HTTPS works, but browsers warn until that CA's root (`pki/authorities/local/root.crt` in Caddy's data directory) is trusted. `caddy-config` does not render this mode. To serve certificates you obtained yourself, such as a wildcard from a manual DNS challenge, add `--tls-cert-dir DIR` (daemon 0.8.1+): Caddy loads every `.pem` file in `DIR`, each a certificate chain followed by its private key, and obtains no certificate for the names they cover, while other names are handled as before. `DIR` is a path as Caddy sees it, so a Caddy in Docker needs it inside one of its mounts, such as `/data/site-deploy-certs`. Caddy reads the files only when its configuration changes, so after replacing one, force a reload: `curl -s 127.0.0.1:2019/config/ | curl -sS -X POST -H 'Content-Type: application/json' -H 'Cache-Control: must-revalidate' --data-binary @- 127.0.0.1:2019/load`. **`--caddy-admin` replaces Caddy's entire configuration**, including any other sites. Use it only with a Caddy instance dedicated to this orchestrator. It loads configuration on startup, after deploy/delete, and periodically to retry failed synchronization. If initial synchronization fails, the daemon exits; later failures are reported without undoing a completed deployment. Keep the admin endpoint on loopback. API-loaded configuration is separate from your JSON file: restart the daemon after restarting Caddy, or allow its periodic synchronization to restore the routes.

[`site-deploy upgrade`](#versions-and-upgrades) automates the following installation steps and later upgrades. The example [systemd unit](deploy/site-deploy.service) runs the daemon as a `site-deploy` account with access to the `docker` group and stores state in `/var/lib/site-deploy`. Create that account first, install the binary at `/usr/local/bin/site-deploy`, and install the unit as `/etc/systemd/system/site-deploy.service`:

```sh
sudo useradd --system --user-group --home-dir /var/lib/site-deploy --shell /usr/sbin/nologin site-deploy
sudo install -m 0755 bin/site-deploy /usr/local/bin/site-deploy
sudo install -m 0644 deploy/site-deploy.service /etc/systemd/system/site-deploy.service
sudo systemctl daemon-reload
sudo systemctl enable --now site-deploy
```

Adapt the socket, supplementary group, and service dependencies for rootless Docker. To enable Caddy synchronization in the unit, append `--caddy-admin http://127.0.0.1:2019` to `ExecStart` and order the unit after your dedicated Caddy service. Keep `/var/lib/site-deploy` on persistent storage and back up it together with Docker named volumes. Its generated token is `/var/lib/site-deploy/token`.

Caddy can also run in Docker on Linux using `--network host`; bridge networking cannot reach the host daemon through the container's `127.0.0.1`. Keep its `/data` (certificates) and `/config` (the last routes, for `--resume`) persistent:

```sh
printf '{\n\tadmin 127.0.0.1:2019\n}\n' | sudo tee /etc/caddy/Caddyfile
docker run -d --name caddy --restart unless-stopped --network host \
  -v /etc/caddy/Caddyfile:/etc/caddy/Caddyfile:ro -v caddy_data:/data -v caddy_config:/config \
  caddy:2.11 caddy run --resume --config /etc/caddy/Caddyfile
```

The daemon's unit then can't be ordered after Caddy. If Caddy isn't listening when the daemon starts with `--caddy-admin`, the first sync fails, the daemon exits, and systemd restarts it until Caddy is up. The daemon still uses the host's local Docker Unix socket; remote Docker daemons are unsupported because bind mounts refer to local extracted paths.

## Private registry and image cleanup

The server never builds images, so anything that isn't a stock image has to come from a registry. Keep the two kinds of content apart:
- **Bundles** carry the application: built files, compiled binaries, `node_modules`. They're bind-mounted read-only, so a code change is just a new bundle.
- **Images** carry only dependencies. When no public image fits, build a dependencies-only image and push it to the daemon's private registry. Push a new tag only when the dependencies change.

```sh
./bin/site-deploy serve --data-dir /var/lib/site-deploy --registry 127.0.0.1:9082 --prune-images
```

`--registry` runs a `registry:3` container (`--registry-image`) named `PROJECT_PREFIX-registry`. It's published only on that loopback address, uses storage in `DATA_DIR/registry`, and runs as the daemon's user. Docker allows plain HTTP to loopback registries, so no TLS is needed. Push and reference images by that address:

```sh
site-deploy push my-deps:local my-deps:2026-10-13     # prints: image: 127.0.0.1:9082/my-deps:2026-10-13
site-deploy images                                    # tags, digests, push times, and the apps using each
```

`push` tags the image under the registry address and runs the **local** `docker push`, so your Docker must reach that address. Through a [host](#hosts), `push` also forwards the registry to a random local port and pushes through it (`--registry-via`, `SITE_DEPLOY_REGISTRY_VIA`, does the same for a forward you set up yourself). On the server itself, the registry address works directly. Registries store images by repository name, not by the address used to push them, so the image is still `127.0.0.1:9082/NAME:TAG` on the server, and that's the reference `push` prints. Several pushes can run at once. The registry has no authentication of its own: like the control API, it's reachable only through the server's loopback.

Cleanup runs every `--cleanup-interval` (default `1h`), after each successful `delete`, and on `site-deploy cleanup`. It never runs while an application is starting, because a start may be pulling.
1. **Registry:** a manifest is deleted once none of its tags is referenced by a deployed application's Compose file and every tag is older than `--registry-grace` (default `24h`, which covers the time between a push and its deploy). Tags that share a manifest with a used tag are kept. Offline garbage collection then reclaims the blobs: the registry container is stopped for a few seconds and always restarted. Collection also runs daily, to drop manifests orphaned by re-pushed tags.
2. **Host images** (`--prune-images`): `docker image prune --all` removes every image that no container, running or stopped, uses. Suspended applications keep their stopped containers, so their images stay. A redeploy pulls what it needs again. Enable this only when the host's Docker is dedicated to site-deploy, because it also removes unrelated unused images.

Because of host pruning, never rely on an image that can't be pulled. Don't load images with `docker load`, and don't set `pull_policy: never`. `delete` followed by `deploy` would remove the image in between. Registry storage is not included in the upgrade backups taken by `install-host.sh`, since its images can be pushed again.

## Hosts

The control API and gateway deliberately bind only literal loopback addresses, so the CLI reaches a server's daemon through SSH. Configure each server once as a named host:

```sh
site-deploy host add home deploy@192.0.2.10
site-deploy host add cloud deploy@203.0.113.10 -e "ssh -p 2222"   # remote shell, like rsync -e
site-deploy host fetch-token cloud  # copies /var/lib/site-deploy/token with sudo on the server
site-deploy host list               # * marks the current host
site-deploy host use cloud          # switch the current host
```

Every command then runs against the current host, or the one `--host NAME` or `SITE_DEPLOY_HOST` names:

```sh
site-deploy list
site-deploy --host home deploy hello ./hello.zip
site-deploy --host cloud ssh -- df -h /   # ssh with the host's settings; no command opens a shell
```

For each command the CLI starts ssh in the background with forwards from free local ports to the daemon's control API (and, for `push`, its registry), and stops it when the command finishes, so commands can run in parallel. Deploys, deletes, suspends, resumes, pushes and cleanups print the host they went to. The destination is anything ssh accepts, including `~/.ssh/config` aliases. The remote shell works like rsync's: `-e`/`--rsh COMMAND` on `host add` saves one for the host, `-e` on any command overrides it for that run, and `SITE_DEPLOY_RSH` (like `RSYNC_RSH`) is the default for hosts without one. It must accept ssh's arguments, and it's split at spaces, with quotes and backslashes keeping spaces inside an argument. A daemon that isn't on 9081/9082 needs `--api-port` and `--registry-port`. Hosts live in `~/.config/site-deploy/hosts.json` (`SITE_DEPLOY_CONFIG` overrides it). Tokens default to `~/.config/site-deploy/tokens/NAME`, and `--token-file` points elsewhere. `site-deploy host help` lists every option.

On the server itself, or through a tunnel you manage, `--server URL` bypasses hosts. `SITE_DEPLOY_SSH` names an unconfigured SSH target for one run, reached with `-e` or `SITE_DEPLOY_RSH`; the GitHub Action uses it. Keep ports 9080, 9081 and 2019 private: public requests go through Caddy.

[`site-deploy-remote`](deploy/site-deploy-remote) is deprecated. It's kept so existing scripts keep working, and it runs `site-deploy` against the server in `~/.config/site-deploy/remote.env`, whichever host is current.

## GitHub Actions

[`action.yml`](action.yml) deploys from a workflow through the same kind of SSH tunnel as a [host](#hosts). It uses the CLI [released](#versions-and-upgrades) for the action's `VERSION`, or builds it from the action's own source while there's no release for it, so the CLI always matches the ref you use. It packages the bundle directory with [`bundle.sh`](skills/site-deploy/scripts/bundle.sh), optionally pushes images first, and replaces an existing deployment of the same name (`delete` then `deploy`, so there is a brief outage).

On the server, give CI its own SSH user and key. The key only has to forward ports to the control API and the registry, so restrict it in that user's `~/.ssh/authorized_keys`:

```text
restrict,port-forwarding,permitopen="127.0.0.1:9081",permitopen="127.0.0.1:9082" ssh-ed25519 AAAA... github-actions
```

In the repository that deploys, add these secrets: `SITE_DEPLOY_SSH_KEY` (the private key), `SITE_DEPLOY_KNOWN_HOSTS` (output of `ssh-keyscan HOST`, run from a network you trust), and `SITE_DEPLOY_TOKEN` (the daemon's token). Then:

```yaml
name: Deploy
on:
  push:
    branches: [main]
concurrency: deploy-my-site # one deploy of this app at a time

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      # Only for apps that need their own dependencies image:
      - run: docker build -t my-site-deps:local -f deps.Dockerfile .
      - id: deploy
        uses: imeredith/site-deploy@main
        with:
          host: deploy@server.example.com # or ssh://deploy@server.example.com:2222
          ssh-key: ${{ secrets.SITE_DEPLOY_SSH_KEY }}
          known-hosts: ${{ secrets.SITE_DEPLOY_KNOWN_HOSTS }}
          token: ${{ secrets.SITE_DEPLOY_TOKEN }}
          name: my-site
          bundle: site # directory with compose.yaml, or a ready ZIP
          push: my-site-deps:local my-site-deps:2026-10-14
      - run: echo "Live at https://${{ steps.deploy.outputs.domain }}"
```

Inputs: `host`, `ssh-key`, `known-hosts` and `token` are required. `name` is the application, and leaving it empty only installs and configures `site-deploy` for later steps. `bundle` defaults to `.`. Set `replace: false` to fail instead of replacing an existing application. `push` takes one `LOCAL_IMAGE NAME:TAG` per line, and `registry-port` defaults to `9082`. The output `domain` is the deployed application's domain. Later steps in the same job can run `site-deploy` directly, for example `site-deploy logs my-site`.

The key, known hosts and token are written under `RUNNER_TEMP`, which the runner empties at the start and end of each job. Host keys missing from `known-hosts` are refused. On Linux and macOS runners, amd64 or arm64, the action downloads the released CLI and checks it against the release's `SHA256SUMS`, so it needs no Go. A download that doesn't match fails the step. Otherwise it builds the CLI, with the runner's Go when it is 1.21 or newer, or else after installing Go with `actions/setup-go`. A copy of the action checked out into the workspace and run as a local action always builds.

The daemon rejects CLIs older than itself, and the action's CLI comes from the ref a workflow names. With `@main`, push this repository to GitHub before upgrading a daemon that CI deploys to. A workflow pinned to an older tag needs a newer one once the daemon passes it.

## Versions and upgrades

`VERSION` holds the release version. `make build` stamps it, plus the commit as build metadata (`0.1.0+abc1234`, with `.dirty` for uncommitted changes). Metadata does not affect ordering. Bump `VERSION` for every build you install on a server.

**Releases:** when a `VERSION` bump reaches `main`, the [Release workflow](.github/workflows/release.yml) runs `make check`, builds static CLIs for Linux and macOS on amd64 and arm64, and publishes them as GitHub release `vVERSION` with a `SHA256SUMS` file. The [GitHub Action](#github-actions) uses them. A published release is never replaced, so a new build needs a new `VERSION`.

**The CLI must be at least as new as the daemon.** A release daemon reports its version in the `X-Site-Deploy-Version` header of authenticated responses and at `GET /v1/version`. Every other endpoint requires the `X-Site-Deploy-Client-Version` request header. If that version is missing, older than the daemon's, or `dev`, the daemon returns `426 Upgrade Required` before doing anything, so an outdated CLI can never apply a change. `site-deploy daemon-version` prints both versions and exits with status 1 if this CLI is too old. A `dev` daemon accepts any CLI for local development.

Because of that rule, always upgrade the CLI first, then the daemon. Newer CLIs must keep working against older daemons. When adding API features, gate them on the daemon's version: the API rejects unknown query parameters and endpoints.

`site-deploy upgrade` installs the CLI's own version as a host's daemon, so the CLI is always upgraded first. From a checkout:

```sh
make install-cli                  # the CLI, plus Linux server builds for amd64 and arm64 beside it
site-deploy upgrade HOST          # or --host HOST; installs only a newer version
site-deploy upgrade HOST --check  # report the versions and what it would do, and change nothing
make upgrade HOST=NAME            # both steps
```

Before installing, it compares three versions, ignoring build metadata:

- **The installed daemon's.** It installs only a newer version. When the host already runs this version it says so and does nothing. When the host runs a newer one it refuses: update the CLI first.
- **The repository's latest**, `VERSION` on the `main` branch of the GitHub repository (`SITE_DEPLOY_VERSION_URL` overrides the address). A CLI older than that is refused: update the checkout and run `make install-cli`. A newer CLI, not pushed yet, gets a warning, because workflows using the [GitHub Action](#github-actions) take their CLI from the repository and a newer daemon refuses it (HTTP 426). If the repository can't be reached, it warns and carries on.
- `--force` installs anyway: to reinstall the same version, downgrade, or install a CLI older than the repository's.

The server binary is the CLI itself when the server has the same CPU architecture, and otherwise the build that `make install-cli` puts in `PREFIX/lib/site-deploy/linux-ARCH/` (`--binary FILE` names another). It must be a Linux build of the CLI's exact version for the server's architecture, which the CLI reads from the binary's Go build information. The CLI sends it, the unit and [`deploy/install-host.sh`](deploy/install-host.sh) to the server through the host's remote shell in one SSH session, and runs the script with `sudo`. The SSH user needs passwordless sudo. `SITE_DEPLOY_SSH` (with `SITE_DEPLOY_RSH`) works in place of a configured host. `install-host.sh` also performs first installs, after which `upgrade` saves the new daemon's token as the host's token. It:

1. Keeps each release in `/usr/local/lib/site-deploy/VERSION/` and points `/usr/local/bin/site-deploy` at the active one. The newest five releases are kept.
2. Creates the `site-deploy` account if needed and installs the unit. It leaves drop-ins such as `/etc/systemd/system/site-deploy.service.d/*.conf` untouched, so put host-specific flags like `--caddy-admin` there.
3. Stops the daemon and archives the data directory to `/var/backups/site-deploy/TIMESTAMP-OLDVERSION.tar.gz`. The newest five archives are kept. Docker named volumes are not included.
4. Switches the symlink, restarts the daemon, and waits for it to report the new version. If it doesn't, the script logs the failure, restores the previous release, and exits nonzero. If the failed release had already migrated the data format, it also restores the data archive and keeps the failed copy beside it as `/var/lib/site-deploy.failed-TIMESTAMP`.

Application containers keep running while the daemon restarts. On start, the daemon reconciles saved applications against Docker. The gateway is down during the restart, so public requests fail for those few seconds.

The data directory records its layout version in `DATA_DIR/format` (currently `2`; 0.2.0 moved it from `1` because older daemons would serve `basic_auth` applications without authentication). A release that changes persisted state incompatibly must bump `store.DataFormat` and migrate older layouts in `Store.CheckFormat`. Daemons refuse to start on a newer format than they understand. That way, rolling back the binary cannot misread migrated state.

To roll back by hand, repoint the symlink and restart:

```sh
sudo ln -sfn /usr/local/lib/site-deploy/OLD/site-deploy /usr/local/bin/site-deploy
sudo systemctl restart site-deploy
```

If the newer release had migrated the data format, also restore the archive taken before the upgrade. Stop the daemon, empty `/var/lib/site-deploy`, then run `sudo tar -xzpf ARCHIVE -C /var/lib/site-deploy` before starting it again.

## Scope and validation

This is a trusted-operator, single-host system. The API token authorizes Docker deployments; deploy only trusted bundles and images. It does not provide hostile-tenant isolation. The Compose validator restricts host mounts to the operator's `--host-path` allowlist and to site-deploy volumes the operator created, and restricts namespaces, privileged mode, image builds, external resources, custom resource names, and unsupported Compose keys. Use image-based services, bundle-local files, allowed host paths, and project-owned volumes/networks. ZIP extraction rejects traversal, symlinks, special files, duplicate paths, and excessive expansion. Limits are 100 MiB per upload, 10,000 extracted paths, 128 MiB per file, and 512 MiB expanded total.

Run unit tests and static checks without creating Docker resources:

```sh
go test -race ./...
go vet ./...
```

The real Docker/Caddy lifecycle test is explicitly opt-in:

```sh
SITE_DEPLOY_INTEGRATION=1 go test -tags integration -v ./internal/integration
```

It requires Linux, a working Docker socket, Compose, and access to the test images. Defaults are `node:24-bookworm-slim` and `caddy:2`; override them with `SITE_DEPLOY_TEST_IMAGE` and `SITE_DEPLOY_TEST_CADDY_IMAGE`. The site-deploy volumes test also needs `SITE_DEPLOY_TEST_BTRFS_DIR`, a directory on btrfs, where it makes and removes its own volumes directory; without it, that test is skipped. The same variable enables the btrfs package's unit test, which needs no Docker or root: `SITE_DEPLOY_TEST_BTRFS_DIR=DIR go test ./internal/btrfs`. Each run uses a random namespace, temporary directories, and dynamically allocated ports, and removes only resources that it owns so concurrent agents' deployments remain separate.

See [architecture and lifecycle details](docs/architecture.md).
