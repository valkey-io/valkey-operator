#!/bin/bash
set -e

announce_file="/data/announce.conf"
mkdir -p /data
cat /dev/null > "$announce_file"

if [ "$VALKEY_ENABLE_EXTERNAL_ANNOUNCE" = "true" ]; then
	if [ -z "$VALKEY_NODE_PORT" ] || [ -z "$VALKEY_NODE_BUS_PORT" ]; then
		base_node_port="${VALKEY_BASE_NODE_PORT:-30000}"
		replica_count="${VALKEY_REPLICA_COUNT:-0}"
		if [ -n "$POD_NAME" ]; then
			trimmed="${POD_NAME%-0}"
			if [ "$trimmed" != "$POD_NAME" ]; then
				replica="${trimmed##*-}"
				tmp="${trimmed%-*}"
				shard="${tmp##*-}"
				if echo "$replica" | grep -qE '^[0-9]+$' && echo "$shard" | grep -qE '^[0-9]+$'; then
					ordinal="$((shard * (replica_count + 1) + replica))"
					VALKEY_NODE_PORT="$((base_node_port + ordinal * 2))"
					VALKEY_NODE_BUS_PORT="$((base_node_port + ordinal * 2 + 1))"
				fi
			fi
		fi
	fi

	if [ -n "$VALKEY_NODE_IP" ] && [ -n "$VALKEY_NODE_PORT" ] && [ -n "$VALKEY_NODE_BUS_PORT" ]; then
		cat <<EOF > "$announce_file"
cluster-announce-ip $VALKEY_NODE_IP
cluster-announce-port $VALKEY_NODE_PORT
cluster-announce-bus-port $VALKEY_NODE_BUS_PORT
EOF
	fi
fi

exec valkey-server /config/valkey.conf
