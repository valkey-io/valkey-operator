#!/bin/sh
#
# Runs in the init container before valkey-server starts (POSIX sh).
# Intentionally a no-op for now: it ships in the scripts ConfigMap so later
# behaviour arrives as a script change, which does not roll pods.
set -e

exit 0
