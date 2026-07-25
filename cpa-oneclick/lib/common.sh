#!/usr/bin/env bash
# Shared helpers for CPA one-click control scripts.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CPA_ONECLICK_DIR="${SCRIPT_DIR}"
DEFAULT_ROOT="$(cd "${CPA_ONECLICK_DIR}/.." && pwd)"

CPA_ROOT="${CPA_ROOT:-${DEFAULT_ROOT}}"
CPA_CONFIG="${CPA_CONFIG:-${CPA_ROOT}/config.yaml}"
CPA_EXAMPLE_CONFIG="${CPA_EXAMPLE_CONFIG:-${CPA_ROOT}/config.example.yaml}"
CPA_BIN="${CPA_BIN:-${CPA_ROOT}/cli-proxy-api}"
CPA_LOG_DIR="${CPA_LOG_DIR:-${CPA_ONECLICK_DIR}/logs}"
CPA_LOG_FILE="${CPA_LOG_FILE:-${CPA_LOG_DIR}/cpa.log}"
CPA_PID_FILE="${CPA_PID_FILE:-${CPA_LOG_DIR}/cpa.pid}"
CPA_OPEN_BROWSER="${CPA_OPEN_BROWSER:-0}"

mkdir -p "${CPA_LOG_DIR}"

log() {
  printf '[cpa-oneclick] %s\n' "$*"
}

err() {
  printf '[cpa-oneclick] ERROR: %s\n' "$*" >&2
}

resolve_port() {
  if [[ -n "${CPA_PORT:-}" ]]; then
    printf '%s\n' "${CPA_PORT}"
    return 0
  fi
  local port=""
  if [[ -f "${CPA_CONFIG}" ]]; then
    port="$(awk '
      /^[[:space:]]*port:[[:space:]]*[0-9]+/ {
        gsub(/[^0-9]/, "", $2)
        print $2
        exit
      }
    ' "${CPA_CONFIG}" 2>/dev/null || true)"
  fi
  if [[ -z "${port}" ]]; then
    port="8317"
  fi
  printf '%s\n' "${port}"
}

is_pid_running() {
  local pid="$1"
  [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null
}

read_pid() {
  if [[ -f "${CPA_PID_FILE}" ]]; then
    tr -d '[:space:]' <"${CPA_PID_FILE}" || true
  fi
}

ensure_config() {
  if [[ -f "${CPA_CONFIG}" ]]; then
    return 0
  fi
  if [[ ! -f "${CPA_EXAMPLE_CONFIG}" ]]; then
    err "config not found: ${CPA_CONFIG}"
    err "and example config missing: ${CPA_EXAMPLE_CONFIG}"
    return 1
  fi
  cp "${CPA_EXAMPLE_CONFIG}" "${CPA_CONFIG}"
  log "created config from example: ${CPA_CONFIG}"
  log "please edit api-keys / remote-management.secret-key before production use"
}

ensure_binary() {
  if [[ -x "${CPA_BIN}" ]]; then
    return 0
  fi
  if [[ -f "${CPA_BIN}" ]]; then
    chmod +x "${CPA_BIN}" || true
    if [[ -x "${CPA_BIN}" ]]; then
      return 0
    fi
  fi

  if ! command -v go >/dev/null 2>&1; then
    err "binary not found: ${CPA_BIN}"
    err "install Go, or build first: go build -o cli-proxy-api ./cmd/server"
    return 1
  fi

  log "binary missing, building: ${CPA_BIN}"
  (
    cd "${CPA_ROOT}"
    go build -o "${CPA_BIN}" ./cmd/server
  )
  chmod +x "${CPA_BIN}"
  log "build complete: ${CPA_BIN}"
}

health_check() {
  local port
  port="$(resolve_port)"
  if command -v curl >/dev/null 2>&1; then
    curl -fsS --max-time 2 "http://127.0.0.1:${port}/" >/dev/null 2>&1 \
      || curl -fsS --max-time 2 "http://127.0.0.1:${port}/v1/models" >/dev/null 2>&1 \
      || return 1
    return 0
  fi
  # Fallback: TCP connect check
  if command -v nc >/dev/null 2>&1; then
    nc -z 127.0.0.1 "${port}" >/dev/null 2>&1
    return $?
  fi
  return 0
}

wait_for_ready() {
  local retries="${1:-30}"
  local i
  for ((i = 1; i <= retries; i++)); do
    if health_check; then
      return 0
    fi
    sleep 0.3
  done
  return 1
}

maybe_open_browser() {
  local port
  port="$(resolve_port)"
  local url="http://127.0.0.1:${port}/management.html"
  if [[ "${CPA_OPEN_BROWSER}" != "1" ]]; then
    return 0
  fi
  if command -v open >/dev/null 2>&1; then
    open "${url}" >/dev/null 2>&1 || true
  elif command -v xdg-open >/dev/null 2>&1; then
    xdg-open "${url}" >/dev/null 2>&1 || true
  fi
}

print_endpoints() {
  local port
  port="$(resolve_port)"
  log "API:        http://127.0.0.1:${port}"
  log "Management: http://127.0.0.1:${port}/management.html"
  log "Log file:   ${CPA_LOG_FILE}"
  log "PID file:   ${CPA_PID_FILE}"
}
