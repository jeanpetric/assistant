#!/bin/sh
# Reset the app to a fresh state: stop the running server, back up the database and
# delete it. config.json is kept. The next start recreates an empty database.
# This wipes every task, so it asks first; CONFIRM=reset skips the question.
set -eu
. "$(dirname "$0")/common.sh"

if [ "${CONFIRM:-}" != reset ]; then
	echo "This deletes ALL tasks, sessions and schedules in $db (a backup is kept)."
	echo "To just restart the app, use: make restart"
	printf "Type 'reset' to continue: "
	read -r answer || answer=""
	if [ "$answer" != reset ]; then
		echo "aborted, nothing changed" >&2
		exit 1
	fi
fi

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
