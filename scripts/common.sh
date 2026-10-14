#!/usr/bin/env bash
# Shared helpers, sourced by the other scripts.

IMAGE="${SHEAF_IMAGE:-sheaf-claude}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
# Must match USER_UID/USER_GID in container/Containerfile.
CONTAINER_UID=1000
CONTAINER_GID=1000

die() { echo "error: $*" >&2; exit 1; }

require_podman() {
  command -v podman >/dev/null 2>&1 || die "podman not found in PATH"
  if [[ "$(uname -s)" == "Darwin" ]]; then
    local running
    running="$(podman machine list --format '{{.Running}}' 2>/dev/null | grep -c true || true)"
    [[ "${running}" -ge 1 ]] || die "no running podman machine; run 'podman machine start'"
  fi
}

require_image() {
  podman image exists "${IMAGE}" || die "image '${IMAGE}' not found; run scripts/build-image.sh"
}

# Fills the PODMAN_RUN_ARGS array with the options shared by claude.sh and shell.sh.
# Honours WITH_GLOBAL_INSTRUCTIONS=0 to skip mounting ~/.claude/CLAUDE.md.
build_run_args() {
  PODMAN_RUN_ARGS=(
    --rm -it
    --userns="keep-id:uid=${CONTAINER_UID},gid=${CONTAINER_GID}"
    --cap-drop=ALL
    --security-opt no-new-privileges
    -v "${PROJECT_ROOT}:/workspace"
    -w /workspace
    -e TERM
  )
  if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
    PODMAN_RUN_ARGS+=(-e ANTHROPIC_API_KEY)
  fi
  if [[ "${WITH_GLOBAL_INSTRUCTIONS:-1}" == "1" && -f "${HOME}/.claude/CLAUDE.md" ]]; then
    PODMAN_RUN_ARGS+=(-v "${HOME}/.claude/CLAUDE.md:/home/dev/.claude/CLAUDE.md:ro")
  fi
}
