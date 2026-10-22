VERSION ?= dev
LDFLAGS := -X github.com/jponge/sheaf/internal/cli.Version=$(VERSION)

.PHONY: build install clean test test-shells test-symlinked-tmp vet vet-windows demo demo-check

# staticcheck runs at a pinned version through `go run` (downloaded into the
# module cache on first use): it is a tool, not a dependency of the module.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2

# go test's per-package timeout (Go's default is 10m): the suite runs real git
# and pty sessions, and slow machines (e.g. CI's macOS runners) exceed 10m.
TEST_TIMEOUT ?= 30m

build:
	go build -ldflags "$(LDFLAGS)" -o bin/sheaf ./cmd/sheaf

# Installs sheaf into $GOBIN (default: $(go env GOPATH)/bin).
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/sheaf

# Removes the build outputs: bin/ (sheaf, and the demo's Linux build and
# video).
clean:
	rm -rf bin

test:
	go test -race -timeout $(TEST_TIMEOUT) ./...

# Runs the suite with TMPDIR reached through a symbolic link, like macOS's
# /var -> /private/var: tests must expect the resolved paths that git and
# sheaf report. The temporary directory is removed afterwards.
test-symlinked-tmp:
	@base=$$(mktemp -d) && trap 'chmod -R u+w "$$base" 2>/dev/null; rm -rf "$$base"' EXIT && \
	mkdir "$$base/real" && ln -s "$$base/real" "$$base/link" && \
	echo "TMPDIR=$$base/link" && \
	TMPDIR="$$base/link" go test -race -count=1 -timeout $(TEST_TIMEOUT) ./...

# Runs the cli tests with fish and zsh required: `make test` skips the fish and
# zsh tests when the shells are missing. The shells are the ones on PATH, or
# the ones unpacked (no root needed, e.g. `dpkg-deb -x fish_*.deb DIR`, same
# for zsh) under SHELLS_ROOT=DIR, giving DIR/usr/bin/fish and DIR/usr/bin/zsh:
# wrappers then set their library path, the zsh function and module paths,
# and run a copy of fish next to links to its data (fish finds its data
# relative to its binary). See shellBinary in internal/cli/cmd_shell_test.go.
SHELLS_ROOT ?=
test-shells:
	@set -e; tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT; \
	if [ -n "$(SHELLS_ROOT)" ]; then \
	  r=$$(cd "$(SHELLS_ROOT)" && pwd); \
	  lib=$$(ls -d "$$r"/usr/lib/*-linux-gnu 2>/dev/null | head -n 1); \
	  mod=$$(ls -d "$$lib"/zsh/*/ 2>/dev/null | head -n 1); \
	  fpath=$$(find "$$r/usr/share/zsh/functions" -type d | tr '\n' ':'); \
	  mkdir -p "$$tmp/bin" "$$tmp/fish/bin"; \
	  cp "$$r/usr/bin/fish" "$$tmp/fish/bin/fish"; \
	  ln -s "$$r/usr/share" "$$tmp/fish/share"; ln -s "$$r/etc" "$$tmp/fish/etc"; \
	  printf '#!/bin/sh\nLD_LIBRARY_PATH="%s" exec "%s" "$$@"\n' "$$lib" "$$tmp/fish/bin/fish" > "$$tmp/bin/fish"; \
	  printf '#!/bin/sh\nLD_LIBRARY_PATH="%s" FPATH="%s" SHEAF_TEST_ZSH_MODULE_PATH="%s" exec "%s" "$$@"\n' \
	    "$$lib" "$${fpath%:}" "$${mod%/}" "$$r/usr/bin/zsh" > "$$tmp/bin/zsh"; \
	  chmod +x "$$tmp/bin/fish" "$$tmp/bin/zsh"; \
	  export SHEAF_TEST_FISH="$$tmp/bin/fish" SHEAF_TEST_ZSH="$$tmp/bin/zsh"; \
	  "$$SHEAF_TEST_FISH" --no-config -c 'echo "fish $$version, data: $$__fish_data_dir"'; \
	  "$$SHEAF_TEST_ZSH" -fc 'echo "zsh $$ZSH_VERSION"'; \
	fi; \
	SHEAF_TEST_REQUIRE_SHELLS=1 go test -race -count=1 -timeout $(TEST_TIMEOUT) ./internal/cli/

vet:
	go vet ./...
	go run $(STATICCHECK) ./...

# Vets the Windows build: staticcheck is built for the host and analyses the
# code with GOOS=windows.
vet-windows:
	GOOS=windows go vet ./...
	go run -exec 'env GOOS=windows' $(STATICCHECK) ./...

# Records demo/sheaf.gif with vhs in a container (needs only docker; see the
# README): vhs renders through Chromium, which stays inside the image built
# from demo/Dockerfile. sheaf is built for the container's architecture into
# bin/linux/, then demo/record.sh creates the demo bundle in the container's
# /tmp (demo/setup.sh), runs vhs (a video) and makes the GIF in two ffmpeg
# passes (vhs's own GIF output keeps every frame in memory). Nothing uses the network (beyond pulling
# the image), gh or the user's configuration. The container runs with the
# user's ids so that the GIF is theirs.
DEMO_IMAGE := sheaf-demo-vhs
demo:
	@command -v docker >/dev/null || { echo "make demo: docker not found on PATH (see Regenerating the demo in README.md)" >&2; exit 1; }
	@set -e; \
	arch=$$(docker version --format '{{.Server.Arch}}'); \
	docker build -q -t $(DEMO_IMAGE) - < demo/Dockerfile >/dev/null; \
	CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o bin/linux/sheaf ./cmd/sheaf; \
	docker run --rm --user "$$(id -u):$$(id -g)" -e HOME=/tmp \
	  -v "$(CURDIR):/vhs" -w /vhs --entrypoint bash $(DEMO_IMAGE) demo/record.sh

# Checks the demo without vhs: demo/check.sh runs the steps of the tape in
# plain mode against a fresh demo bundle (temporary, offline), and vhs's
# parser validates the tape (vhs at a pinned version through `go run`, like
# staticcheck; docker is not needed).
VHS := github.com/charmbracelet/vhs@v0.12.1
demo-check:
	@set -e; bin=$$(mktemp -d); \
	trap 'rm -rf "$$bin"' EXIT; trap 'exit 130' INT; trap 'exit 143' TERM; \
	go build -ldflags "$(LDFLAGS)" -o "$$bin/sheaf" ./cmd/sheaf; \
	PATH="$$bin:$$PATH" demo/check.sh; \
	go run $(VHS) validate demo/sheaf.tape
