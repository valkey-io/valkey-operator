#!/bin/bash
#
# Liveness check of a Valkey node (Bash only).
#
# Usage: liveness-check.sh [timeout] [port]
#        Default timeout is 4s, and default Valkey port is 6379.
set -e

timeout=${1:-"4"}
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

# Perform check
response=$(
    timeout $timeout \
    valkey-cli -h localhost -p $port PING)

responseFirstWord=$(echo "$response" | head -n1 | awk '{print $1;}')
if [ "$response" != "PONG" ] && [ "$responseFirstWord" != "LOADING" ] && [ "$responseFirstWord" != "MASTERDOWN" ]; then
    echo "$response" >&2
    exit 1
fi
