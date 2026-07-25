#!/usr/bin/env bash
# Restart CLIProxyAPI (CPA).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

"${SCRIPT_DIR}/stop.sh"
sleep 0.5
"${SCRIPT_DIR}/start.sh"
