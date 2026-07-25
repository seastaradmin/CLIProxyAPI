#!/usr/bin/env bash
# Start CLIProxyAPI (CPA) in the background.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

existing_pid="$(read_pid)"
if is_pid_running "${existing_pid}"; then
  log "already running (pid=${existing_pid})"
  print_endpoints
  exit 0
fi

# Stale pid file
if [[ -n "${existing_pid}" ]]; then
  rm -f "${CPA_PID_FILE}"
fi

ensure_config
ensure_binary

log "starting CPA from ${CPA_ROOT}"
log "config: ${CPA_CONFIG}"
log "binary: ${CPA_BIN}"

(
  cd "${CPA_ROOT}"
  nohup "${CPA_BIN}" --config "${CPA_CONFIG}" >>"${CPA_LOG_FILE}" 2>&1 &
  echo $! >"${CPA_PID_FILE}"
)

pid="$(read_pid)"
if ! is_pid_running "${pid}"; then
  err "process exited immediately (pid=${pid})"
  err "check log: ${CPA_LOG_FILE}"
  tail -n 40 "${CPA_LOG_FILE}" 2>/dev/null || true
  exit 1
fi

if wait_for_ready 40; then
  log "started successfully (pid=${pid})"
else
  log "process is running (pid=${pid}), but health check did not pass yet"
  log "it may still be booting; check log: ${CPA_LOG_FILE}"
fi

print_endpoints
maybe_open_browser
