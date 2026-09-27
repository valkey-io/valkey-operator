#!/bin/sh
# Uploads the snapshot in BACKUP_DIR to s3:BUCKET/PREFIX/<snapshot>/ and then
# prunes the oldest snapshots under the prefix beyond BACKUP_RETENTION.
# rclone reads the bucket credentials and endpoint from RCLONE_CONFIG_S3_*.
set -eu

: "${BACKUP_BUCKET:?}" "${BACKUP_PREFIX:?}"
BACKUP_DIR="${BACKUP_DIR:-/backup}"
BACKUP_RETENTION="${BACKUP_RETENTION:-0}"

snapshot=$(cat "$BACKUP_DIR/.snapshot")
dest="s3:$BACKUP_BUCKET/$BACKUP_PREFIX/$snapshot"

# The RDBs go first and the manifest last: a snapshot without manifest.json
# is incomplete and the restore side refuses it.
rclone copy "$BACKUP_DIR" "$dest" --include "shard-*.rdb"
rclone copyto "$BACKUP_DIR/manifest.json" "$dest/manifest.json"
echo "uploaded snapshot $snapshot to $dest"

if [ "$BACKUP_RETENTION" -gt 0 ]; then
  # Snapshot names sort by time, so everything but the last N is older.
  rclone lsf --dirs-only "s3:$BACKUP_BUCKET/$BACKUP_PREFIX/" | sed 's#/$##' | sort |
    awk -v keep="$BACKUP_RETENTION" '{ a[NR] = $0 } END { for (i = 1; i <= NR - keep; i++) print a[i] }' |
    while read -r old; do
      [ -n "$old" ] || continue
      echo "pruning snapshot $old"
      rclone purge "s3:$BACKUP_BUCKET/$BACKUP_PREFIX/$old"
    done
fi
