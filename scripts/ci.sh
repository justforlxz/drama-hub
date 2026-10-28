#!/usr/bin/env bash
# drama-hub CI 单入口。本地与 GitHub Actions 跑同一份，避免两套结论。
#
#   ./scripts/ci.sh          全量：lint + 容器测试 + 版本锁
#   ./scripts/ci.sh --lint   只跑静态检查
#   ./scripts/ci.sh --test   只跑容器测试
#
# 环境：ENGINE=podman（默认）| docker
set -euo pipefail
cd "$(dirname "$0")/.."

ENGINE=${ENGINE:-podman}
export CONTAINER_ENGINE=$ENGINE

run_lint=1
run_test=1
for arg in "$@"; do
  case "$arg" in
    --lint) run_test=0 ;;
    --test) run_lint=0 ;;
    *) echo "未知参数: $arg" >&2; exit 2 ;;
  esac
done

# podman 与 docker 的参数差异只写这一处。
run_in() {
  local img=$1; shift
  if [ "$ENGINE" = docker ]; then
    "$ENGINE" run --rm --network=none --user "$(id -u):$(id -g)" \
      -v "$PWD:/src:ro" -w /src --tmpfs /tmp:exec "$img" "$@"
  else
    "$ENGINE" run --rm --pull=never --network=none --userns=keep-id \
      --security-opt label=disable -v "$PWD:/src:ro,Z" -w /src \
      --tmpfs /tmp:exec "$img" "$@"
  fi
}

lint_img=localhost/drama-hub-lint:local
test_img=localhost/drama-hub-e2e:local
[ "$ENGINE" = docker ] && lint_img=drama-hub-lint:local && test_img=drama-hub-e2e:local

build_lint() {
  # 版本来自 env.lock，避免第二份真值。
  # shellcheck disable=SC1091
  . ./env.lock
  timeout 900 "$ENGINE" build -f Containerfile --target lint \
    --build-arg "STATICCHECK_VERSION=${STATICCHECK_VERSION}" -t "$lint_img" .
}

if [ "$run_lint" -eq 1 ]; then
  echo "== 构建 lint 镜像 =="
  build_lint
  echo "== lint =="
  run_in "$lint_img" scripts/ci-lint.sh
fi

if [ "$run_test" -eq 1 ]; then
  echo "== 构建测试镜像 =="
  timeout 1200 "$ENGINE" build -f Containerfile --target e2e -t "$test_img" .
  echo "== test =="
  run_in "$test_img" -test.timeout 5m
fi

echo "== 版本锁 =="
./scripts/env-check.sh

echo "== ci.sh 通过 =="
