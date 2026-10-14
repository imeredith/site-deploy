#!/usr/bin/env bash
# Install or upgrade site-deploy on this host. Run as root with the new binary;
# site-deploy.service must sit next to this script. deploy/upgrade.sh copies
# both here and runs:
#
#   sudo ./install-host.sh ./site-deploy
#
# Every release is kept in /usr/local/lib/site-deploy/VERSION/ and
# /usr/local/bin/site-deploy points at the active one. The daemon is stopped
# and its data directory archived to /var/backups/site-deploy before the
# switch. Application containers keep running while the daemon restarts, but
# the gateway is down, so public requests fail for those few seconds. If the
# new daemon does not report the new version, the previous release is restored,
# along with the data backup if the new release had already migrated the data
# format (an older daemon refuses a newer format).
#
# Host-specific daemon flags belong in a systemd drop-in, such as
# /etc/systemd/system/site-deploy.service.d/host.conf, which this script keeps.
set -euo pipefail

binary=${1:?usage: install-host.sh BINARY}
here=$(cd "$(dirname "$0")" && pwd)
releases=/usr/local/lib/site-deploy
link=/usr/local/bin/site-deploy
data=/var/lib/site-deploy
backups=/var/backups/site-deploy
keep=${SITE_DEPLOY_KEEP:-5}

[ "$(id -u)" = 0 ] || { echo "install-host.sh: run as root" >&2; exit 1; }

# $1 = binary; prints its version.
binary_version() { "$1" version --json | sed -n 's/.*"version":"\([^"]*\)".*/\1/p'; }
# $1 = expected version; waits for the running daemon to report it.
running_is() {
	local running
	for _ in $(seq 1 30); do
		running=$("$link" daemon-version --json --token-file "$data/token" 2>/dev/null |
			sed -n 's/.*"daemon":"\([^"]*\)".*/\1/p' || true)
		[ "$running" = "$1" ] && return 0
		sleep 1
	done
	return 1
}
# $1 = release binary; atomically repoints the active symlink.
activate() { ln -sfn "$1" "$link.new" && mv -T "$link.new" "$link"; }
# Replaces the data directory with the pre-upgrade archive, keeping the failed
# release's copy beside it for inspection.
restore_data() {
	local aside="$data.failed-$(date -u +%Y%m%dT%H%M%SZ)"
	mv "$data" "$aside"
	install -d -m 0700 -o site-deploy -g site-deploy "$data"
	tar -C "$data" -xzpf "$archive"
	# Registry storage is not archived; keep the images it already holds.
	[ ! -d "$aside/registry" ] || mv "$aside/registry" "$data/registry"
	echo "Restored $data from $archive; the failed release's data is in $aside" >&2
}

new=$(binary_version "$binary")
case $new in
'' | dev*) echo "install-host.sh: refusing unversioned build '$new'; build with make" >&2; exit 1 ;;
esac
previous=$(readlink "$link" 2>/dev/null || true)
old=none
[ -n "$previous" ] && old=$(binary_version "$previous")

install -d -m 0755 "$releases/$new"
# Install via rename: the target may be the running executable on a reinstall.
install -m 0755 "$binary" "$releases/$new/.site-deploy.tmp"
mv -f "$releases/$new/.site-deploy.tmp" "$releases/$new/site-deploy"

id site-deploy >/dev/null 2>&1 ||
	useradd --system --user-group --home-dir "$data" --shell /usr/sbin/nologin site-deploy
install -m 0644 "$here/site-deploy.service" /etc/systemd/system/site-deploy.service
systemctl daemon-reload

systemctl stop site-deploy 2>/dev/null || true
old_format=$(cat "$data/format" 2>/dev/null || true)
archive=""
if [ -d "$data" ]; then
	install -d -m 0700 "$backups"
	archive="$backups/$(date -u +%Y%m%dT%H%M%SZ)-$old.tar.gz"
	# Registry storage can be large and its images can be pushed again.
	tar -C "$data" --exclude=./.lock --exclude='./staging/*' --exclude=./registry -czf "$archive" .
	chmod 0600 "$archive"
	echo "Backed up $data to $archive (Docker volumes and registry images are not included)"
	ls -1t "$backups"/*.tar.gz | tail -n +$((keep + 1)) | xargs -r rm --
fi

activate "$releases/$new/site-deploy"
systemctl enable --quiet site-deploy
systemctl restart site-deploy

if running_is "$new"; then
	echo "site-deploy $new is running (previous: $old)"
	# Keep the newest releases, always including the active and previous ones.
	ls -1dt "$releases"/*/ | tail -n +$((keep + 1)) | while read -r dir; do
		dir=${dir%/}
		[ "$dir" = "$releases/$new" ] || [ "$dir" = "$(dirname "$previous")" ] || rm -rf -- "$dir"
	done
	exit 0
fi

echo "install-host.sh: site-deploy $new did not come up. Recent log:" >&2
journalctl -u site-deploy -n 30 --no-pager >&2 || true
if [ -n "$previous" ]; then
	echo "Rolling back to $old" >&2
	systemctl stop site-deploy
	activate "$previous"
	if [ -n "$archive" ] && [ "$(cat "$data/format" 2>/dev/null || true)" != "$old_format" ]; then
		restore_data
	fi
	systemctl restart site-deploy
	if running_is "$old"; then
		echo "Rolled back to $old" >&2
	else
		echo "Rollback to $old also failed. Restore the data backup $archive" >&2
	fi
fi
exit 1
