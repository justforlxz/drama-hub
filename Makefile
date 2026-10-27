BIN     := bin/drama-hub
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

ENGINE  ?= podman
IMAGE   ?= localhost/drama-hub
TAG     ?= dev
PORT    ?= 8080
# 运行期挂载：数据卷归属当前用户，避开 SELinux 与 root 归属问题。
RUNARGS := --userns=keep-id --security-opt label=disable -v "$(CURDIR)/data:/data:Z"

.PHONY: build run test lint check fmt fixtures clean image e2e-image env-check shell

## ---- 本机（只做快速反馈，不产出验收物） ----

# 只是编译检查；运行与测试一律走容器，因为开发机 ffmpeg 与容器 ffmpeg 不是同一个版本。
build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/drama-hub

lint:
	go vet ./...
	@command -v staticcheck >/dev/null 2>&1 \
		&& staticcheck ./... \
		|| echo "staticcheck 未安装，已跳过（go install honnef.co/go/tools/cmd/staticcheck@latest）"

fmt:
	gofmt -l -w .

fixtures:
	./scripts/make-fixtures.sh

clean:
	rm -rf bin

## ---- 容器（唯一的运行与测试路径） ----

image:
	$(ENGINE) build -f Containerfile --target runtime \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE):$(TAG) .

e2e-image:
	$(ENGINE) build -f Containerfile --target e2e \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE)-e2e:$(TAG) .

env-check:
	./scripts/env-check.sh

# 测试只有这一条路径。--network=none 把「测试不访问外网」变成环境强制，而不是靠自觉。
# 源码只读挂载，测试临时文件写 /tmp（tmpfs，需 exec）。
test: e2e-image
	$(ENGINE) run --rm --pull=never --network=none \
		--userns=keep-id --security-opt label=disable \
		-v "$(CURDIR):/src:ro,Z" \
		--tmpfs /tmp:exec \
		$(IMAGE)-e2e:$(TAG) -test.timeout 5m

check: lint test

run: image
	mkdir -p data
	$(ENGINE) run --rm --pull=never -p $(PORT):8080 $(RUNARGS) $(IMAGE):$(TAG)

shell: e2e-image
	$(ENGINE) run --rm -it --pull=never \
		--userns=keep-id --security-opt label=disable \
		-v "$(CURDIR):/src:Z" --tmpfs /tmp:exec \
		--entrypoint /bin/sh $(IMAGE)-e2e:$(TAG)
