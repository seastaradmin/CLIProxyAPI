#!/usr/bin/env bash
# Stop the background CLIProxyAPI (CPA) process.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

pid="$(read_pid)"
if [[ -z "${pid}" ]]; then
  log "not running (no pid file)"
  exit 0
fi

if ! is_pid_running "${pid}"; then
  log "not running (stale pid=${pid})"
  rm -f "${CPA_PID_FILE}"
  exit 0
fi

log "stopping pid=${pid}"
kill "${pid}" 2>/dev/null || true

for _ in $(seq 1 30); do
  if ! is_pid_running "${pid}"; then
    rm -f "${CPA_PID_FILE}"
    log "stopped"
    exit 0
  fi
  sleep 0.2
done

log "process still alive, sending SIGKILL"
kill -9 "${pid}" 2>/dev/null || true
rm -f "${CPA_PID_FILE}"
log "force stopped"
