#!/bin/sh
# 必须通过的静态检查（cwd=/src）。任一失败 → 退出 1。
set -eu

fail=0
mark() { echo "FAIL: $*" >&2; fail=1; }

echo "--- gofmt -l ---"
# gofmt 对未格式化文件仍退出 0，必须看输出。
unformatted=$(gofmt -l . 2>&1) || mark "gofmt 执行失败"
if [ -n "$unformatted" ]; then
	printf '%s\n' "$unformatted" >&2
	mark "以上文件未格式化（执行 gofmt -w）"
fi

echo "--- go build ./... ---"
go build ./... || mark "编译失败"

echo "--- go vet ./... ---"
go vet ./... || mark "vet 失败"

echo "--- staticcheck ./... ---"
staticcheck ./... || mark "staticcheck 失败（悬空变量/函数、未检查返回值等）"

if [ "$fail" -ne 0 ]; then
	echo "== lint 未通过 ==" >&2
	exit 1
fi
echo "== lint 通过 =="
