VERSION ?= dev
LDFLAGS := -X github.com/jponge/sheaf/internal/cli.Version=$(VERSION)

.PHONY: build test test-shells test-symlinked-tmp vet

build:
	go build -ldflags "$(LDFLAGS)" -o bin/sheaf ./cmd/sheaf

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
