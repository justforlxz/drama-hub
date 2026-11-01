#!/usr/bin/env bash
# drama-hub CI 单入口。本地与 GitHub Actions 跑同一份，避免两套结论。
#
#   ./scripts/ci.sh                全量：lint + unit + integration + 版本锁
#   ./scripts/ci.sh --lint         只跑静态检查（含「测试不碰真实上游」门禁）
#   ./scripts/ci.sh --unit         只跑 unit 级（容器 --network=none，不碰数据库）
#   ./scripts/ci.sh --integration  只跑 integration 级（起 postgres:16 服务容器）
#   ./scripts/ci.sh --test         unit + integration
#
# 两级分工（ADR-0009）：
#   unit        纯函数与不碰数据库的包，容器 --network=none；
#   integration 起 postgres:16 服务容器并建自定义网络，测试容器同网络、用
#               DRAMA_HUB_TEST_DB_URL 连它。测试的上游一律 mock（httptest / fixture），
#               scripts/check-test-upstream.sh 把这条从纪律变成检查。
#
# 环境：ENGINE=podman（默认）| docker
set -euo pipefail
cd "$(dirname "$0")/.."

ENGINE=${ENGINE:-podman}
export CONTAINER_ENGINE=$ENGINE

# 版本锁（PG_IMAGE / STATICCHECK_VERSION …）从 env.lock 读，避免第二份真值。
# shellcheck disable=SC1091
. ./env.lock

want_lint=0
want_unit=0
want_integration=0
if [ $# -eq 0 ]; then
  want_lint=1
  want_unit=1
  want_integration=1
fi
for arg in "$@"; do
  case "$arg" in
    --lint) want_lint=1 ;;
    --unit) want_unit=1 ;;
    --integration) want_integration=1 ;;
    --test) want_unit=1; want_integration=1 ;;
    *) echo "未知参数: $arg" >&2; exit 2 ;;
  esac
done

# 门禁在两级之前跑：任何一条路径（含只跑测试）都先确认测试代码不碰真实上游。
./scripts/check-test-upstream.sh

lint_img=localhost/drama-hub-lint:local
test_img=localhost/drama-hub-e2e:local
[ "$ENGINE" = docker ] && lint_img=drama-hub-lint:local && test_img=drama-hub-e2e:local

# podman 与 docker 的参数差异只写这一处。
#   run_container <network> <img> [args...]
run_container() {
  local net=$1 img=$2
  shift 2
  local -a extra=()
  if [ -n "${TEST_DB_URL:-}" ]; then
    extra=(-e "DRAMA_HUB_TEST_DB_URL=$TEST_DB_URL")
  fi
  if [ "$ENGINE" = docker ]; then
    "$ENGINE" run --rm --network "$net" --user "$(id -u):$(id -g)" \
      "${extra[@]}" \
      -v "$PWD:/src:ro" -w /src --tmpfs /tmp:exec "$img" "$@"
  else
    "$ENGINE" run --rm --pull=never --network "$net" --userns=keep-id \
      --security-opt label=disable "${extra[@]}" \
      -v "$PWD:/src:ro,Z" -w /src \
      --tmpfs /tmp:exec "$img" "$@"
  fi
}

build_lint() {
  # 版本来自 env.lock，避免第二份真值。
  timeout 900 "$ENGINE" build -f Containerfile --target lint \
    --build-arg "STATICCHECK_VERSION=${STATICCHECK_VERSION}" -t "$lint_img" .
}

if [ "$want_lint" -eq 1 ]; then
  echo "== 构建 lint 镜像 =="
  build_lint
  echo "== lint =="
  run_container none "$lint_img" scripts/ci-lint.sh
fi

if [ "$want_unit" -eq 1 ] || [ "$want_integration" -eq 1 ]; then
  echo "== 构建测试镜像 =="
  timeout 1200 "$ENGINE" build -f Containerfile --target e2e -t "$test_img" .
fi

if [ "$want_unit" -eq 1 ]; then
  echo "== unit 级（--network=none，不碰数据库） =="
  run_container none "$test_img" unit -test.timeout 5m
fi

# integration 级的 PG 服务容器：临时网络 + 临时容器，退出即清理。
# fsync 关掉只针对这个用完即弃的测试库：DDL 密集的测试套在 fsync 上会慢一个量级，
# 而它不承载任何需要持久化的数据。
pg_started=0
cleanup_pg() {
  [ "$pg_started" -eq 1 ] || return 0
  "$ENGINE" rm -f "$pg_name" >/dev/null 2>&1 || true
  "$ENGINE" network rm "$net_name" >/dev/null 2>&1 || true
}
run_integration() {
  net_name="drama-hub-ci-net-$$"
  pg_name="drama-hub-ci-pg-$$"
  trap cleanup_pg EXIT INT TERM
  "$ENGINE" network create "$net_name" >/dev/null
  "$ENGINE" run -d --name "$pg_name" --network "$net_name" \
    -e POSTGRES_USER=drama -e POSTGRES_PASSWORD=drama -e POSTGRES_DB=drama \
    --health-cmd "pg_isready -U drama -d drama" --health-interval 1s \
    --health-timeout 2s --health-retries 60 \
    "$PG_IMAGE" -c fsync=off -c synchronous_commit=off -c full_page_writes=off >/dev/null
  pg_started=1

  # 健康检查由容器自己跑；这里再等一次是为了在测试前给出可读的失败原因。
  local ready=0
  for _ in $(seq 1 60); do
    if "$ENGINE" exec "$pg_name" pg_isready -U drama -d drama >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  if [ "$ready" -ne 1 ]; then
    echo "postgres 未就绪，容器日志：" >&2
    "$ENGINE" logs "$pg_name" >&2 || true
    return 1
  fi

  # 用容器 IP 而不是名字：docker / podman 对自定义网络内 DNS 的支持不一致。
  local pg_ip
  pg_ip=$("$ENGINE" inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$pg_name")
  if [ -z "$pg_ip" ]; then
    echo "取不到 postgres 容器 IP" >&2
    return 1
  fi
  TEST_DB_URL="postgres://drama:drama@${pg_ip}:5432/drama?sslmode=disable"
  export TEST_DB_URL

  echo "== integration 级（PG ${pg_ip}，测试只连它） =="
  run_container "$net_name" "$test_img" integration -test.timeout 10m
}

if [ "$want_integration" -eq 1 ]; then
  run_integration
fi

echo "== 版本锁 =="
./scripts/env-check.sh

echo "== ci.sh 通过 =="
