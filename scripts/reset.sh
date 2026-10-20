#!/bin/sh
# Reset the app to a fresh state: stop the running server, back up the database and
# delete it. config.json is kept. The next start recreates an empty database.
set -eu

dir="${ASSISTANT_HOME:-$HOME/.assistant}"
config="$dir/config.json"

json_value() {
	[ -f "$config" ] || return 0
	sed -n "s/^[[:space:]]*\"$1\":[[:space:]]*\"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}[[:space:]]*$/\1/p" "$config"
}

db="$(json_value db)"
db="${db:-$dir/assistant.db}"
port="$(json_value port)"
port="${port:-6767}"

# Only stop the listener if it is this app, never some other server on the port.
pids=""
for pid in $(lsof -ti "tcp:$port" -sTCP:LISTEN 2>/dev/null || true); do
	[ "$(basename "$(ps -p "$pid" -o comm=)")" = assistant ] && pids="$pids $pid"
done
pids="${pids# }"
if [ -n "$pids" ]; then
	echo "stopping server on port $port (pid $pids)"
	kill $pids
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		lsof -ti "tcp:$port" -sTCP:LISTEN >/dev/null 2>&1 || break
		sleep 0.5
	done
fi

if [ -f "$db" ]; then
	backup="$dir/backups/$(date +%Y%m%d-%H%M%S)"
	mkdir -p "$backup"
	for f in "$db" "$db-wal" "$db-shm"; do
		[ -f "$f" ] && mv "$f" "$backup/"
	done
	echo "database moved to $backup"
else
	echo "no database at $db"
fi

echo "reset done — start with ./assistant"
