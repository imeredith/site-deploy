#!/usr/bin/env bash
# Run by action.yml. Uses the released CLI for the action's VERSION, which
# github-action-release.sh downloaded, or else builds it from this checkout, so
# it always matches the action's ref. The SSH key, known_hosts and token go
# under RUNNER_TEMP, which the runner empties at the start and end of every
# job. Then it
# optionally pushes images and deploys a bundle. Later steps in the job can run
# site-deploy directly.
#
# Inputs arrive as INPUT_* variables (see action.yml), never interpolated into
# the script.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
: "${RUNNER_TEMP:?}" "${GITHUB_ENV:?}" "${GITHUB_PATH:?}" "${GITHUB_OUTPUT:?}"

fail() {
	echo "::error::$*"
	exit 1
}

for input in host ssh-key known-hosts token; do
	variable=INPUT_$(echo "$input" | tr a-z- A-Z_)
	[ -n "${!variable:-}" ] || fail "input $input is required"
done
case $INPUT_HOST in *[[:space:]]*) fail "input host must be a single SSH destination" ;; esac
case ${INPUT_REGISTRY_PORT:=9082} in '' | *[!0-9]*) fail "input registry-port must be a port number" ;; esac

state=$RUNNER_TEMP/site-deploy
rm -rf "$state"
mkdir -p "$state/bin"
chmod 700 "$state"

if [ -n "${RELEASED_CLI:-}" ]; then
	# Downloaded and checked against its release's SHA256SUMS.
	install -m 0755 "$RELEASED_CLI" "$state/bin/site-deploy"
	echo "Using the released $("$state/bin/site-deploy" version)"
else
	echo "::group::Build site-deploy $(cat "$root/VERSION")"
	# Same build as make build, written straight to RUNNER_TEMP so jobs sharing
	# this action's directory on a self-hosted runner never race.
	CGO_ENABLED=0 go build -C "$root" -trimpath \
		-ldflags "-X site-deploy/internal/version.Version=$(cat "$root/VERSION")" \
		-o "$state/bin/site-deploy" ./cmd/site-deploy
	echo "::endgroup::"
fi

(
	umask 077
	printf '%s\n' "$INPUT_SSH_KEY" | tr -d '\r' >"$state/key"
	printf '%s\n' "$INPUT_KNOWN_HOSTS" | tr -d '\r' >"$state/known_hosts"
	printf '%s\n' "$INPUT_TOKEN" | tr -d '\r' >"$state/token"
	cat >"$state/ssh_config" <<-EOF
		Host *
		  IdentityFile "$state/key"
		  IdentitiesOnly yes
		  UserKnownHostsFile "$state/known_hosts"
		  GlobalKnownHostsFile /dev/null
		  StrictHostKeyChecking yes
		  BatchMode yes
		  ConnectTimeout 20
	EOF
)

# For this script and for later steps. SITE_DEPLOY_CONFIG names a file that
# doesn't exist, so a self-hosted runner's own hosts stay out of it.
settings=(
	"SITE_DEPLOY_SSH=$INPUT_HOST"
	"SITE_DEPLOY_RSH=ssh -F '$state/ssh_config'"
	"SITE_DEPLOY_TOKEN_FILE=$state/token"
	"SITE_DEPLOY_REGISTRY_PORT=$INPUT_REGISTRY_PORT"
	"SITE_DEPLOY_CONFIG=$state/no-hosts.json"
)
for setting in "${settings[@]}"; do
	export "${setting?}"
	echo "$setting" >>"$GITHUB_ENV"
done
export PATH="$state/bin:$PATH"
echo "$state/bin" >>"$GITHUB_PATH"

# Fails early, with both versions, if the daemon is newer than this ref.
site-deploy daemon-version </dev/null ||
	fail "could not use the daemon at $INPUT_HOST; if the CLI is older than the daemon, use a newer ref of this action"

mapfile -t pushes <<<"$INPUT_PUSH"
for line in "${pushes[@]}"; do
	read -r image reference extra <<<"$line" || true
	case $image in '' | '#'*) continue ;; esac
	[ -n "${reference:-}" ] && [ -z "${extra:-}" ] || fail "push lines are LOCAL_IMAGE NAME:TAG, got: $line"
	echo "::group::Push $image as $reference"
	site-deploy push "$image" "$reference" </dev/null
	echo "::endgroup::"
done

name=${INPUT_NAME:-}
[ -n "$name" ] || exit 0

bundle=${INPUT_BUNDLE:-.}
if [ -d "$bundle" ]; then
	zip=$state/bundle.zip
	"$root/skills/site-deploy/scripts/bundle.sh" "$bundle" "$zip"
elif [ -f "$bundle" ]; then
	zip=$bundle
else
	fail "bundle $bundle is neither a directory nor a ZIP"
fi

if errors=$(site-deploy inspect "$name" 2>&1 >/dev/null </dev/null); then
	case ${INPUT_REPLACE:-true} in
	true)
		echo "Deleting the current deployment of $name (named volumes are kept)"
		site-deploy delete "$name" </dev/null
		;;
	false) fail "$name is already deployed; set replace: true to delete and deploy it again" ;;
	*) fail "input replace must be true or false" ;;
	esac
elif [[ $errors != *"HTTP 404"* ]]; then
	echo "$errors" >&2
	fail "could not check whether $name is deployed"
fi

echo "Deploying $name"
site-deploy deploy "$name" "$zip" --json </dev/null >"$state/deploy.json"
cat "$state/deploy.json"
domain=$(sed -nE 's/^    "domain": "([^"]*)".*/\1/p' "$state/deploy.json" | head -1)
echo "domain=$domain" >>"$GITHUB_OUTPUT"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	echo "Deployed **$name** to \`$domain\`" >>"$GITHUB_STEP_SUMMARY"
fi
