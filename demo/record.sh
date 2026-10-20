#!/usr/bin/env bash
# Records demo/sheaf.gif; run by `make demo` inside its container (see
# demo/Dockerfile), with the repository mounted at the working directory and
# sheaf built for the container in bin/linux/. The demo bundle lives in the
# container's /tmp and goes away with it.
set -euo pipefail

dir=/tmp/sheaf-demo
export PATH="$PWD/bin/linux:$PATH"
demo/setup.sh "$dir" >/dev/null
SHEAF_DEMO_DIR=$dir exec vhs demo/sheaf.tape
