# syntax=docker/dockerfile:1
#
# 三段式：build / runtime / e2e。
# ffmpeg 只出现在 runtime 段，e2e 段从 runtime 派生 —— 所以「测试用的 ffmpeg」
# 与「线上跑的 ffmpeg」是同一个二进制，由镜像结构保证，不靠纪律。
#
# 基础镜像按 digest 固定，见 env.lock。只固定 tag 挡不住上游重推。

ARG BASE=alpine@sha256:ee16d92f96db08d9d2d07da48555a27f3a69d8f0759021dbc601195ed0d2b5bd
ARG GOIMG=golang@sha256:bff16a94c0b4b6d9642581e2a3aa81beb3edb940b2f5a3aac1d22c6a68cb7747

# ---------------------------------------------------------------- build
FROM ${GOIMG} AS build

WORKDIR /src

# 依赖层单独缓存：go.mod/go.sum 不变时无需重下模块。
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none

ENV CGO_ENABLED=0 GOFLAGS=-trimpath

RUN go build -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/drama-hub ./cmd/drama-hub

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
# 集成测试环境 = runtime + Go 工具链。ffmpeg 与 runtime 同源，
# 所以调 ffmpeg 的测试跑在与线上完全一致的用户空间里。
#
# gcc/musl-dev 是为了 -race（race 检测依赖 cgo）；这个镜像只用于开发与 CI。
FROM runtime AS e2e

USER root

RUN apk add --no-cache build-base

COPY --from=build /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH \
    CGO_ENABLED=1 \
    GOCACHE=/tmp/go-build \
    GOMODCACHE=/tmp/go-mod

RUN mkdir -p /src /tmp/go-build /tmp/go-mod && chown -R app:app /src /tmp/go-build /tmp/go-mod

USER app
WORKDIR /src

ENTRYPOINT ["go"]
CMD ["test", "-race", "-timeout", "5m", "./..."]
