BIN     := bin/drama-hub
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build run test lint check fmt fixtures clean

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/drama-hub

run:
	DRAMA_HUB_ADDR=$${DRAMA_HUB_ADDR:-:8080} go run ./cmd/drama-hub

test:
	go test -race ./...

lint:
	go vet ./...
	@command -v staticcheck >/dev/null 2>&1 \
		&& staticcheck ./... \
		|| echo "staticcheck 未安装，已跳过（go install honnef.co/go/tools/cmd/staticcheck@latest）"

check: lint test

fmt:
	gofmt -l -w .

fixtures:
	./scripts/make-fixtures.sh

clean:
	rm -rf bin
