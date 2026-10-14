VERSION ?= dev
LDFLAGS := -X github.com/jponge/sheaf/internal/cli.Version=$(VERSION)

.PHONY: build test test-symlinked-tmp vet

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

vet:
	go vet ./...
