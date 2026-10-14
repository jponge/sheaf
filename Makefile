VERSION ?= dev
LDFLAGS := -X github.com/jponge/sheaf/internal/cli.Version=$(VERSION)

.PHONY: build test vet

build:
	go build -ldflags "$(LDFLAGS)" -o bin/sheaf ./cmd/sheaf

test:
	go test -race ./...

vet:
	go vet ./...
