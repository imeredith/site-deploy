#!/usr/bin/env bash
# Run by action.yml before anything else. Downloads the released CLI for this
# action's VERSION and the runner's platform from the action's own repository
# (.github/workflows/release.yml publishes it), checks it against the release's
# SHA256SUMS, and prints its path as the output "path". When there's no such
# release yet, or the action isn't one from GitHub, it prints no path and the
# action builds the CLI from source instead. A download that doesn't match its
# checksum fails the step.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
: "${RUNNER_TEMP:?}" "${GITHUB_OUTPUT:?}"
version=$(cat "$root/VERSION")

fallback() {
	echo "$*; building site-deploy $version from source instead"
	exit 0
}

# github.action_repository is empty inside some composite actions, so fall back
# to the path the runner checks actions out to: .../_actions/OWNER/REPO/REF.
repository=${ACTION_REPOSITORY:-}
if [ -z "$repository" ] && [[ $root =~ /_actions/([^/]+)/([^/]+)/[^/]+$ ]]; then
	repository=${BASH_REMATCH[1]}/${BASH_REMATCH[2]}
fi
[[ $repository =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fallback "This copy of the action isn't from a GitHub repository"

case ${RUNNER_OS:-}/${RUNNER_ARCH:-} in
Linux/X64) platform=linux-amd64 ;;
Linux/ARM64) platform=linux-arm64 ;;
macOS/X64) platform=darwin-amd64 ;;
macOS/ARM64) platform=darwin-arm64 ;;
*) fallback "There is no release build for ${RUNNER_OS:-this OS} on ${RUNNER_ARCH:-this architecture}" ;;
esac

directory=$RUNNER_TEMP/site-deploy-release
rm -rf "$directory"
mkdir -p "$directory"
base=https://github.com/$repository/releases/download/v$version
fetch() {
	curl -fsSL --retry 3 --connect-timeout 20 -o "$directory/$1" "$base/$1"
}
fetch SHA256SUMS || fallback "$repository has no release v$version yet"
fetch "site-deploy-$platform" || fallback "Release v$version of $repository has no site-deploy-$platform"

if command -v sha256sum >/dev/null; then
	sum=(sha256sum)
else
	sum=(shasum -a 256)
fi
if ! (cd "$directory" && grep -E " [*]?site-deploy-$platform\$" SHA256SUMS | "${sum[@]}" -c -); then
	echo "::error::site-deploy-$platform from release v$version of $repository doesn't match its SHA256SUMS"
	exit 1
fi
chmod 0755 "$directory/site-deploy-$platform"
echo "path=$directory/site-deploy-$platform" >>"$GITHUB_OUTPUT"
