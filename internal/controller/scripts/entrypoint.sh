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
# 1. The server is held back until every such replica has been promoted or
#    follows another primary. Replicas reconnect by address, and the restarted
#    container keeps the pod address, so a wiped server that started at once
#    would be resynced from before the failover. The wait is capped at six
#    cluster-node-timeouts (90s by default) and at most 300s, which the
#    startup probe of the container allows for.
# 2. Once every replica has moved on, the node identity and any snapshot are
#    dropped, so the server joins as a new, empty node, like a recreated pod,
#    and the operator attaches it to the promoted primary. If a replica has
#    not moved on by the cap, the identity and the snapshot are kept: an
#    empty new node would leave the slots with a dead ID, while the old
#    identity keeps the cluster serving.
#
# A primary without a reachable replica keeps its identity for the same
# reason. A replica flagged fail, noaddr or nofailover does not count.
set -e

conf=$1
data_dir=${VALKEY_DATA_DIR:-/data}
case $data_dir in ""|/) echo "Refusing to use '$data_dir' as the data dir" >&2; exit 1 ;; esac
nodes_conf="$data_dir/nodes.conf"
# Seconds a single ROLE call may take, connection and reply included.
role_timeout=3

# config_value prints the last value of a directive in the rendered config,
# or the given default.
config_value() {
    awk -v key="$1" -v def="$2" '$1 == key { v = $2 } END { print (v == "" ? def : v) }' "$conf" 2>/dev/null
}

# timeout_cmd runs a command with a DURATION (seconds) limit. Polls every
# 0.1s; on timeout sends SIGTERM then SIGKILL. Returns the command's exit
# code, or 124 on timeout.
timeout_cmd() {
    duration=$1; shift
    "$@" &
    cmdpid=$!
    count=0
    max_count=$((duration * 10))
    while [ $count -lt $max_count ]; do
        if ! kill -0 $cmdpid 2>/dev/null; then
            wait $cmdpid
            return $?
        fi
        sleep 0.1
        count=$((count + 1))
    done
    kill -TERM $cmdpid 2>/dev/null
    sleep 0.1
    kill -0 $cmdpid 2>/dev/null && sleep 1 && kill -KILL $cmdpid 2>/dev/null
    wait $cmdpid 2>/dev/null
    return 124
}

# replica_moved_on succeeds once the replica at $1:$2 reports itself a
# primary, or a replica of a primary other than this node. The answer goes
# through a file rather than a pipe, so a call that is cut short by the
# timeout cannot hold the read open through a child of its own.
replica_moved_on() {
    out_file=$(mktemp)
    if ! timeout_cmd "$role_timeout" valkey-cli -t "$role_timeout" -h "$1" -p "$2" ${VALKEY_TLS_ARGS:-} ${auth_args:-} ROLE > "$out_file" 2>/dev/null; then
        rm -f "$out_file"
        return 1
    fi
    role=$(sed -n 1p "$out_file" | tr -d '\r')
    primary=$(sed -n 2p "$out_file" | tr -d '\r')
    rm -f "$out_file"
    [ "$role" = "master" ] && return 0
    [ "$role" = "slave" ] && [ -n "$primary" ] && [ "$primary" != "$my_ip" ] && [ "$primary" != "$my_hostname" ] && return 0
    return 1
}

# drop_identity removes the node identity and any snapshot, so the server
# starts as a new, empty node.
drop_identity() {
    rm -f "$nodes_conf" "$data_dir/$(config_value dbfilename dump.rdb)" "$data_dir"/temp-*.rdb
    rm -rf "$data_dir/$(config_value appenddirname appendonlydir)"
}

if [ -f "$nodes_conf" ]; then
    # nodes.conf lines: <id> <ip:port@cport[,hostname[,tls-port=N,...]]> <flags> <primary-id> ...
    # The flags of this node contain "myself"; a replica's fourth field names its primary.
    me=$(awk '$3 ~ /myself/ && $3 ~ /master/ { print $1 }' "$nodes_conf")
    if [ -n "$me" ]; then
        my_addr=$(awk '$3 ~ /myself/ { print $2 }' "$nodes_conf")
        # ip:port is everything before the cluster bus port; the port sits
        # after the last colon, so an IPv6 address keeps its own colons.
        my_endpoint=${my_addr%%@*}
        my_ip=${my_endpoint%:*}
        my_hostname=""
        case $my_addr in *@*,*) rest=${my_addr#*@}; rest=${rest#*,}; my_hostname=${rest%%,*} ;; esac
        # Replicas of this node that can be promoted, as ip:port, one per
        # line. A replica flagged fail or noaddr cannot be reached, and one
        # flagged nofailover will not promote itself, so they do not count.
        replicas=$(awk -v me="$me" '
            $3 ~ /slave/ && $3 !~ /(^|,)fail(,|$)/ && $3 !~ /noaddr/ && $3 !~ /nofailover/ && $4 == me {
                split($2, a, "@"); p = a[1]
                if (match($2, /tls-port=[0-9]+/)) { tp = substr($2, RSTART + 9, RLENGTH - 9); if (tp != "0") sub(/:[0-9]+$/, ":" tp, p) }
                print p
            }' "$nodes_conf")
        if [ -n "$replicas" ]; then
            cap=$(( $(config_value cluster-node-timeout 15000) * 6 / 1000 ))
            [ "$cap" -gt 300 ] && cap=300
            [ "$cap" -lt 1 ] && cap=1
            auth_args=""
            if [ -n "${VALKEY_USER:-}" ] && [ -n "${VALKEYCLI_AUTH:-}" ]; then
                auth_args="--user $VALKEY_USER"
            fi
            echo "Previous run was primary $me with a replica; waiting up to ${cap}s for the replica hand-off" >&2
            started=$(date +%s)
            deadline=$((started + cap))
            handoff_ok=true
            for r in $replicas; do
                ip=${r%:*}; port=${r##*:}
                until replica_moved_on "$ip" "$port"; do
                    if [ "$(date +%s)" -ge "$deadline" ]; then
                        handoff_ok=false
                        break
                    fi
                    sleep 1
                done
                [ "$handoff_ok" = true ] || break
            done
            waited=$(( $(date +%s) - started ))
            if [ "$handoff_ok" = true ]; then
                echo "Replica hand-off done after ${waited}s; starting as a new node" >&2
                drop_identity
            else
                echo "Replica $ip:$port still follows this node after ${waited}s; keeping the previous identity" >&2
            fi
        fi
    fi
fi

exec valkey-server "$@"
