#!/usr/bin/env bash
# Show CLIProxyAPI (CPA) status.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

port="$(resolve_port)"
pid="$(read_pid)"

if is_pid_running "${pid}"; then
  log "status: running"
  log "pid:    ${pid}"
else
  if [[ -n "${pid}" ]]; then
    log "status: stopped (stale pid=${pid})"
    rm -f "${CPA_PID_FILE}"
  else
    log "status: stopped"
  fi
  log "port:   ${port}"
  log "config: ${CPA_CONFIG}"
  log "binary: ${CPA_BIN}"
  exit 1
fi

log "port:   ${port}"
log "config: ${CPA_CONFIG}"
log "binary: ${CPA_BIN}"
log "log:    ${CPA_LOG_FILE}"

if health_check; then
  log "health: ok"
else
  log "health: not ready"
fi

print_endpoints
