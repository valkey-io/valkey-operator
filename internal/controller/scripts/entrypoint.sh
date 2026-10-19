#!/bin/sh
#
# Entrypoint of the server container of a ValkeyNode without persistence
# (POSIX sh, works with bash, dash & busybox ash).
#
# Usage: entrypoint.sh <valkey-server arguments>
#
# Without persistence /data is an emptyDir, which outlives a restart of the
# server container while the dataset in memory does not. A primary whose
# server crashed would reload nodes.conf and come back as the same node, with
# the same slots and an empty dataset, and its replicas would full-sync to
# nothing. So when the previous run was a primary with a replica, the node
# identity is dropped before the server starts: the node joins as a new,
# empty node, the replica is promoted and keeps the data, and the operator
# attaches the new node to it. A primary without a replica keeps its identity:
# its data is gone either way, and keeping the slots keeps the cluster serving.
set -e

data_dir=${VALKEY_DATA_DIR:-/data}
nodes_conf="$data_dir/nodes.conf"

if [ -f "$nodes_conf" ]; then
    # nodes.conf lines: <id> <address> <flags> <primary-id> ...; flags of this
    # node contain "myself", and a replica's fourth field names its primary.
    me=$(awk '$3 ~ /myself/ && $3 ~ /master/ { print $1 }' "$nodes_conf")
    if [ -n "$me" ] && awk -v me="$me" '$3 ~ /slave/ && $4 == me { found = 1 } END { exit !found }' "$nodes_conf"; then
        echo "Previous run was primary $me with a replica; starting as a new node" >&2
        rm -f "$nodes_conf" "$data_dir/dump.rdb"
        rm -rf "$data_dir/appendonlydir"
    fi
fi

exec valkey-server "$@"
