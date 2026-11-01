#!/bin/sh
# 运行 build 段编译好的测试二进制。
#
#   run-tests unit [参数]         unit 级：不碰数据库，容器应 --network=none
#   run-tests integration [参数]  integration 级：需要 DRAMA_HUB_TEST_DB_URL 指向 PG
#   run-tests all [参数]          两级一起跑（本地排查用）
#
# 分级是显式清单（UNIT_PKGS）：unit 只有「纯函数、不碰数据库」的包，其余一律进
# integration。默认落在更严的一级 —— 忘了登记的新包会要求 PG，而不是静默当纯单测跑。
#
# 参数原样透传给每个测试二进制（如 -test.v、-test.run）。
set -eu

root=/usr/local/bin/tests
src=/src

mode=${1:-all}
case "$mode" in
	unit | integration | all) shift ;;
	*)
		echo "用法: run-tests <unit|integration|all> [测试二进制参数]" >&2
		exit 2
		;;
esac

if [ "$mode" != unit ] && [ -z "${DRAMA_HUB_TEST_DB_URL:-}" ]; then
	echo "integration 级需要 DRAMA_HUB_TEST_DB_URL 指向测试用 PostgreSQL（scripts/ci.sh 会起服务容器）" >&2
	exit 2
fi

# unit 级清单：不 import 数据库驱动的包（httpx / plugin / 插件实现）。
UNIT_PKGS="internal/httpx internal/plugin internal/plugin/hongguo"

is_unit() {
	for p in $UNIT_PKGS; do
		[ "$p" = "$1" ] && return 0
	done
	return 1
}

in_tier() {
	case "$mode" in
	all) return 0 ;;
	unit) is_unit "$1" ;;
	integration) ! is_unit "$1" ;;
	esac
}

found=0
status=0
for t in $(find "$root" -name pkg.test | sort); do
	rel=${t#"$root"/}
	rel=${rel%/pkg.test}
	in_tier "$rel" || continue

	found=$((found + 1))
	dir="$src/$rel"
	[ "$rel" = "." ] && dir="$src"
	echo "==> [$mode] $rel"
	if [ -d "$dir" ]; then
		(cd "$dir" && "$t" "$@") || status=1
	else
		echo "  源码目录不存在：$dir" >&2
		status=1
	fi
done

if [ "$found" -eq 0 ]; then
	echo "没有找到 $mode 级的测试二进制：$root" >&2
	exit 1
fi

exit "$status"
