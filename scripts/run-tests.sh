#!/bin/sh
# 运行 build 段编译好的全部测试二进制。
#
# 每个二进制放在镜像内与包路径一致的目录树下，运行前把 cwd 设回对应的 /src 子目录 ——
# go test 原生就是按包目录跑的，testdata/ 这类相对路径只有这样才能解析。
#
# 参数原样透传给每个测试二进制（如 -test.v、-test.run）。
set -eu

root=/usr/local/bin/tests
src=/src
found=0
status=0

for t in $(find "$root" -name pkg.test | sort); do
	found=1
	rel=${t#"$root"/}
	rel=${rel%/pkg.test}
	dir="$src/$rel"
	[ "$rel" = "." ] && dir="$src"
	echo "==> $rel"
	if [ -d "$dir" ]; then
		(cd "$dir" && "$t" "$@") || status=1
	else
		echo "  源码目录不存在：$dir" >&2
		status=1
	fi
done

if [ "$found" -eq 0 ]; then
	echo "没有找到测试二进制：$root" >&2
	exit 1
fi

exit "$status"
