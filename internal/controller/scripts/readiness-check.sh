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

# The mounted file is the current _operator password. It updates when the
# Secret rotates, without a container restart. The environment variable is
# fixed at container start and is the fallback when the file is not mounted.
pwfile="/etc/valkey/system-passwords/_operator"
if [ -f "$pwfile" ]; then
    # A file the process cannot read must not fail the probe. set -e would
    # exit here and drop the password from the environment.
    pw=$(cat "$pwfile" 2>/dev/null) || true
    if [ -n "$pw" ]; then
        VALKEYCLI_AUTH=$pw
        export VALKEYCLI_AUTH
    fi
fi

# Authenticate as the custom user when user and password are set.
auth_args=""
if [ -n "${VALKEY_USER:-}" ] && [ -n "${VALKEYCLI_AUTH:-}" ]; then
    auth_args="--user $VALKEY_USER"
fi

# Perform checks
response=$(
    timeout_cmd $timeout \
    valkey-cli -h localhost -p $port $tls_args $auth_args PING)

if [ "$response" != "PONG" ]; then
    echo "$response" >&2
    exit 1
fi

# valkey_status_file=/tmp/.valkey_cluster_check
# if [ ! -f "$valkey_status_file" ]; then
#     response=$(
#         timeout $timeout \
#         valkey-cli -h localhost -p $port CLUSTER INFO | grep cluster_state | tr -d '[:space:]')

#     if [ "$response" != "cluster_state:ok" ]; then
#         echo "$response" >&2
#         exit 1
#     else
#         touch "$valkey_status_file"
#     fi
# fi
