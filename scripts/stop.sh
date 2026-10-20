#!/bin/sh
# Stop the running server, if any.
set -eu
. "$(dirname "$0")/common.sh"

pids="$(server_pids)"
if [ -z "$pids" ]; then
	echo "not running"
	exit 0
fi
echo "stopping server on port $port (pid $pids)"
kill $pids
for _ in 1 2 3 4 5 6 7 8 9 10; do
	listening || exit 0
	sleep 0.5
done
echo "server did not stop within 5s" >&2
exit 1
