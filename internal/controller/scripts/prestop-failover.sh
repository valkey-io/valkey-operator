#!/bin/sh
#
# preStop hook of a Valkey node (POSIX sh, works with bash, dash & busybox ash).
#
# Kubernetes runs it before SIGTERM. On a cluster primary with an online
# replica it sends that replica a plain CLUSTER FAILOVER and waits for this
# node to become a replica. The primary is still up and votes, so the handoff
# also works in 1- and 2-shard clusters, where the election started by
# shutdown-on-sigterm failover cannot reach a majority once the primary has
# exited. It always exits 0, so shutdown-on-sigterm stays as the fallback.
#
# Usage: prestop-failover.sh [port]
#        Default Valkey port is 6379.

port=${1:-"6379"}

# Run a command with a DURATION (seconds) timeout. Polls every 0.1s; on timeout
# sends SIGTERM then SIGKILL. Returns the command's exit code, or 124 on timeout.
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

# The hook's own output is not kept; write to the server's log instead.
log() {
    echo "preStop: $*" > /proc/1/fd/1 2>/dev/null || echo "preStop: $*" >&2
}

tls_args=""
if [ -n "${VALKEY_TLS_ARGS:-}" ]; then
    tls_args="$VALKEY_TLS_ARGS"
fi

auth_args=""
if [ -n "${VALKEY_USER:-}" ] && [ -n "${VALKEYCLI_AUTH:-}" ]; then
    auth_args="--user $VALKEY_USER"
fi

cli() {
    host=$1; cport=$2; shift 2
    timeout_cmd 2 valkey-cli -h "$host" -p "$cport" $tls_args $auth_args "$@"
}

local_role() {
    cli localhost "$port" INFO replication | tr -d '\r' | sed -n 's/^role://p'
}

[ "$(local_role)" = "master" ] || exit 0

# The online replica with the highest offset, as "ip port". INFO replication
# lists each replica as slaveN:ip=...,port=...,state=...,offset=...,lag=...;
# splitting on "," and the first "=" keeps an IPv6 address whole.
target=$(cli localhost "$port" INFO replication | tr -d '\r' | awk '
    /^slave[0-9]+:/ {
        sub(/^slave[0-9]+:/, "")
        ip = ""; rport = ""; state = ""; offset = -1
        n = split($0, fields, ",")
        for (i = 1; i <= n; i++) {
            eq = index(fields[i], "=")
            key = substr(fields[i], 1, eq - 1)
            value = substr(fields[i], eq + 1)
            if (key == "ip") ip = value
            else if (key == "port") rport = value
            else if (key == "state") state = value
            else if (key == "offset") offset = value + 0
        }
        if (state == "online" && offset > best) { best = offset; choice = ip " " rport }
    }
    BEGIN { best = -1 }
    END { if (choice != "") print choice }')

if [ -z "$target" ]; then
    log "no online replica, leaving the handoff to shutdown-on-sigterm"
    exit 0
fi
replica_ip=${target% *}
replica_port=${target##* }

# Wait for as long as the replica may take: cluster-manual-failover-timeout,
# rounded up to seconds, plus a second for the role change to show.
timeout_ms=$(cli localhost "$port" CONFIG GET cluster-manual-failover-timeout | tr -d '\r' | sed -n 2p)
case "$timeout_ms" in
    ''|*[!0-9]*) timeout_ms=5000 ;;
esac
wait_s=$(( (timeout_ms + 999) / 1000 + 1 ))

reply=$(cli "$replica_ip" "$replica_port" CLUSTER FAILOVER | tr -d '\r')
if [ "$reply" != "OK" ]; then
    log "CLUSTER FAILOVER on $replica_ip:$replica_port failed: $reply"
    exit 0
fi
log "asked $replica_ip:$replica_port to take over, waiting up to ${wait_s}s"

deadline=$(( $(date +%s) + wait_s ))
while [ "$(date +%s)" -lt "$deadline" ]; do
    if [ "$(local_role)" = "slave" ]; then
        log "handed the primary role to $replica_ip:$replica_port"
        exit 0
    fi
    sleep 0.2
done
log "the handoff to $replica_ip:$replica_port did not finish within ${wait_s}s"
exit 0
