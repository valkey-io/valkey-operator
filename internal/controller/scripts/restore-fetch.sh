#!/bin/sh
# Stages this shard's RDB from the snapshot under DATA_DIR/.restore, unless the
# data dir already holds state. restore-install decides afterwards whether the
# staged file goes into place; it has valkey-cli, this image only has rclone.
# rclone reads the bucket credentials and endpoint from RCLONE_CONFIG_S3_*.
set -eu

: "${RESTORE_BUCKET:?}" "${RESTORE_PATH:?}" "${RESTORE_SHARD_INDEX:?}" "${RESTORE_EXPECTED_SHARDS:?}"
DATA_DIR="${DATA_DIR:-/data}"
staging="$DATA_DIR/.restore"
rm -rf "$staging"

aofdir="$DATA_DIR/${RESTORE_APPENDDIRNAME:-appendonlydir}"
if [ -e "$DATA_DIR/nodes.conf" ] || [ -e "$DATA_DIR/${RESTORE_DBFILENAME:-dump.rdb}" ] || [ -e "$aofdir/${RESTORE_APPENDFILENAME:-appendonly.aof}.manifest" ]; then
  echo "data dir already holds state, nothing to stage"
  exit 0
fi

src="s3:$RESTORE_BUCKET/$RESTORE_PATH"
mkdir -p "$staging"
rclone copyto "$src/manifest.json" "$staging/manifest.json"
shards=$(tr -d ' \n' < "$staging/manifest.json" | grep -o '"index":[0-9]*' | wc -l | tr -d ' ')
if [ "$shards" -ne "$RESTORE_EXPECTED_SHARDS" ]; then
  echo "snapshot $RESTORE_PATH has $shards shard(s), this cluster has $RESTORE_EXPECTED_SHARDS; a restore needs the same shard count" >&2
  exit 1
fi
file="shard-$RESTORE_SHARD_INDEX.rdb"
rclone copyto "$src/$file" "$staging/$file"
echo "staged $file from $src"
