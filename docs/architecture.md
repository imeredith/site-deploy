# Architecture

`site-deploy` is a single-host deployment controller. One daemon owns a persistent data directory and controls a local Docker Engine through the Docker Compose CLI. Caddy and the daemon run in the host network namespace.

```text
Public HTTP/TLS
      |
      v
Caddy :80/:443 ──> gateway 127.0.0.1:9080 ──> application 127.0.0.1:<dynamic>
                         |                              |
                         | acquire/release request      | Compose project
                         v                              | <prefix><application>
CLI ──> API 127.0.0.1:9081 ──> orchestrator ──> Docker ──┘
          Bearer token             |
                                  v
                         persistent data directory
```

Caddy terminates TLS and matches registered application domains. Domains with `basic_auth` accounts get their own route, which runs Caddy's `authentication` handler (bcrypt, with a verified-credential cache) before proxying. Unauthenticated requests therefore never reach the gateway or wake an application. The Go gateway owns wake-up and activity tracking; no custom Caddy plugin is required. Unknown hosts receive 404. Only the selected service has a host-published port, bound to IPv4 loopback; other project services communicate through Docker networks.

## Deployment lifecycle

1. The authenticated API streams a ZIP upload to a private temporary file, bounded at 100 MiB.
2. Extraction into a fresh staging directory checks paths, file types, duplicates, checksums, and expansion limits. Sources must be regular files or directories inside the bundle.
3. The manifest package validates one root Compose file and its `x-site-deploy` extension before invoking Docker. It generates a configuration that replaces uploaded port publications with one loopback publication for the chosen service. Uploaded `restart: always` becomes `unless-stopped`; `deploy.restart_policy` is rejected so it cannot override suspension behavior.
4. The manager reserves the application name and domain, stores the generated configuration in private `compose/<name>.json`, moves the remaining staged files into the application's permanent directory, and persists its state. The root bundle filename `compose.generated.json` is reserved.
5. Docker runs `compose up --detach --no-build --wait` for the entire project. Compose healthchecks and a TCP connection to the routed port determine readiness. The application should supply a healthcheck when merely accepting TCP is insufficient.
6. The daemon saves the current upstream and running state. If Caddy synchronization is enabled, it regenerates the dedicated Caddy configuration. A configuration-sync failure is reported separately from the deployment result.

An application name identifies a stable Compose project. Each fresh data directory receives a random `sd-<24 hex digits>-` prefix, persisted in `project-prefix`, so independent installations can use the same application name on one Docker Engine. Existing installations retain their `sd-` prefix and volume identities; their application names must still differ across legacy installations sharing Docker. Names and domains must be unique within each daemon. Multiple daemons also need separate API and gateway listen ports and suitable Caddy routing.

## Suspension and requests

The manager serializes each application's lifecycle operations. Several requests arriving at a stopped application share one startup operation. Startup belongs to the daemon, so one disconnected client does not cancel a startup that other clients need. Each waiting request can still cancel its own wait.

After startup, the gateway forwards the original request body, host, and unchanged query string, including duplicate parameters and semicolons. A request holds a lease until the response finishes, including streaming responses and WebSockets. A lease prevents idle and explicit suspension. Deletion breaks leases instead: every lease of an application shares a context that deletion cancels. The gateway passes that cancellation to the proxied request, so `ReverseProxy` aborts responses and streams and closes upgraded connections. Deletion then waits up to 5 seconds for those requests to finish before stopping the project. Finishing a request resets the idle timer.

A browser page load (`GET` with `Sec-Fetch-Mode: navigate`) waits at most one second. If the application is still starting, the gateway returns a self-contained resuming page instead: HTTP 503 with `Retry-After` and `Cache-Control: no-store`, so caches and crawlers treat it as temporary. Startup continues because it belongs to the daemon. The page sends status checks to its own URL with an `X-Site-Deploy-Resume` header. The gateway answers each check itself after waiting up to ten seconds, with `running`, `starting`, or `failed`, and a check takes and releases a lease like any request, so it can wake the application and resets the idle timer. The page reloads on `running` and switches to a generic failure message on `failed`; it never reloads after a failure, which would restart a failing application in a loop. A page load during the startup-failure cooldown gets the failure page directly. Other requests wait for startup without a time limit of their own.

A failed upstream connection invalidates that running generation and returns 503 without replaying the request. The next request waits for existing leases to finish, then starts one shared recovery operation. Client cancellation and late failures from released leases do not invalidate a healthy replacement. Application HTTP error responses alone do not trigger a restart.

The idle worker scans every five seconds by default. A running application with no active leases is eligible once its `idle_timeout` has elapsed. The default `stop` mode runs `docker compose stop`, releasing the containers' memory while retaining containers, bind files, and named volumes. Optional `pause` freezes the containers while retaining their memory. A later request or `resume` reconciles the whole Compose project and waits for readiness again.

Idle accounting only sees requests through the gateway. Background work, direct connections to a database, and jobs inside containers do not acquire leases. Use `idle_timeout: 0` for always-on stacks. Dependencies within the same Compose project stop and resume together; there is no separate dependency activity timer. A manually suspended app still wakes on its next request.

Typical persisted states are `starting`, `running`, `stopping`, `suspended`, `deleting`, and `error`. Failed startup stops partially started containers and leaves an inspectable application record and logs for diagnosis. Automatic wake attempts share the failure for a two-second cooldown; an explicit `resume` bypasses the cooldown. Cleanup has a separate deadline of up to 30 seconds after a startup failure. Normal stop allows containers 20 seconds to exit before Docker terminates them. Connection failures trigger recovery on subsequent traffic; there is no continuous background reconciliation of externally changed containers. Use `resume` to reconcile a stack explicitly.

