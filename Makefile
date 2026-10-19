VERSION ?= dev
LDFLAGS := -X github.com/jponge/sheaf/internal/cli.Version=$(VERSION)

.PHONY: build install test test-shells test-symlinked-tmp vet vet-windows demo demo-check

# staticcheck runs at a pinned version through `go run` (downloaded into the
# module cache on first use): it is a tool, not a dependency of the module.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2

build:
	go build -ldflags "$(LDFLAGS)" -o bin/sheaf ./cmd/sheaf

# Installs sheaf into $GOBIN (default: $(go env GOPATH)/bin).
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/sheaf

test:
	go test -race ./...

# Runs the suite with TMPDIR reached through a symbolic link, like macOS's
# /var -> /private/var: tests must expect the resolved paths that git and
# sheaf report. The temporary directory is removed afterwards.
test-symlinked-tmp:
	@base=$$(mktemp -d) && trap 'chmod -R u+w "$$base" 2>/dev/null; rm -rf "$$base"' EXIT && \
	mkdir "$$base/real" && ln -s "$$base/real" "$$base/link" && \
	echo "TMPDIR=$$base/link" && \
	TMPDIR="$$base/link" go test -race -count=1 ./...

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
	SHEAF_TEST_REQUIRE_SHELLS=1 go test -race -count=1 ./internal/cli/

vet:
	go vet ./...
	go run $(STATICCHECK) ./...

# Vets the Windows build: staticcheck is built for the host and analyses the
# code with GOOS=windows.
vet-windows:
	GOOS=windows go vet ./...
	go run -exec 'env GOOS=windows' $(STATICCHECK) ./...

# Records demo/sheaf.gif with vhs (needs vhs, ttyd and ffmpeg on PATH; see
# the README). demo/setup.sh creates the demo bundle in DEMO_DIR (replaced if
# an earlier run left it; a symbolic link or another existing directory is
# refused and kept), with sheaf built into a temporary directory first on
# PATH; both are removed afterwards, also on interruption (the bundle is
# kept when setup.sh fails). Nothing uses the network, gh or the user's
# configuration. The INT and TERM traps exit so that the EXIT trap runs:
# dash does not run it on a signal.
DEMO_DIR ?= /tmp/sheaf-demo
demo:
	@for t in vhs ttyd ffmpeg; do \
	  command -v $$t >/dev/null || { echo "make demo: $$t not found on PATH (see Regenerating the demo in README.md)" >&2; exit 1; }; \
	done
	@set -e; bin=$$(mktemp -d); made=; \
	trap 'rm -rf "$$bin"; if [ -n "$$made" ]; then rm -rf "$(DEMO_DIR)"; fi' EXIT; \
	trap 'exit 130' INT; trap 'exit 143' TERM; \
	go build -ldflags "$(LDFLAGS)" -o "$$bin/sheaf" ./cmd/sheaf; \
	PATH="$$bin:$$PATH" demo/setup.sh "$(DEMO_DIR)"; made=1; \
	PATH="$$bin:$$PATH" SHEAF_DEMO_DIR="$(DEMO_DIR)" vhs demo/sheaf.tape

# Checks the demo without vhs: demo/check.sh runs the steps of the tape in
# plain mode against a fresh demo bundle (temporary, offline), and vhs's
# parser validates the tape (vhs at a pinned version through `go run`, like
# staticcheck; ttyd and ffmpeg are not needed).
VHS := github.com/charmbracelet/vhs@v0.12.1
demo-check:
	@set -e; bin=$$(mktemp -d); \
	trap 'rm -rf "$$bin"' EXIT; trap 'exit 130' INT; trap 'exit 143' TERM; \
	go build -ldflags "$(LDFLAGS)" -o "$$bin/sheaf" ./cmd/sheaf; \
	PATH="$$bin:$$PATH" demo/check.sh; \
	go run $(VHS) validate demo/sheaf.tape
