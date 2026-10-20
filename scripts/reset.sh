#!/bin/sh
# Reset the app to a fresh state: stop the running server, back up the database and
# delete it. config.json is kept. The next start recreates an empty database.
set -eu
. "$(dirname "$0")/common.sh"

"$(dirname "$0")/stop.sh"

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
