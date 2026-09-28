#!/usr/bin/env bash
# Manage the WhatsApp bridge that the WhatsApp MCP server talks to (127.0.0.1:8080).
#
#   bridge.sh start    start in the background and wait until it is connected to
#                      WhatsApp (up to 70 s); no-op if already running
#   bridge.sh stop     stop it
#   bridge.sh restart  stop, then start
#   bridge.sh status   show whether the process is running (else the last log lines)
#   bridge.sh health   ask the running bridge for /api/health (connected, last event, ...)
#   bridge.sh logs     follow the log
#   bridge.sh fg       run in the foreground (use this if you need to scan a QR code again)
#
# Works through a symlink (e.g. ~/.local/bin/wa-bridge -> scripts/bridge.sh).
# The bridge is rebuilt when a Go source file, go.mod or go.sum is newer than
# the binary. The log is whatsapp-bridge/bridge.log, rotated to bridge.log.1
# on start once it is over 10 MB.

set -u

# The store holds the WhatsApp session keys and message history, and the log
# may too (with -debug): keep everything this script creates owner-only.
umask 077

# Resolve symlinks to find the repository.
src="${BASH_SOURCE[0]}"
while [[ -L "$src" ]]; do
  dir="$(cd -P "$(dirname "$src")" && pwd)"
  src="$(readlink "$src")"
  [[ "$src" != /* ]] && src="$dir/$src"
done
REPO_DIR="$(cd -P "$(dirname "$src")/.." && pwd)"

BRIDGE_DIR="$REPO_DIR/whatsapp-bridge"
BIN="$BRIDGE_DIR/whatsapp-bridge"
LOG="$BRIDGE_DIR/bridge.log"
TOKEN_FILE="$BRIDGE_DIR/store/bridge_token"
PORT=8080
LOG_MAX_BYTES=$((10 * 1024 * 1024))

pid() { pgrep -f "$BIN" | head -1; }

build_if_needed() {
  if [[ -x "$BIN" ]] && [[ -z "$(find "$BRIDGE_DIR" -maxdepth 1 \( -name '*.go' -o -name go.mod -o -name go.sum \) -newer "$BIN" -print -quit)" ]]; then
    return 0
  fi
  local version
  version="$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)"
  echo "Building bridge ($version)..."
  (cd "$BRIDGE_DIR" && go build -ldflags "-X main.version=$version" -o whatsapp-bridge .) || exit 1
}

rotate_log() {
  if [[ -f "$LOG" ]] && (( $(wc -c <"$LOG") > LOG_MAX_BYTES )); then
    mv -f "$LOG" "$LOG.1"
    echo "Rotated $LOG to $LOG.1"
  fi
}

# Seconds start waits for the bridge to connect to WhatsApp. The bridge
# itself gives up (exit 1) after 60 s without a connection.
START_TIMEOUT=70

# new_log_lines prints what the log gained since it was $1 bytes long.
new_log_lines() {
  [[ -f "$LOG" ]] && tail -c +"$(( $1 + 1 ))" "$LOG"
}

start() {
  if [[ -n "$(pid)" ]]; then
    echo "Bridge already running (pid $(pid))."
    return 0
  fi
  build_if_needed
  rotate_log
  local log_start=0
  [[ -f "$LOG" ]] && log_start=$(wc -c <"$LOG" | tr -d ' ')
  # Run from the bridge dir so it finds its login session in store/
  (cd "$BRIDGE_DIR" && nohup "$BIN" >>"$LOG" 2>&1 &)
  printf "Starting bridge (waiting up to %ss for WhatsApp)" "$START_TIMEOUT"
  local deadline=$(( SECONDS + START_TIMEOUT ))
  while (( SECONDS < deadline )); do
    sleep 1
    if [[ -z "$(pid)" ]]; then
      echo " ... exited. Last log lines:"
      new_log_lines "$log_start" | tail -n 15
      return 1
    fi
    if health >/dev/null 2>&1; then
      echo " ... connected (pid $(pid)). Log: $LOG"
      return 0
    fi
    if new_log_lines "$log_start" | grep -q "Scan this QR code"; then
      echo " ... it needs pairing. Run: $0 stop && $0 fg"
      return 1
    fi
    printf "."
  done
  echo " ... not connected after ${START_TIMEOUT}s (pid $(pid)). Last log lines:"
  new_log_lines "$log_start" | tail -n 15
  return 1
}

status() {
  if [[ -n "$(pid)" ]]; then
    echo "Running (pid $(pid))."
    return 0
  fi
  echo "Not running."
  if [[ -s "$LOG" ]]; then
    echo "Last log lines ($LOG):"
    tail -n 10 "$LOG"
  fi
  return 1
}

stop() {
  local p
  p="$(pid)"
  if [[ -z "$p" ]]; then
    echo "Bridge not running."
    return 0
  fi
  kill "$p"
  for _ in {1..10}; do
    if [[ -z "$(pid)" ]]; then
      echo "Bridge stopped."
      return 0
    fi
    sleep 0.5
  done
  kill -9 "$p" 2>/dev/null
  echo "Bridge killed."
}

# health prints the bridge's /api/health JSON. Exit status: 0 connected and
# logged in, 2 running but not connected or not logged in, 1 not reachable.
health() {
  if [[ ! -r "$TOKEN_FILE" ]]; then
    echo "No API token at $TOKEN_FILE. Has the bridge been started?"
    return 1
  fi
  local body
  # The header is read from a file descriptor so the token isn't in ps output.
  if ! body="$(curl -fsS --max-time 2 -H @<(printf 'X-Bridge-Token: %s\n' "$(<"$TOKEN_FILE")") "http://127.0.0.1:$PORT/api/health")"; then
    echo "Bridge not reachable on 127.0.0.1:$PORT ($(if [[ -n "$(pid)" ]]; then echo "process $(pid) running"; else echo "not running"; fi))."
    return 1
  fi
  if command -v python3 >/dev/null 2>&1; then
    printf '%s\n' "$body" | python3 -m json.tool
  else
    printf '%s\n' "$body"
  fi
  local compact="${body//[[:space:]]/}"
  [[ "$compact" == *'"connected":true'* && "$compact" == *'"logged_in":true'* ]] || return 2
}

case "${1:-start}" in
  start)   start ;;
  stop)    stop ;;
  restart) stop; start ;;
  status)  status ;;
  health)  health ;;
  logs)    tail -f "$LOG" ;;
  fg)      stop >/dev/null; build_if_needed; cd "$BRIDGE_DIR" && exec "$BIN" ;;
  *)       echo "Usage: $0 {start|stop|restart|status|health|logs|fg}"; exit 1 ;;
esac
