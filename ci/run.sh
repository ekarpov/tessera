#!/usr/bin/env bash
# Full Tessera gate against a clean commit; synthetic disposable PostgreSQL.
set -euo pipefail
cd "$(dirname "$0")/.."
status=$(git status --porcelain)
[ -z "$status" ] || { echo 'ci: commit or stash working-tree changes first' >&2; exit 2; }
sha=$(git rev-parse --verify HEAD)
mkdir -p .ci
exec 9>.ci/run.lock
flock -n 9 || { echo 'ci: another run is active' >&2; exit 2; }
logs="$PWD/.ci/$sha"
mkdir -p "$logs"
state=$(mktemp -d)
container=
cleanup() {
  if [ -n "$container" ]; then docker rm -f -v "$container" >/dev/null 2>&1 || true; fi
  rm -rf -- "$state"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Password file rather than a password in Docker's environment or logs.
(umask 077; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$state/password")
chmod 644 "$state/password" # The container's postgres user must be able to read it.
container=$(docker run -d --name "tessera-ci-${sha:0:12}-$$" \
  -p 127.0.0.1::5432 -e POSTGRES_USER=tessera -e POSTGRES_DB=tessera \
  -e POSTGRES_PASSWORD_FILE=/run/secrets/password \
  -v "$state/password:/run/secrets/password:ro" postgres:17-bookworm)
ready=no
for ((i=0; i<60; i++)); do
  if docker exec "$container" pg_isready -h 127.0.0.1 -U tessera -d tessera >/dev/null 2>&1; then ready=yes; break; fi
  sleep 1
done
[ "$ready" = yes ] || { echo 'ci: PostgreSQL did not become ready' >&2; exit 1; }
docker exec "$container" createdb -U tessera tessera_restore
port=$(docker port "$container" 5432/tcp)
password=$(cat "$state/password")
export TESSERA_TEST_POSTGRES_DSN="postgres://tessera:$password@$port/tessera?sslmode=disable"
export TESSERA_TEST_POSTGRES_RESTORE_DSN="postgres://tessera:$password@$port/tessera_restore?sslmode=disable"
# Do not echo DSNs through Make's release-check recipe.
rc=0
make --silent release-check >"$logs/release-check.log" 2>&1 || rc=$?
verdict=pass; [ "$rc" -eq 0 ] || verdict=fail
printf 'ci: %s\ncommit: %s\nran: %s\ngate: make release-check (SQLite and PostgreSQL)\n' \
  "$verdict" "$sha" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$logs/summary.txt"
git -c user.name=tessera-ci -c user.email=ci@localhost notes --ref=ci add -f -F "$logs/summary.txt" "$sha"
cat "$logs/summary.txt"
exit "$rc"
