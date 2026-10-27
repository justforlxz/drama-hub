# syntax=docker/dockerfile:1
#
# 三段式：build / runtime / e2e。
# ffmpeg 只出现在 runtime 段，e2e 段 FROM runtime —— 测试与线上用同一个 ffmpeg
# 二进制，一致性由镜像结构保证，不靠纪律。
#
# 测试二进制在 build 段（自带 gcc）编译好再拷进 e2e，所以 e2e 里不放 Go 工具链：
# 镜像小、测试不用每次重新编译。ffmpeg 是运行期 exec 调用的，测试二进制里不含它，
# 因此「编译期工具链」与「运行期 ffmpeg」无关。
#
# 基础镜像按 digest 固定，见 env.lock。只固定 tag 挡不住上游重推。

ARG BASE=alpine@sha256:ee16d92f96db08d9d2d07da48555a27f3a69d8f0759021dbc601195ed0d2b5bd
ARG GOIMG=golang@sha256:bff16a94c0b4b6d9642581e2a3aa81beb3edb940b2f5a3aac1d22c6a68cb7747

# ---------------------------------------------------------------- build
FROM ${GOIMG} AS build

# -race 依赖 cgo，构建期需要 gcc/musl-dev；它们留在 build 段，不进任何运行镜像。
RUN apk add --no-cache gcc musl-dev

WORKDIR /src

# 依赖层单独缓存：go.mod/go.sum 不变时无需重下模块。
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none

ENV GOFLAGS=-trimpath

# 宿主机产物：单二进制，纯静态。
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/drama-hub ./cmd/drama-hub

# 每包一个测试二进制（`go test -c` 一次只能编一个包）。带 -race。
# 二进制按包路径放进镜像里的目录树，runner 才能把 cwd 设回该包目录，
# 否则 testdata/ 这类相对路径会找不到（go test 原生就是按包目录跑的）。
RUN set -eux; \
    mod=github.com/justforlxz/drama-hub; \
    for pkg in $(go list ./...); do \
      counts=$(go list -f '{{len .TestGoFiles}} {{len .XTestGoFiles}}' "$pkg"); \
      [ "$counts" = "0 0" ] && continue; \
      rel=${pkg#"$mod"/}; case "$pkg" in "$mod") rel=.;; esac; \
      mkdir -p "/out/tests/$rel"; \
      go test -c -race -o "/out/tests/$rel/pkg.test" "$pkg"; \
    done

# ---------------------------------------------------------------- runtime
# 唯一的 ffmpeg 真值来源。
FROM ${BASE} AS runtime

RUN apk add --no-cache ffmpeg tzdata \
 && adduser -D -u 1000 -h /data app \
 && mkdir -p /data && chown app:app /data

COPY --from=build /out/drama-hub /usr/local/bin/drama-hub

USER app
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080

ENV DRAMA_HUB_ADDR=:8080 \
    DRAMA_HUB_DB=/data/drama.db \
    DRAMA_HUB_LOG_LEVEL=info

ENTRYPOINT ["/usr/local/bin/drama-hub"]

# ---------------------------------------------------------------- e2e
# 只比 runtime 多一组已编译的测试二进制与一个 runner。
# 任何 ffmpeg 行为都在与 runtime 完全相同的用户空间里被测到。
FROM runtime AS e2e

USER root

COPY --from=build /out/tests /usr/local/bin/tests
COPY --from=build /src/scripts/run-tests.sh /usr/local/bin/run-tests
RUN chmod 0755 /usr/local/bin/run-tests \
 && find /usr/local/bin/tests -name pkg.test -exec chmod 0755 {} +

USER app
WORKDIR /tmp

ENTRYPOINT ["/usr/local/bin/run-tests"]
CMD ["-test.v"]
