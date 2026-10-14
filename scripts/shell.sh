#!/usr/bin/env bash
# Opens a bash shell in the sandbox container (same mounts and options as claude.sh).
# Usage: scripts/shell.sh [--no-global-instructions] [command...]
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

WITH_GLOBAL_INSTRUCTIONS=1
if [[ "${1:-}" == "--no-global-instructions" ]]; then
  WITH_GLOBAL_INSTRUCTIONS=0
  shift
fi

require_podman
require_image
build_run_args
if [[ $# -eq 0 ]]; then
  set -- bash
fi
exec podman run "${PODMAN_RUN_ARGS[@]}" "${IMAGE}" "$@"
