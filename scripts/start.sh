#!/bin/sh
# Start the server in the background; output goes to the log file.
set -eu
. "$(dirname "$0")/common.sh"

if [ -n "$(server_pids)" ]; then
	echo "already running on http://127.0.0.1:$port"
	exit 0
fi
if listening; then
	echo "port $port is used by another program" >&2
	exit 1
fi

mkdir -p "$dir"
nohup "$root/assistant" -config "$config" >>"$log" 2>&1 &
for _ in 1 2 3 4 5 6 7 8 9 10; do
	if listening; then
		echo "running on http://127.0.0.1:$port (log: $log)"
		exit 0
	fi
	sleep 0.5
done
echo "server did not start, last lines of $log:" >&2
tail -n 20 "$log" >&2
exit 1
