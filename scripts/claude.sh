#!/usr/bin/env bash
# Runs Claude Code with --dangerously-skip-permissions inside the sandbox container.
# Usage: scripts/claude.sh [--no-global-instructions] [claude args...]
# Requires ANTHROPIC_API_KEY in the host environment.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

WITH_GLOBAL_INSTRUCTIONS=1
if [[ "${1:-}" == "--no-global-instructions" ]]; then
  WITH_GLOBAL_INSTRUCTIONS=0
  shift
fi

[[ -n "${ANTHROPIC_API_KEY:-}" ]] || die "ANTHROPIC_API_KEY is not set in the host environment"
require_podman
require_image
build_run_args
exec podman run "${PODMAN_RUN_ARGS[@]}" "${IMAGE}" claude --dangerously-skip-permissions "$@"
