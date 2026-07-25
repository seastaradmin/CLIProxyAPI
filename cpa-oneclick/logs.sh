#!/usr/bin/env bash
# Tail CPA logs.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

if [[ ! -f "${CPA_LOG_FILE}" ]]; then
  log "log file not found yet: ${CPA_LOG_FILE}"
  exit 1
fi

exec tail -n "${1:-100}" -f "${CPA_LOG_FILE}"
