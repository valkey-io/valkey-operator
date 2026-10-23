#!/bin/sh
#
# Starts the server of a cluster node without persistence
# (POSIX sh, works with bash, dash & busybox ash).
#
# Usage: server-start.sh <valkey.conf> <further valkey-server arguments>
#
# Without persistence /data is an emptyDir, which outlives a restart of the
# server container while the dataset in memory does not. A primary whose
# server crashed would reload nodes.conf and come back as the same node, with
# the same slots and an empty dataset, and its replicas would full-sync to
# nothing. So when the previous run was a primary with a replica that can be
# promoted, this script:
#
#   1. waits until every such replica is a primary or follows another node.
#      Replicas reconnect by address, and the restarted container keeps the
#      pod address, so a wiped server that started at once would be synced
#      from before the failover. The wait is capped at six
#      cluster-node-timeouts (at most 600s), which the startup probe allows
#      for;
#   2. then empties /data, so the server starts as a new, empty node, like a
#      recreated pod, and the operator attaches it to the promoted primary.
#      If a replica has not moved on by the cap, the identity and the
#      snapshot are kept instead: an empty new node would leave the slots
#      with a dead ID, while the old identity keeps the cluster serving.
#
# Any other node, a replica, a primary without such a replica, or a first
# start, runs the server untouched.
set -e

conf=$1
data_dir=${VALKEY_DATA_DIR:-/data}
nodes_conf="$data_dir/nodes.conf"
role_timeout=3 # seconds one ROLE call may take, connection and reply included

case $data_dir in ""|/) echo "Refusing to use '$data_dir' as the data dir" >&2; exit 1 ;; esac

# config_value KEY DEFAULT prints the last value of KEY in the rendered config.
config_value() {
    awk -v key="$1" -v def="$2" '$1 == key { v = $2 } END { print (v == "" ? def : v) }' "$conf" 2>/dev/null
}

# timeout_cmd SECONDS COMMAND... runs the command with a time limit. It polls
# every 0.1s and on timeout sends SIGTERM, then SIGKILL. Returns the command's
# exit code, or 124 on timeout. Same as in the probe scripts.
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

# nodes.conf has one line per node:
#
#   <id> <ip:port@cport[,hostname[,...]]> <flags> <primary-id> ... <slots>
#
# The flags of this node contain "myself". A replica carries "slave" and the
# id of its primary in the fourth field. Valkey writes the legacy names.

# previous_primary_id prints this node's id when the previous run was a
# primary, and nothing otherwise.
previous_primary_id() {
    awk '$3 ~ /myself/ && $3 ~ /master/ { print $1 }' "$nodes_conf"
}

# own_address prints this node's ip and hostname, space separated. The port
# sits after the last colon, so an IPv6 address keeps its own colons.
own_address() {
    awk '$3 ~ /myself/ {
        split($2, parts, "@")                 # ip:port | cport,hostname,...
        endpoint = parts[1]
        sub(/:[0-9]+$/, "", endpoint)         # drop the port
        n = split(parts[2], extra, ",")       # extra[2] is the hostname, if any
        print endpoint, (n >= 2 ? extra[2] : "")
    }' "$nodes_conf"
}

# promotable_replicas ID prints the replicas of primary ID that can take
# over, as ip:port, one per line. A replica flagged fail or noaddr cannot be
# reached, and one flagged nofailover will not promote itself.
promotable_replicas() {
    awk -v me="$1" '
        $4 != me || $3 !~ /slave/ { next }
        $3 ~ /(^|,)fail(,|$)/ || $3 ~ /noaddr/ || $3 ~ /nofailover/ { next }
        {
            split($2, parts, "@")
            address = parts[1]                # ip:port
            if (match($2, /tls-port=[0-9]+/)) {
                tls_port = substr($2, RSTART + 9, RLENGTH - 9)
                if (tls_port != "0") sub(/:[0-9]+$/, ":" tls_port, address)
            }
            print address
        }' "$nodes_conf"
}

# replica_moved_on IP PORT succeeds once the replica reports itself a primary,
# or a replica of a primary other than this node. The reply goes through a
# file under the data dir, since the root filesystem may be read-only and a
# call cut short by the timeout must not hold a pipe open.
replica_moved_on() {
    reply="$data_dir/.role-reply"
    if ! timeout_cmd "$role_timeout" valkey-cli -t "$role_timeout" -h "$1" -p "$2" ${VALKEY_TLS_ARGS:-} ${auth_args:-} ROLE > "$reply" 2>/dev/null; then
        rm -f "$reply"
        return 1
    fi
    role=$(sed -n 1p "$reply" | tr -d '\r')
    primary=$(sed -n 2p "$reply" | tr -d '\r')
    rm -f "$reply"
    case $role in
        master) return 0 ;;
        slave) [ -n "$primary" ] && [ "$primary" != "$my_ip" ] && [ "$primary" != "$my_hostname" ] ;;
        *) return 1 ;;
    esac
}

# wait_for_handoff SECONDS waits until every replica in $replicas has moved
# on. Succeeds when all did within the time, fails on the first that did not.
wait_for_handoff() {
    deadline=$(( $(date +%s) + $1 ))
    for replica in $replicas; do
        ip=${replica%:*}
        port=${replica##*:}
        until replica_moved_on "$ip" "$port"; do
            if [ "$(date +%s)" -ge "$deadline" ]; then
                echo "Replica $ip:$port still follows this node" >&2
                return 1
            fi
            sleep 1
        done
    done
}

# drop_identity empties the data dir: nodes.conf, the snapshot and the
# append-only dir, so the server starts as a new, empty node.
drop_identity() {
    rm -rf "${data_dir:?}"/*
}

if [ -f "$nodes_conf" ] && me=$(previous_primary_id) && [ -n "$me" ]; then
    replicas=$(promotable_replicas "$me")
    if [ -n "$replicas" ]; then
        address=$(own_address)
        my_ip=${address%% *}
        my_hostname=${address#* }
        auth_args=""
        if [ -n "${VALKEY_USER:-}" ] && [ -n "${VALKEYCLI_AUTH:-}" ]; then
            auth_args="--user $VALKEY_USER"
        fi
        # Six cluster-node-timeouts, at most 600s. The startup probe of the
        # container allows for the same, see startupFailureThreshold.
        cap=$(( $(config_value cluster-node-timeout 15000) * 6 / 1000 ))
        [ "$cap" -gt 600 ] && cap=600
        [ "$cap" -lt 1 ] && cap=1
        echo "Previous run was primary $me with a replica; waiting up to ${cap}s for the replica hand-off" >&2
        started=$(date +%s)
        if wait_for_handoff "$cap"; then
            echo "Replica hand-off done after $(( $(date +%s) - started ))s; starting as a new node" >&2
            drop_identity
        else
            echo "Keeping the previous identity after $(( $(date +%s) - started ))s" >&2
        fi
    fi
fi

exec valkey-server "$@"
