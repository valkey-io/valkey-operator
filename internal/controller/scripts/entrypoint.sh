#!/bin/sh
#
# Entrypoint of the server container of a cluster node without persistence
# (POSIX sh, works with bash, dash & busybox ash).
#
# Usage: entrypoint.sh <valkey.conf> <further valkey-server arguments>
#
# Without persistence /data is an emptyDir, which outlives a restart of the
# server container while the dataset in memory does not. A primary whose
# server crashed would reload nodes.conf and come back as the same node, with
# the same slots and an empty dataset, and its replicas would full-sync to
# nothing. So when the previous run was a primary with a reachable replica:
#
# 1. The node identity and any snapshot are dropped, so the server joins as a
#    new, empty node, like a recreated pod.
# 2. The server is held back until every replica has been promoted or follows
#    another primary. Replicas reconnect by address, and the restarted
#    container keeps the pod address, so a wiped server that started at once
#    would be resynced from before the failover. The wait is capped at six
#    cluster-node-timeouts (90s by default) and at most 300s, which the
#    startup probe of the container allows for; after that the server starts
#    anyway.
#
# The operator then introduces the new node and attaches it to the promoted
# primary. A primary without a reachable replica keeps its identity: its data
# is gone either way, and keeping the slots keeps the cluster serving.
set -e

conf=$1
data_dir=${VALKEY_DATA_DIR:-/data}
nodes_conf="$data_dir/nodes.conf"

# config_value prints the last value of a directive in the rendered config,
# or the given default.
config_value() {
    awk -v key="$1" -v def="$2" '$1 == key { v = $2 } END { print (v == "" ? def : v) }' "$conf" 2>/dev/null
}

# replica_moved_on succeeds once the replica at $1:$2 reports itself a
# primary, or a replica of a primary other than this node.
replica_moved_on() {
    out=$(valkey-cli -h "$1" -p "$2" ${VALKEY_TLS_ARGS:-} ${auth_args:-} ROLE 2>/dev/null) || return 1
    role=$(printf '%s\n' "$out" | sed -n 1p | tr -d '\r')
    primary=$(printf '%s\n' "$out" | sed -n 2p | tr -d '\r')
    [ "$role" = "master" ] && return 0
    [ "$role" = "slave" ] && [ -n "$primary" ] && [ "$primary" != "$my_ip" ] && [ "$primary" != "$my_hostname" ] && return 0
    return 1
}

if [ -f "$nodes_conf" ]; then
    # nodes.conf lines: <id> <ip:port@cport[,hostname[,tls-port=N,...]]> <flags> <primary-id> ...
    # The flags of this node contain "myself"; a replica's fourth field names its primary.
    me=$(awk '$3 ~ /myself/ && $3 ~ /master/ { print $1 }' "$nodes_conf")
    if [ -n "$me" ]; then
        my_addr=$(awk '$3 ~ /myself/ { print $2 }' "$nodes_conf")
        my_ip=${my_addr%%:*}
        my_hostname=""
        case $my_addr in *@*,*) rest=${my_addr#*@}; rest=${rest#*,}; my_hostname=${rest%%,*} ;; esac
        # Reachable replicas of this node as ip:port, one per line. A replica
        # flagged fail or noaddr cannot be promoted, so it does not count.
        replicas=$(awk -v me="$me" '
            $3 ~ /slave/ && $3 !~ /(^|,)fail(,|$)/ && $3 !~ /noaddr/ && $4 == me {
                split($2, a, "@"); p = a[1]
                if (match($2, /tls-port=[0-9]+/)) { tp = substr($2, RSTART + 9, RLENGTH - 9); if (tp != "0") sub(/:[0-9]+$/, ":" tp, p) }
                print p
            }' "$nodes_conf")
        if [ -n "$replicas" ]; then
            echo "Previous run was primary $me with a replica; starting as a new node" >&2
            rm -f "$nodes_conf" "$data_dir/$(config_value dbfilename dump.rdb)" "$data_dir"/temp-*.rdb
            rm -rf "$data_dir/$(config_value appenddirname appendonlydir)"

            cap=$(( $(config_value cluster-node-timeout 15000) * 6 / 1000 ))
            [ "$cap" -gt 300 ] && cap=300
            [ "$cap" -lt 1 ] && cap=1
            auth_args=""
            if [ -n "${VALKEY_USER:-}" ] && [ -n "${VALKEYCLI_AUTH:-}" ]; then
                auth_args="--user $VALKEY_USER"
            fi
            waited=0
            for r in $replicas; do
                ip=${r%:*}; port=${r##*:}
                until replica_moved_on "$ip" "$port"; do
                    if [ "$waited" -ge "$cap" ]; then
                        echo "Replica $ip:$port still follows this node after ${cap}s; starting anyway" >&2
                        break
                    fi
                    sleep 1
                    waited=$((waited + 1))
                done
            done
            echo "Replica hand-off done after ${waited}s" >&2
        fi
    fi
fi

exec valkey-server "$@"
