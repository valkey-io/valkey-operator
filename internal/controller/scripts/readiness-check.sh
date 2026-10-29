#!/bin/sh
#
# Readiness check of a Valkey node (POSIX sh, works with bash, dash & busybox ash).
#
# Usage: readiness-check.sh [timeout] [port]
#        Default timeout is 1s, and default Valkey port is 6379.
set -e

timeout=${1:-"1"}
port=${2:-"6379"}

# Run a command with a DURATION (seconds) timeout. Polls every 0.1s; on timeout
# sends SIGTERM then SIGKILL. Returns the command's exit code, or 124 on timeout.
timeout_cmd() {
    duration=$1; shift
    "$@" &
    cmdpid=$!

	# Poll every 0.1 seconds
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

    # Timeout reached
    kill -TERM $cmdpid 2>/dev/null
    sleep 0.1
    kill -0 $cmdpid 2>/dev/null && sleep 1 && kill -KILL $cmdpid 2>/dev/null
    wait $cmdpid 2>/dev/null
    return 124
}

# Build TLS args from environment variables if set
tls_args=""
if [ -n "${VALKEY_TLS_ARGS:-}" ]; then
    tls_args="$VALKEY_TLS_ARGS"
fi

# Authenticate as the custom user when user and password are set.
auth_args=""
if [ -n "${VALKEY_USER:-}" ] && [ -n "${VALKEYCLI_AUTH:-}" ]; then
    auth_args="--user $VALKEY_USER"
fi

# valkey-cli with the probe's TLS and auth args. $tls_args and $auth_args are
# intentionally unquoted: they expand to zero or more flag words.
valkey_cli() {
	# shellcheck disable=SC2086
	timeout_cmd "$timeout" valkey-cli -h localhost -p "$port" $tls_args $auth_args "$@"
}

# First field value of an INFO line ("role:slave" -> "slave").
info_field() {
	printf '%s\n' "$1" | tr -d '\r' | awk -F: -v key="$2" '$1 == key { print substr($0, length(key) + 2); exit }'
}

# Perform checks
response=$(valkey_cli PING || true)

if [ "$response" != "PONG" ]; then
    echo "$response" >&2
    exit 1
fi

# Slotless nodes stay unready until they are caught-up replicas, so the PDB
# waits for the sync (#552). A myself,master line with a slot field is already
# serving. Standalone (cluster_enabled:0) stays PING-only.
info=$(valkey_cli INFO || true)
cluster_enabled=$(info_field "$info" cluster_enabled)
if [ "$cluster_enabled" = "0" ]; then
	exit 0
fi
if [ "$cluster_enabled" != "1" ]; then
	echo "INFO did not report cluster_enabled" >&2
	exit 1
fi

nodes=$(valkey_cli CLUSTER NODES || true)
if [ -z "$nodes" ]; then
	echo "CLUSTER NODES failed" >&2
	exit 1
fi

# Slot fields start at the 9th column. Only a primary's marker counts:
# replicas can carry migration markers without holding the shard's data.
has_slots=$(printf '%s\n' "$nodes" | tr -d '\r' | awk '
	{
		n = split($3, flags, ",")
		is_me = 0
		is_master = 0
		for (i = 1; i <= n; i++) {
			if (flags[i] == "myself") is_me = 1
			if (flags[i] == "master") is_master = 1
		}
		if (is_me && is_master && NF >= 9) { print "yes"; exit }
	}
')
if [ "$has_slots" = "yes" ]; then
	exit 0
fi

role=$(info_field "$info" role)
link=$(info_field "$info" master_link_status)
syncing=$(info_field "$info" master_sync_in_progress)
loading=$(info_field "$info" loading)
if [ "$role" = "slave" ] && [ "$link" = "up" ] && [ "$syncing" != "1" ] && [ "$loading" != "1" ]; then
	exit 0
fi

echo "node has no slots and is not a caught-up replica (role=${role:-unknown} link=${link:-unknown} sync=${syncing:-unknown} loading=${loading:-unknown})" >&2
exit 1
