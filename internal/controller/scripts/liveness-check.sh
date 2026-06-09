#!/bin/sh
#
# Liveness check of a Valkey node (POSIX sh, works with bash, dash & busybox ash).
#
# Usage: liveness-check.sh [timeout] [port]
#        Default timeout is 4s, and default Valkey port is 6379.
set -e

timeout=${1:-"4"}
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

# Authenticate as the operator-managed user when configured. valkey-cli has
# no env var for the username, so it is passed explicitly; the password is read
# from REDISCLI_AUTH.
auth_args=""
if [ -n "${VALKEY_OPERATOR_USER:-}" ]; then
    auth_args="--user $VALKEY_OPERATOR_USER"
fi
if [ -n "${REDISCLI_AUTH:-}" ]; then
    auth_args="$auth_args --no-auth-warning"
fi

# Perform check
response=$(
    timeout_cmd $timeout \
    valkey-cli -h localhost -p $port $tls_args $auth_args PING)

responseFirstWord=$(echo "$response" | head -n1 | awk '{print $1;}')
if [ "$response" != "PONG" ] && [ "$responseFirstWord" != "LOADING" ] && [ "$responseFirstWord" != "MASTERDOWN" ]; then
    echo "$response" >&2
    exit 1
fi
