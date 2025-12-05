#!/bin/bash
#
# Readiness check of a Valkey node (Bash only).
#
# Usage: readiness-check.sh [timeout] [port]
#        Default timeout is 1s, and default Valkey port is 6379.
set -e

timeout=${1:-"1"}
port=${2:-"6379"}

# timeout DURATION COMMAND [ARG]...
function timeout {
    local duration=$1; shift

    # Run command and get its pid.
    "$@" &
    local cmdpid=$!

    # Start the timeout supervisor (subshell in parallell).
    (
        remaining=$((duration * 1000)) # Use millisec
        while [ "$remaining" -gt "0" ]; do
            kill -0 $cmdpid || exit 0 # exit subshell if cmd finished.
            sleep 0.1
            remaining=$((remaining - 100))
        done

        echo "Command timed out"
        # First try a SIGTERM, then force terminate using SIGKILL.
        kill -s SIGTERM $cmdpid && sleep 0.1 && kill -0 $cmdpid || exit 0
        sleep 1
        kill -s SIGKILL $cmdpid
    ) 2>/dev/null &

    # Wait for jobs (avoiding <defunct>)
    wait
}

# Perform checks
response=$(
    timeout $timeout \
    valkey-cli -h localhost -p $port PING)

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
