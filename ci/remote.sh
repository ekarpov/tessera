#!/usr/bin/env bash
# Copy committed source only; never send local configuration or credentials.
set -euo pipefail
cd "$(dirname "$0")/.."
[ ! -f ci/remote.local.env ] || . ci/remote.local.env
host="${TESSERA_CI_HOST:?set TESSERA_CI_HOST in ci/remote.local.env}"
key="${TESSERA_CI_KEY:?set TESSERA_CI_KEY in ci/remote.local.env}"
ssh_opts=(-i "$key" -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes)
status=$(git status --porcelain)
[ -z "$status" ] || { echo 'ci-remote: commit or stash working-tree changes first' >&2; exit 2; }
sha=$(git rev-parse --verify HEAD)
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
git bundle create "$tmp/source.bundle" HEAD
# Each invocation owns a unique directory; no shared checkout can be overwritten.
remote=$(ssh "${ssh_opts[@]}" "$host" 'mkdir -p /root/tessera-ci; mktemp -d /root/tessera-ci/run.XXXXXXXX')
[[ "$remote" =~ ^/root/tessera-ci/run\.[a-zA-Z0-9]+$ ]] || { echo 'ci-remote: unexpected remote path' >&2; exit 2; }
scp -q "${ssh_opts[@]}" "$tmp/source.bundle" "$host:$remote/source.bundle"
rc=0
ssh "${ssh_opts[@]}" "$host" "git clone -q '$remote/source.bundle' '$remote/tree' && cd '$remote/tree' && git checkout -q --detach '$sha' && export PATH=\"/usr/local/go/bin:\$PATH\" && flock /root/heavy.lock bash ci/run.sh" || rc=$?
mkdir -p ".ci/$sha"
scp -q -r "${ssh_opts[@]}" "$host:$remote/tree/.ci/$sha/." ".ci/$sha/" || { echo "ci-remote: logs unavailable; remote run retained at $remote" >&2; exit 1; }
if [ -f ".ci/$sha/summary.txt" ]; then
  git -c user.name=tessera-ci -c user.email=ci@localhost notes --ref=ci add -f -F ".ci/$sha/summary.txt" "$sha"
  cat ".ci/$sha/summary.txt"
fi
# Delete only this invocation's validated directory, after retrieving its logs.
ssh "${ssh_opts[@]}" "$host" "rm -rf -- '$remote'"
exit "$rc"
