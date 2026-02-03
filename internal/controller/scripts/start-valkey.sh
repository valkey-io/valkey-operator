#!/bin/bash
set -e

announce_file="/data/announce.conf"
mkdir -p /data
cat /dev/null > "$announce_file"

base_node_port="${VALKEY_BASE_NODE_PORT:-30000}"
if [ -n "$POD_NAME" ] && echo "$POD_NAME" | grep -q -- '-'; then
	ordinal="${POD_NAME##*-}"
	if echo "$ordinal" | grep -qE '^[0-9]+$'; then
		VALKEY_NODE_PORT="$((base_node_port + ordinal * 2))"
		VALKEY_NODE_BUS_PORT="$((base_node_port + ordinal * 2 + 1))"
	fi
fi

if [ -n "$VALKEY_NODE_IP" ] && [ -n "$VALKEY_NODE_PORT" ] && [ -n "$VALKEY_NODE_BUS_PORT" ]; then
	cat <<EOF > "$announce_file"
cluster-announce-ip $VALKEY_NODE_IP
cluster-announce-port $VALKEY_NODE_PORT
cluster-announce-bus-port $VALKEY_NODE_BUS_PORT
EOF
fi

exec valkey-server /config/valkey.conf
