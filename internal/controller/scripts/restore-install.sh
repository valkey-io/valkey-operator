#!/bin/sh
# Puts the RDB restore-fetch staged into the data dir, when it belongs there.
# It belongs there unless a cluster member behind VALKEY_HOST already serves
# any slot of this shard: then the shard has data and this pod is restarting
# into a running cluster, and it must rejoin instead. With no member listed
# at all, the cluster is being created and the RDB goes in. Anything else,
# members that do not answer or answer with an error, is not a reason to
# restore, so the container fails and is retried.
#
# With appendonly on, the RDB becomes the base file of a fresh multi-part AOF.
# The layout is built under the staging dir and moved into place in one
# rename, so a crash cannot leave a half-written directory the server would
# refuse or the next run would take for state.
set -eu

: "${VALKEY_HOST:?}" "${RESTORE_SHARD_INDEX:?}"
DATA_DIR="${DATA_DIR:-/data}"
VALKEY_PORT="${VALKEY_PORT:-6379}"
TLS_ARGS="${VALKEY_TLS_ARGS:-}"
staging="$DATA_DIR/.restore"
file="shard-$RESTORE_SHARD_INDEX.rdb"

if [ ! -e "$staging/$file" ]; then
  echo "nothing staged, not restoring"
  rm -rf "$staging"
  exit 0
fi

# This shard's slot ranges as the snapshot recorded them, space separated.
slots=$(tr -d '\n' < "$staging/manifest.json" | sed 's/},{/}\
{/g' | grep "\"index\":$RESTORE_SHARD_INDEX," | sed -n 's/.*"slots":"\([^"]*\)".*/\1/p')
if [ -z "$slots" ]; then
  echo "manifest has no slots for shard $RESTORE_SHARD_INDEX" >&2
  exit 1
fi

# Only Ready pods are behind the headless Service. A name with no address
# means the cluster is being created. Addresses that all refuse means a
# member is listed but not answering, or the resolver still has the address
# of a pod that just went away, and neither is a reason to load a snapshot:
# fail, and the next attempt asks again.
addresses=$(getent ahosts "$VALKEY_HOST" 2>/dev/null | awk '{print $1}' | sort -u)
if [ -z "$addresses" ]; then
  echo "no cluster member behind $VALKEY_HOST, the cluster is being created"
else
  nodes=""
  for address in $addresses; do
    # shellcheck disable=SC2086
    if nodes=$(timeout 5 valkey-cli --no-auth-warning $TLS_ARGS ${VALKEY_USER:+--user "$VALKEY_USER"} -h "$address" -p "$VALKEY_PORT" CLUSTER NODES 2>&1) && [ -n "$nodes" ]; then
      break
    fi
    case "$nodes" in
      *"Could not connect"*) echo "$address did not answer"; nodes="" ;;
      *) echo "cannot tell from $address whether the cluster already has data: $nodes" >&2; exit 1 ;;
    esac
  done
  if [ -z "$nodes" ]; then
    echo "members are listed behind $VALKEY_HOST but none answered, not restoring; will retry" >&2
    exit 1
  fi
  owner=$(printf '%s\n' "$nodes" | tr -d '\r' | awk -v want="$slots" '
    BEGIN {
      n = split(want, w, " ")
      for (i = 1; i <= n; i++) { split(w[i], p, "-"); ws[i] = p[1] + 0; we[i] = (p[2] == "" ? p[1] : p[2]) + 0 }
    }
    NF >= 9 && $3 ~ /master/ {
      for (j = 9; j <= NF; j++) {
        if ($j ~ /^\[/) continue
        split($j, q, "-"); s = q[1] + 0; e = (q[2] == "" ? q[1] : q[2]) + 0
        for (i = 1; i <= n; i++) if (s <= we[i] && ws[i] <= e) { print $2; exit }
      }
    }')
  if [ -n "$owner" ]; then
    echo "slots $slots are already served by $owner, not restoring"
    rm -rf "$staging"
    exit 0
  fi
  echo "a cluster member answered but nobody serves slots $slots yet"
fi

if [ "${RESTORE_APPENDONLY:-no}" = "yes" ]; then
  aofname="${RESTORE_APPENDFILENAME:-appendonly.aof}"
  dir="$DATA_DIR/${RESTORE_APPENDDIRNAME:-appendonlydir}"
  base="$aofname.1.base.rdb"
  mkdir -p "$staging/aof"
  mv "$staging/$file" "$staging/aof/$base"
  printf 'file %s seq 1 type b\n' "$base" > "$staging/aof/$aofname.manifest"
  mv "$staging/aof" "$dir"
  echo "loaded $file as AOF base $dir/$base"
else
  target="$DATA_DIR/${RESTORE_DBFILENAME:-dump.rdb}"
  mv "$staging/$file" "$target"
  echo "loaded $file as $target"
fi
rm -rf "$staging"
