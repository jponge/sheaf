# The image `make demo` records the demo in: the official vhs image (vhs,
# ttyd, ffmpeg and Chromium, which vhs renders through), pinned by digest,
# plus git, which the demo bundle and sheaf need.
FROM ghcr.io/charmbracelet/vhs:v0.12.1@sha256:ea49a6a1c529be83153e88321892b5585964418f0b9055e8c1e0d732194234a3
RUN apt-get update \
 && apt-get install -y --no-install-recommends git \
 && rm -rf /var/lib/apt/lists/*
