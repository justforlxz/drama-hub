#!/usr/bin/env bash
# 校验当前镜像与 env.lock 是否一致：Go 版本、ffmpeg 版本、关键编码器与封装器。
# 不一致直接退出非零 —— 这是「开发与测试环境一致」从口号变成检查的那一步。
set -euo pipefail
cd "$(dirname "$0")/.."

lock=env.lock
[[ -f $lock ]] || { echo "缺少 env.lock" >&2; exit 1; }
# shellcheck disable=SC1090
source "$lock"

engine=${CONTAINER_ENGINE:-podman}
tag=localhost/drama-hub-envcheck

fail() { printf '\033[31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }
ok()   { printf '\033[32m OK \033[0m %s\n' "$*"; }

echo "== 构建 runtime 镜像 =="
"$engine" build -f Containerfile --target runtime -t "$tag" . >/dev/null
echo "== 构建 lint 镜像 =="
tag_lint=localhost/drama-hub-lint:envcheck
"$engine" build -f Containerfile --target lint --build-arg "STATICCHECK_VERSION=$STATICCHECK_VERSION" -t "$tag_lint" . >/dev/null

echo "== Go 版本 =="
go_ver=$("$engine" run --rm --network=none "$GO_IMAGE" go version | awk '{print $3}')
[[ $go_ver == "$GO_VERSION" ]] || fail "go: 实际 $go_ver，期望 $GO_VERSION"
ok "go $go_ver"

echo "== ffmpeg 版本 =="
# 不接 head：podman 被 SIGPIPE 打断会让 pipefail 下的脚本静默退出 141，
# 恰好在版本不符（要学会报错）时失效。
ff_out=$("$engine" run --rm --network=none --entrypoint ffmpeg "$tag" -version)
ff_line=${ff_out%%$'\n'*}
ff_ver=$(sed -E 's/^ffmpeg version ([^ ]+).*/\1/' <<<"$ff_line")
[[ $ff_ver == "$FFMPEG_VERSION" ]] || fail "ffmpeg: 实际 $ff_ver，期望 $FFMPEG_VERSION（$ff_line）"
ok "ffmpeg $ff_ver"

echo "== staticcheck 版本 =="
sc_out=$("$engine" run --rm --network=none --entrypoint staticcheck "$tag_lint" -version)
sc_ver=$(sed -E 's/.*\(([0-9.]+)\).*/\1/' <<<"$sc_out")
[[ $sc_ver == "$STATICCHECK_VERSION" ]] || fail "staticcheck: 实际 $sc_ver，期望 $STATICCHECK_VERSION（$sc_out）"
ok "staticcheck $sc_ver"

echo
echo "环境与 env.lock 一致。"
