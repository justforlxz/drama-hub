BIN     := bin/drama-hub
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

ENGINE  ?= podman
IMAGE   ?= localhost/drama-hub
TAG     ?= dev
PORT    ?= 8080
RUNARGS := --userns=keep-id --security-opt label=disable \
           -v "$(CURDIR)/data:/data:Z"

.PHONY: build run test lint check fmt fixtures clean \
        image e2e-image env-check test-e2e run-container shell

## ---- 本机（快速迭代，不作为验收依据） ----

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

## ---- 容器（验收依据：环境由镜像与 env.lock 固定） ----

image:
	$(ENGINE) build -f Containerfile --target runtime \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE):$(TAG) .

# 测试镜像 = runtime + Go 工具链，ffmpeg 与 runtime 同源。
e2e-image:
	$(ENGINE) build -f Containerfile --target e2e \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE)-e2e:$(TAG) .

env-check:
	./scripts/env-check.sh

# 集成测试：--network=none 把「测试不访问外网」变成环境强制，而不是靠自觉。
# 源码只读挂载，测试缓存放匿名卷，避免污染工作区。
test-e2e: e2e-image
	$(ENGINE) run --rm --pull=never --network=none \
		--userns=keep-id --security-opt label=disable \
		-v "$(CURDIR):/src:ro,Z" \
		--tmpfs /tmp:exec \
		$(IMAGE)-e2e:$(TAG) test -race -v -timeout 5m ./...

run-container: image
	$(ENGINE) run --rm --pull=never -p $(PORT):8080 $(RUNARGS) $(IMAGE):$(TAG)

shell: e2e-image
	$(ENGINE) run --rm -it --pull=never \
		--userns=keep-id --security-opt label=disable \
		-v "$(CURDIR):/src:Z" --tmpfs /tmp:exec \
		--entrypoint /bin/sh $(IMAGE)-e2e:$(TAG)

