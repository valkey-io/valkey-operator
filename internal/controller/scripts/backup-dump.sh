#!/bin/sh
# Takes one RDB per shard from the cluster behind VALKEY_HOST and writes them
# to BACKUP_DIR next to a manifest.json that records the slot layout.
#
# Discovery runs as the operator user. The SYNC behind valkey-cli --rdb runs
# as the replication user, the only system user allowed to SYNC. Each RDB is
# read from a replica of its shard unless BACKUP_SOURCE is Primary or the
# shard has no live replica.
#
# The per-shard RDBs are taken one after another, so the set is not one
# cluster-wide point in time. Each file is consistent on its own.
set -eu

: "${VALKEY_HOST:?}" "${VALKEY_OPERATOR_USER:?}" "${VALKEY_OPERATOR_PASSWORD:?}"
: "${VALKEY_REPLICATION_USER:?}" "${VALKEY_REPLICATION_PASSWORD:?}"
: "${CLUSTER_NAME:?}" "${NAMESPACE:?}"
VALKEY_PORT="${VALKEY_PORT:-6379}"
BACKUP_DIR="${BACKUP_DIR:-/backup}"
BACKUP_SOURCE="${BACKUP_SOURCE:-Replica}"
TLS_ARGS="${VALKEY_TLS_ARGS:-}"

# shellcheck disable=SC2086
cli() { valkey-cli --no-auth-warning $TLS_ARGS "$@"; }
op() { VALKEYCLI_AUTH="$VALKEY_OPERATOR_PASSWORD" cli --user "$VALKEY_OPERATOR_USER" "$@"; }

info=$(op -h "$VALKEY_HOST" -p "$VALKEY_PORT" CLUSTER INFO | tr -d '\r')
state=$(printf '%s\n' "$info" | awk -F: '$1=="cluster_state"{print $2}')
slots_ok=$(printf '%s\n' "$info" | awk -F: '$1=="cluster_slots_ok"{print $2}')
size=$(printf '%s\n' "$info" | awk -F: '$1=="cluster_size"{print $2}')
if [ "$state" != "ok" ] || [ "$slots_ok" != "16384" ]; then
  echo "refusing to snapshot: cluster_state=$state cluster_slots_ok=$slots_ok" >&2
  exit 1
fi
nodes=$(op -h "$VALKEY_HOST" -p "$VALKEY_PORT" CLUSTER NODES | tr -d '\r')
version=$(op -h "$VALKEY_HOST" -p "$VALKEY_PORT" INFO server | tr -d '\r' | awk -F: '$1=="valkey_version"{print $2}')

mkdir -p "$BACKUP_DIR"
rm -f "$BACKUP_DIR"/shard-*.rdb "$BACKUP_DIR/manifest.json"
stamp=$(date -u +%Y-%m-%dT%H-%M-%SZ)
created=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '%s' "$stamp" > "$BACKUP_DIR/.snapshot"

# Primaries that own slots, ordered by their first slot. That order is the
# shard index the restore side uses. Field 9 onwards are the slot ranges;
# entries in brackets are migrations in flight and are not ownership. The
# flags are a comma-separated list, and "fail" is not "nofailover".
printf '%s\n' "$nodes" | awk '
  function failing(flags,   f, k, m) { m = split(flags, f, ","); for (k = 1; k <= m; k++) if (f[k] == "fail" || f[k] == "fail?") return 1; return 0 }
  $3 ~ /master/ && !failing($3) && NF >= 9 {
    first = $9; sub(/-.*/, "", first)
    slots = ""
    for (i = 9; i <= NF; i++) if ($i !~ /^\[/) slots = slots (slots == "" ? "" : " ") $i
    if (slots != "") print first, $1, $2, slots
  }' | sort -n | cut -d' ' -f2- > "$BACKUP_DIR/.primaries"

# CLUSTER NODES writes an address as ip:port@cport, with ",hostname" behind
# it under hostname announce, and IPv6 without brackets. Dial the ip, which is
# always there: drop the suffixes and split on the last colon.
host_of() { a=${1%%,*}; a=${a%%@*}; printf '%s' "${a%:*}"; }
port_of() { a=${1%%,*}; a=${a%%@*}; printf '%s' "${a##*:}"; }

i=0
entries=""
while read -r id addr slots; do
  source_id=$id
  source_addr=$addr
  role=primary
  if [ "$BACKUP_SOURCE" = "Replica" ]; then
    # Field 8 is the cluster bus link, not replication, so ask each candidate
    # whether its replication link is up before reading its dataset.
    printf '%s\n' "$nodes" | awk -v m="$id" '
      function failing(flags,   f, k, n) { n = split(flags, f, ","); for (k = 1; k <= n; k++) if (f[k] == "fail" || f[k] == "fail?") return 1; return 0 }
      $3 ~ /slave/ && !failing($3) && $4 == m { print $1, $2 }' | while read -r rid raddr; do
      link=$(op -h "$(host_of "$raddr")" -p "$(port_of "$raddr")" INFO replication 2>/dev/null | tr -d '\r' | awk -F: '$1=="master_link_status"{print $2}')
      if [ "$link" = "up" ]; then
        printf '%s %s\n' "$rid" "$raddr" > "$BACKUP_DIR/.replica"
        break
      fi
      echo "replica $raddr of shard $i has replication link '$link', skipping it"
    done
    if [ -s "$BACKUP_DIR/.replica" ]; then
      read -r source_id source_addr < "$BACKUP_DIR/.replica"
      role=replica
    fi
    rm -f "$BACKUP_DIR/.replica"
  fi
  host=$(host_of "$source_addr")
  port=$(port_of "$source_addr")
  file="shard-$i.rdb"
  echo "shard $i (slots $slots): reading RDB from $role $source_addr"
  VALKEYCLI_AUTH="$VALKEY_REPLICATION_PASSWORD" cli --user "$VALKEY_REPLICATION_USER" -h "$host" -p "$port" --rdb "$BACKUP_DIR/$file"
  entry=$(printf '{"index":%d,"file":"%s","slots":"%s","primary":"%s","source":"%s","sourceRole":"%s"}' "$i" "$file" "$slots" "$id" "$source_id" "$role")
  entries="$entries${entries:+,}$entry"
  i=$((i + 1))
done < "$BACKUP_DIR/.primaries"
rm -f "$BACKUP_DIR/.primaries"

if [ "$i" -ne "$size" ]; then
  echo "found $i primaries with slots but cluster_size is $size" >&2
  exit 1
fi
printf '{"version":1,"cluster":"%s","namespace":"%s","snapshot":"%s","createdAt":"%s","valkeyVersion":"%s","shards":[%s]}\n' \
  "$CLUSTER_NAME" "$NAMESPACE" "$stamp" "$created" "$version" "$entries" > "$BACKUP_DIR/manifest.json"
echo "snapshot $stamp: $i shard(s), $(du -sh "$BACKUP_DIR" | cut -f1)"