## Images

Applications run existing images; Compose's default pull policy fetches missing ones from their registry. With `--registry`, the daemon runs a `registry:3` container on a loopback address with storage in `DATA_DIR/registry`, running as the daemon's user so the daemon can read push times from storage. Bundles reference its images as `ADDRESS/NAME:TAG`. Operators push through an SSH forward on any local port (`push --registry-via`): the registry keys images by repository name, so the address used for the push doesn't matter.

Image cleanup holds a manager-wide lock exclusively; every project start holds it shared, so cleanup never removes an image between a pull and the container that uses it, and cleanup is skipped rather than delaying a start. Registry cleanup reads image references from the generated Compose files of all deployed applications (an unreadable file aborts it), deletes manifests whose tags are all unreferenced and older than the grace period, then stops the registry for offline `garbage-collect --delete-untagged` and restarts it. Host cleanup (`--prune-images`) runs `docker image prune --all`, relying on stopped containers of suspended applications to protect their images.

## Persistence and recovery

```text
DATA_DIR/
  .lock
  format
  token
  project-prefix
  state/<name>.json
  compose/<name>.json
  apps/<name>/<uploaded files>
  staging/<temporary uploads and extraction directories>
  registry/<private registry storage, with --registry>
```

An exclusive filesystem lock prevents two daemon processes from owning one data directory. `format` records the on-disk layout version. A daemon refuses to start on a format newer than it supports, and a release that changes the layout migrates older formats before loading state. State records are written through a temporary file, synced, and renamed. On daemon restart, saved applications are inspected against Docker, the upstream port is recovered, and the last-activity time is reset. Partially running or unhealthy projects are stopped before being marked suspended. Running containers are not intentionally stopped when the daemon exits. HTTP traffic is unavailable while its gateway is down.

Generated Compose files have owner-only permissions and live outside the bind-mounted application tree. Docker receives an explicit project directory so relative bundle paths still resolve correctly. Older saved configurations inside application directories are migrated using a fresh file, keeping open writable handles to the old file from changing the protected copy. This migration preserves the existing configuration rather than changing running containers' policies.

Delete runs Compose `down` without volume deletion, records a durable `deleting` marker, removes extracted files and protected configuration, and then releases the app record and domain. If file removal fails, the record stays available: correct ownership or permissions and retry `delete`. The retry works after a daemon restart even if the Compose file was already removed. Named volumes stay in Docker. Recreating the same application and declared volume names in the same data directory reuses that data; changing the application name or project prefix creates a different Compose project. Writable bind mounts into the extracted bundle disappear on deletion, so keep persistent data in named volumes. Backups need the daemon data directory, including `project-prefix`, and relevant Docker volumes.

Deploy currently rejects an existing name or domain. Replacement is delete followed by redeploy, with downtime. There is no rolling update, rollback, multi-host scheduling, database backup, or automatic volume garbage collection.

## Boundaries

The control API requires a Bearer token except for its minimal `/healthz` response. Authenticated responses carry the daemon version. A release daemon rejects any CLI older than itself with `426 Upgrade Required` before acting; `GET /v1/version` is exempt, so any CLI can report the mismatch. The default token is generated once with 256 bits of entropy and stored with mode `0600`. API and gateway addresses must be literal loopback IPs. Remote operators reach the API through SSH forwarding. Caddy's admin API also stays on loopback.

ZIP validation and the supported Compose subset prevent accidental path escapes and selected unsafe host features. Bind mounts of absolute host paths need an entry in the `--host-path` allowlist (`internal/hostpaths`) for that application, read-only unless the entry is writable. The path must exist (Compose's `create_host_path` is always off), be a regular file or directory, and contain no symlink. Docker resolves bind sources at every container start, and a container may change a writable tree in between, so the manager checks the generated file's host mounts again before each start. On daemon startup, running applications whose mounts the allowlist no longer allows are stopped and marked `error`. Compose commands use a controlled environment, an explicit project name, the generated file, and a local Unix Docker socket. Automatic `.env` discovery is disabled; application environment files must be declared explicitly. Docker runs existing images and never builds uploaded source.

These controls do not make uploaded code safe for hostile tenants. The authenticated operator can deploy arbitrary application code, allocate resources, and access Docker through the daemon. Treat bundles and images as trusted operator inputs. CPU and memory settings are Docker resource constraints, not scheduler reservations or a host-wide admission policy.

`--caddy-admin` opts into complete configuration replacement on a dedicated Caddy instance. Caddy uses its normal HTTP/TLS routing and proxies only configured hosts to the local gateway. Without that option, the operator owns generating and loading its JSON configuration. Caddy owns certificates separately from the orchestrator's data directory.

## Verification

Package tests cover extraction, manifest restrictions, Docker command construction, state storage, concurrent lifecycle operations, gateway behavior, authentication, and CLI requests. They use temporary paths and fake runtimes or HTTP servers.

The opt-in integration test uses real Docker and Caddy to exercise upload, routing, cold starts, request bodies, exact query preservation, crash recovery, protected configuration, project isolation, restart/resource settings, persistence, active-stream protection, and idle suspension. It chooses a random `sdtest-` namespace for each run and explicitly cleans up only its own resources. Run it with `SITE_DEPLOY_INTEGRATION=1 go test -tags integration -v ./internal/integration`; the build tag alone does not enable Docker operations.
