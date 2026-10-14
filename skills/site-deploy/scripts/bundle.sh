#!/usr/bin/env bash
# Package a site-deploy bundle: zip the CONTENTS of DIR, so the Compose file
# sits at the ZIP root, then check the result against the server's limits.
#
#   bundle.sh DIR OUT.zip
#
# Skips .git/. Symlinks are stored as the files they point to, because the
# server rejects symlinks inside bundles.
set -euo pipefail

usage="usage: bundle.sh DIR OUT.zip"
dir=${1:?$usage}
out=${2:?$usage}
[ -d "$dir" ] || { echo "bundle.sh: $dir is not a directory" >&2; exit 1; }
command -v zip >/dev/null || { echo "bundle.sh: zip is not installed" >&2; exit 1; }
case $out in /*) ;; *) out=$PWD/$out ;; esac

composes=()
for name in compose.yaml compose.yml docker-compose.yaml docker-compose.yml; do
	[ -f "$dir/$name" ] && composes+=("$name")
done
if [ ${#composes[@]} -ne 1 ]; then
	echo "bundle.sh: $dir must contain exactly one of compose.yaml, compose.yml, docker-compose.yaml, docker-compose.yml (found ${#composes[@]})" >&2
	exit 1
fi
compose=${composes[0]}
grep -q '^x-site-deploy:' "$dir/$compose" ||
	{ echo "bundle.sh: $compose has no top-level x-site-deploy: block (domain, service, port)" >&2; exit 1; }
grep -qE '^[[:space:]]+build:' "$dir/$compose" &&
	{ echo "bundle.sh: $compose uses build:, but the server only runs prebuilt images" >&2; exit 1; }
grep -qE '^[[:space:]]+pull_policy:[[:space:]]*["'\'']?never' "$dir/$compose" &&
	echo "bundle.sh: warning: pull_policy: never breaks redeploys once cleanup removes the unused image; push it to the registry instead" >&2
[ -e "$dir/compose.generated.json" ] &&
	{ echo "bundle.sh: compose.generated.json is a reserved filename" >&2; exit 1; }

rm -f "$out"
(cd "$dir" && zip -qr -X "$out" . -x '.git/*' '*/.git/*')

size=$(stat -c %s "$out")
listing=$(unzip -Z1 "$out")
paths=$(printf '%s\n' "$listing" | wc -l)
expanded=$(unzip -l "$out" | tail -1 | awk '{print $1}')
printf '%s\n' "$listing" | grep -qx "$compose" ||
	{ echo "bundle.sh: $compose is not at the ZIP root" >&2; exit 1; }
[ "$size" -le $((100 << 20)) ] || { echo "bundle.sh: ZIP is $((size >> 20)) MiB; the upload limit is 100 MiB" >&2; exit 1; }
[ "$paths" -le 10000 ] || { echo "bundle.sh: $paths paths; the limit is 10000" >&2; exit 1; }
[ "$expanded" -le $((512 << 20)) ] || { echo "bundle.sh: expands to $((expanded >> 20)) MiB; the limit is 512 MiB" >&2; exit 1; }
echo "Wrote $out: $paths paths, $((size / 1024)) KiB zipped, $((expanded / 1024)) KiB expanded"
