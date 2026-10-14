#!/usr/bin/env bash
# Build this checkout, install it as the local CLI, then install it on a server,
# built for the server's architecture (amd64 or arm64).
#
#   deploy/upgrade.sh [--force] HOST     e.g. deploy/upgrade.sh prod
#
# HOST is a host configured with `site-deploy host add`, so its remote shell
# (-e) is used for everything here. A plain SSH destination such as
# deploy@192.0.2.10 also works, with SITE_DEPLOY_RSH for its remote shell. The SSH user needs
# passwordless sudo on the server. The local CLI is upgraded before the daemon
# because a release daemon rejects CLIs older than itself. The new version
# must be newer than the running daemon's; --force allows reinstalling the
# same version or downgrading. On a first install to a configured host, the
# daemon's new token is copied into the host's token file.
set -euo pipefail

force=no
if [ "${1:-}" = --force ]; then
	force=yes
	shift
fi
target=${1:?usage: deploy/upgrade.sh [--force] HOST}
cd "$(dirname "$0")/.."

make build
cli=$PWD/bin/site-deploy
if "$cli" host show "$target" >/dev/null 2>&1; then
	configured=yes
	remote() { "$cli" --host "$target" ssh -- "$@"; }
else
	configured=no
	remote() { SITE_DEPLOY_SSH=$target "$cli" ssh -- "$@"; }
fi

new=$("$cli" version --json | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
running=$(remote 'sudo /usr/local/bin/site-deploy daemon-version --json --token-file /var/lib/site-deploy/token 2>/dev/null' </dev/null |
	sed -n 's/.*"daemon":"\([^"]*\)".*/\1/p' || true)
echo "Built ${new}; ${target} is running ${running:-no daemon}"

# Compare MAJOR.MINOR.PATCH only; build metadata does not order releases.
new_core=${new%%+*}
running_core=${running%%+*}
if [ -n "$running" ] && [ "$force" = no ]; then
	if [ "$new_core" = "$running_core" ] || ! printf '%s\n%s\n' "$running_core" "$new_core" | sort -V -C; then
		echo "upgrade.sh: $new is not newer than $running; bump VERSION, or pass --force to reinstall" >&2
		exit 1
	fi
fi

machine=$(remote uname -m </dev/null)
case $machine in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*)
	echo "upgrade.sh: unsupported server architecture $machine" >&2
	exit 1
	;;
esac
make server GOARCH=$arch

make install-cli
echo "Installed CLI $new to ${PREFIX:-$HOME/.local}/bin"

dir=$(remote mktemp -d </dev/null)
tar -cf - -C "$PWD/bin/linux-$arch" site-deploy -C "$PWD/deploy" install-host.sh site-deploy.service | remote "tar -xf - -C $dir"
remote "sudo bash $dir/install-host.sh $dir/site-deploy; status=\$?; rm -rf $dir; exit \$status" </dev/null

if [ -z "$running" ] && [ "$configured" = yes ]; then
	"$cli" host fetch-token "$target"
fi
