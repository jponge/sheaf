#!/usr/bin/env bash
# Builds the sandbox image. Extra arguments are passed to 'podman build' (e.g. --no-cache).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

require_podman
exec podman build \
  --build-arg "USER_UID=${CONTAINER_UID}" \
  --build-arg "USER_GID=${CONTAINER_GID}" \
  -t "${IMAGE}" \
  -f "${PROJECT_ROOT}/container/Containerfile" \
  "$@" \
  "${PROJECT_ROOT}/container"
