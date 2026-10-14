#!/usr/bin/env bash
# Runs Claude Code with --dangerously-skip-permissions inside the sandbox container.
# Usage: scripts/claude.sh [--no-global-instructions] [claude args...]
# Authentication: run /login once inside the container (subscription login); it is stored in the
# '${SHEAF_STATE_VOLUME:-sheaf-claude-home}' podman volume and reused by later runs.
# Remove it with 'podman volume rm sheaf-claude-home' to log out.
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
if ! podman volume exists "${STATE_VOLUME}"; then
  echo "First run: no login stored yet. Run /login in Claude Code and follow the URL (state is kept in volume '${STATE_VOLUME}')." >&2
fi
exec podman run "${PODMAN_RUN_ARGS[@]}" "${IMAGE}" claude --dangerously-skip-permissions "$@"
