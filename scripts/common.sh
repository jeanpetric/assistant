# Shared by the scripts in this directory: paths, config values and the running server.
# ASSISTANT_HOME overrides ~/.assistant (handy for trying the scripts out).

root="$(cd "$(dirname "$0")/.." && pwd)"
dir="${ASSISTANT_HOME:-$HOME/.assistant}"
config="$dir/config.json"
log="$dir/assistant.log"

json_value() {
	[ -f "$config" ] || return 0
	sed -n "s/^[[:space:]]*\"$1\":[[:space:]]*\"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}[[:space:]]*$/\1/p" "$config"
}

db="$(json_value db)"
db="${db:-$dir/assistant.db}"
port="$(json_value port)"
port="${port:-6767}"

# PIDs of this app listening on the port — never some other server that happens to use it.
server_pids() {
	pids=""
	for pid in $(lsof -ti "tcp:$port" -sTCP:LISTEN 2>/dev/null || true); do
		[ "$(basename "$(ps -p "$pid" -o comm=)")" = assistant ] && pids="$pids $pid"
	done
	echo "${pids# }"
}

listening() {
	lsof -ti "tcp:$port" -sTCP:LISTEN >/dev/null 2>&1
}
