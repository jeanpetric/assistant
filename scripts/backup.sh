#!/bin/sh
# Back up the database to one self-contained file, safe while the app is running:
# SQLite's online backup copies a consistent snapshot including what is still in the
# WAL (a plain cp of assistant.db could miss it or catch it half-written). The copy is
# checked with PRAGMA integrity_check before it gets its final name, so every
# backups/assistant-*.db is a complete database. Prints the backup's path.
# Restore: make stop, cp the file over the db path, delete the db's -wal and -shm, make start.
set -eu
. "$(dirname "$0")/common.sh"

command -v sqlite3 >/dev/null || { echo "sqlite3 not found" >&2; exit 1; }
[ -f "$db" ] || { echo "no database at $db" >&2; exit 1; }

mkdir -p "$dir/backups"
out="$dir/backups/assistant-$(date +%Y%m%d-%H%M%S).db"
[ -e "$out" ] && { echo "$out already exists" >&2; exit 1; }
# Built in a scratch dir so SQLite's side files (-shm, -journal) never land next to backups.
work="$(mktemp -d "$dir/backups/.partial.XXXXXX")"
trap 'rm -rf "$work"' EXIT
tmp="$work/assistant.db"

# .timeout waits out the app's write lock instead of failing on SQLITE_BUSY.
sqlite3 "$db" ".timeout 10000" ".backup '$tmp'"
# The copy keeps the source's WAL mode; switch it to a rollback journal so the backup
# stays a single file when opened (the app turns WAL back on when it runs from it).
sqlite3 "$tmp" "PRAGMA journal_mode=DELETE;" >/dev/null

check="$(sqlite3 "$tmp" "PRAGMA integrity_check;")"
if [ "$check" != ok ]; then
	echo "integrity check failed, no backup written:" >&2
	echo "$check" >&2
	exit 1
fi

mv "$tmp" "$out"
echo "$out"
