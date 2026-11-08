VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test lint cross clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/kb ./cmd/kb

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/kb

test:
	go vet ./...
	go test ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

cross:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/kb-darwin-arm64 ./cmd/kb
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/kb-darwin-amd64 ./cmd/kb
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/kb-linux-amd64 ./cmd/kb
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/kb-linux-arm64 ./cmd/kb

clean:
	go clean
	rm -f bin/kb bin/kb-*
