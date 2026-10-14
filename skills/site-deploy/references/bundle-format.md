# Bundle format

A bundle is a ZIP whose **root** contains exactly one of `compose.yaml`, `compose.yml`, `docker-compose.yaml` or
`docker-compose.yml`, plus every file the services bind-mount. The authoritative rules live in the validator
(`internal/manifest/` in the site-deploy repo). Upload errors name the offending key, so read them literally.

## Contents
- [x-site-deploy](#x-site-deploy)
- [What the validator allows](#what-the-validator-allows)
- [Getting code into a container without building](#getting-code-into-a-container-without-building)
- [Examples](#examples): static site, Node app with data, app + Postgres, password-protected site, compiled app on a dependencies-only image
- [Limits](#limits)

## x-site-deploy

```yaml
x-site-deploy:
  domain: myapp.example.com   # required; hostname only, no scheme/port/path/wildcard
  service: web            # required; the Compose service that receives HTTP
  port: 8080              # required; port INSIDE that container; app must listen on 0.0.0.0
  idle_timeout: 5m        # default 5m; time since last finished request; 0 = never suspend
  start_timeout: 60s      # default 60s; deadline for compose up + healthchecks + TCP connect
  suspend_mode: stop      # default stop (frees memory); pause keeps memory, faster wake
  basic_auth:             # optional (daemon 0.2.0+); omit for a public site
    - user: preview
      hash: $2a$14$...    # bcrypt hash, written as-is (no $$ escaping)
```

## What the validator allows

- **Top-level keys:** `services`, `volumes`, `networks`, `configs`, `secrets`, `x-site-deploy` (and the legacy `version`).
- **Images only.** Every service needs `image:`. `build:` is not a supported key, because the server never builds uploaded source.
- **Bind mounts** must be relative paths that exist inside the ZIP (`./public:/usr/share/nginx/html:ro`). `~`, `..`,
  `$` and symlinks are rejected. `env_file`, `configs.file` and `secrets.file` follow the same rule.
- **Host paths** (absolute bind sources such as `/srv/media:/media:ro`) work only if the server's operator has allowed
  them with `serve --host-path` (README, "Host paths"). Entries are read-only unless marked `:rw`, so add `:ro`. They
  may be limited to named apps. The path must already exist and contain no symlinks. Check the server notes or ask before
  relying on one. A refused mount fails the deploy and names the allowed paths.
- **Named volumes and networks** must be project-owned: no `external`, no custom `name:`. Volume driver `local`
  only, network driver `bridge` only. Named volumes are where persistent data belongs, because they survive delete/redeploy.
- **No custom resource names:** `container_name` is not a supported key.
- **Ports:** any uploaded `ports:` are replaced. Only the routed service gets an ephemeral `127.0.0.1` port.
  Services talk to each other by service name on the Compose network.
- **Restart policy:** `restart: always` becomes `unless-stopped`, so Docker doesn't wake idle apps. `deploy.restart_policy`
  is rejected.
- **Also rejected:** `privileged: true`, host/container `network_mode` (only `none` on non-routed services),
  `security_opt` other than `no-new-privileges`.
- **Supported and passed through:** `command`, `entrypoint`, `environment`, `env_file`, `depends_on` (with
  `condition: service_healthy`), `healthcheck`, `user`, `read_only`, `tmpfs`, `init`, `cpus`, `mem_limit`,
  `deploy.resources`, `logging`, `labels`, and most other ordinary service settings.
- **Interpolation:** image names, paths and resource references must be literal. Compose `${VAR}` there is rejected.
  The bundle's `.env` is **not** loaded implicitly; use `env_file: ./app.env` or explicit `environment:` values.
  Write `$$` where a command needs a literal `$` inside the container.
- `compose.generated.json` is a reserved filename at the ZIP root.

## Getting code into a container without building

The server never builds images. Split every app into **what changes on each release** (your code) and **what
it runs on** (its dependencies):

- **Static site:** run the site's build locally (`npm run build`, `hugo`, …), copy the output into `public/`,
  and serve it with `nginx:alpine` (example 1).
- **Interpreted app** (Node/Python/Ruby): bind-mount the source into the official runtime image. Install
  dependencies locally first, so `node_modules` or vendor directories ship in the bundle (example 2). Watch the size
  limits. Native modules must be built for linux/amd64 against a glibc compatible with the image, so prefer
  `-bookworm` images over `-alpine` when shipping prebuilt native dependencies.
- **Compiled app, or one that needs system packages** (Chromium, ffmpeg, …): build the program locally and put the
  binary in the bundle. Build a **dependencies-only** image (no app code) and push it to the private registry
  (example 5). Tag it by its dependencies, for example by date or by a hash of the Dockerfile, and push a new tag
  only when they change. Releasing new code is then just a new bundle.
- **Pushing:** `site-deploy push LOCAL_IMAGE NAME:TAG` pushes through an SSH tunnel and prints the reference to use:
  `127.0.0.1:9082/NAME:TAG`. The registry is only reachable on the server's loopback. Each push uses its own random
  local port, so pushes can run in parallel. Only layers the registry lacks are sent. `site-deploy images` lists
  what's stored and which apps use it.
- **Don't** copy images onto the server with `docker save | ssh docker load`, and don't set `pull_policy: never`. Cleanup
  removes host images that no container uses, for example between the `delete` and `deploy` of a release. After that only
  images that can be pulled come back.

Keep a `healthcheck` on anything slower to become ready than "accepts TCP". site-deploy waits for healthchecks
before routing traffic. Without one, the TCP check passes as soon as the container starts, because Docker's port proxy
accepts connections before the app listens. A slow app then answers its first request after a wake with a plain 503
instead of the "Starting up" page.

Make containers stop promptly. A Node or Python process running as PID 1 ignores SIGTERM, so every delete,
redeploy and suspend waits Docker's 20-second stop timeout before the container is killed. Add `init: true`, or use
an entrypoint such as `tini`. A test app's delete dropped from 20s to 0.4s with it.

## Examples

### 1. Static site

```
mysite/
  compose.yaml
  public/index.html ...
```
```yaml
x-site-deploy:
  domain: mysite.example.com
  service: web
  port: 80

services:
  web:
    image: nginx:alpine
    volumes:
      - ./public:/usr/share/nginx/html:ro
    mem_limit: 64m
```

### 2. Node app with persistent data

```yaml
x-site-deploy:
  domain: counter.example.com
  service: web
  port: 8080
  idle_timeout: 15m

services:
  web:
    image: node:24-bookworm-slim
    command: [node, /app/server.js]
    working_dir: /app
    init: true
    environment:
      PORT: "8080"          # listen on 0.0.0.0:8080
      DATA_DIR: /data
    volumes:
      - ./app:/app:ro       # source + node_modules, from the bundle
      - data:/data          # survives delete/redeploy under the same app name
    mem_limit: 256m
    healthcheck:
      test: [CMD, node, -e, "fetch('http://127.0.0.1:8080/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"]
      interval: 5s
      timeout: 3s
      retries: 5
      start_period: 5s

volumes:
  data: {}
```

### 3. App + Postgres (suspend and wake together)

```yaml
x-site-deploy:
  domain: notes.example.com
  service: app
  port: 3000

services:
  app:
    image: ghcr.io/example/notes:1.4.2
    env_file: ./app.env               # DATABASE_URL=postgres://notes:...@db:5432/notes
    depends_on:
      db:
        condition: service_healthy
  db:
    image: postgres:17-bookworm
    environment:
      POSTGRES_USER: notes
      POSTGRES_PASSWORD: change-me    # stored in the bundle on the server; fine for personal use
      POSTGRES_DB: notes
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: [CMD-SHELL, "pg_isready -U notes -d notes"]
      interval: 5s
      retries: 10

volumes:
  pgdata: {}
```

The database stops when the app idles. Raise `idle_timeout`, or use `0`, if cold starts with Postgres are too slow.

### 4. Password-protected site

Add `basic_auth` to `x-site-deploy`. This needs daemon 0.2.0 or newer; older daemons reject the key. Caddy checks
the password before anything reaches site-deploy, so bots and scanners get a 401 without waking the app.

```yaml
x-site-deploy:
  domain: preview.example.com
  service: web
  port: 80
  basic_auth:
    - user: preview
      hash: $2a$14$O4UeNfvX7xUhP9mj6tjWIOkvvB/sTqKipfaXtgWqmJ7MZjMg/l9me   # add more {user, hash} entries as needed

services:
  web:
    image: nginx:alpine
    volumes:
      - ./public:/usr/share/nginx/html:ro
```

- **Hash, never plaintext.** Generate the hash with `site-deploy --host NAME ssh -- caddy hash-password --plaintext 'PASSWORD'`
  where Caddy is installed on the server, or locally with `docker run --rm caddy:2-alpine caddy hash-password --plaintext 'PASSWORD'`. Plaintext is rejected.
- **Paste the hash as-is.** `x-site-deploy` never goes through Compose, so `$` must not be doubled.
  Quoting the hash in YAML is optional.
- **Usernames** use letters, digits and `. _ @ + -` (no `:`), at most 64 characters. Each app can have up to 64 accounts.
- **Changing accounts means redeploying** (delete + deploy). `list` shows `basic auth (N)` in ACCESS, and
  `inspect` shows usernames, never hashes.
- **Passwords:** if the user didn't pick one, generate a strong one (`openssl rand -base64 18`), tell them what it is,
  and don't write it into any file. Only the hash goes in the bundle.

### 5. Compiled app on a dependencies-only image

A Rust (or Go, …) program that needs Chromium at runtime. Built and pushed once, and again only when the
dependencies change:

```dockerfile
# deps.Dockerfile: what the program runs on, never the program itself
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends chromium ca-certificates tini \
 && rm -rf /var/lib/apt/lists/* && useradd --create-home --uid 10001 app
USER app
```
```sh
docker build -f deps.Dockerfile -t render-deps .
site-deploy push render-deps render-deps:2026-10-13
```

Each release builds the program locally, for the same Debian release as the image, into `app/`, and deploys this
bundle:

```yaml
x-site-deploy:
  domain: render.example.com
  service: web
  port: 3000
  idle_timeout: 0            # background work doesn't count as activity

services:
  web:
    image: 127.0.0.1:9082/render-deps:2026-10-13
    entrypoint: [tini, --, /app/render, serve]
    volumes:
      - ./app:/app:ro        # the program and frontend, from the bundle
      - data:/data
    mem_limit: 2g

volumes:
  data: {}
```

## Limits

100 MiB ZIP upload, 10,000 paths, 128 MiB per file, 512 MiB expanded in total. Names: lowercase letter first, at most 48
of `[a-z0-9-]`. The routed service must run exactly one instance.
