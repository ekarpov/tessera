#!/bin/sh
# One encrypted Tessera backup, for cron, a systemd timer, or a hand.
#
# THIS TAKES THE SERVER DOWN. The engine holds an exclusive lock on its
# database, so a backup cannot run beside a live server: a second process is
# refused with dataset_unavailable. A backup job that did not stop the server
# would fail every night and leave a log nobody reads and no backups at all,
# which is worse than no job.
#
# Keep the window short and the failure loud: if anything here fails, the
# server is restarted and the script exits non-zero so the scheduler notices.
set -eu

CONFIG="${TESSERA_CONFIG:-/etc/tessera/server.json}"
KEY="${TESSERA_BACKUP_KEY:-/etc/tessera/backup.key}"
DESTINATION="${TESSERA_BACKUP_DIR:-/var/backups/tessera}"
SERVICE="${TESSERA_SERVICE:-tesserad.service}"
KEEP="${TESSERA_BACKUP_KEEP:-14}"

archive="$DESTINATION/tessera-$(date -u +%Y-%m-%dT%H%M%SZ).enc"
mkdir -p "$DESTINATION"
chmod 700 "$DESTINATION"

restart() { systemctl start "$SERVICE" || true; }

systemctl stop "$SERVICE"
trap restart EXIT INT TERM
tesserad -config "$CONFIG" -backup "$archive" -backup-key-file "$KEY"
trap - EXIT INT TERM
restart

# Only prune once the new archive exists, so a failed backup never costs an
# old one.
test -s "$archive"
ls -1t "$DESTINATION"/tessera-*.enc 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
    rm -f -- "$old"
done

# The archive is encrypted in process and the key is the only way back. Copy
# both off this machine, and never into the same place: a backup that dies
# with the host is a copy, not a backup.
echo "$archive"
