#!/bin/sh
# 发布镜像的 tag 口径，唯一真值（口径见 docs/adr/0008-image-publish.md）：
#   - 分支 push：出不可变 `sha-<7位>`；main 上额外更新 `latest`
#   - tag push（`v*`）：只出同名 tag，不动 `latest`，也不重出 `sha-*`
#     —— `sha-<7>` 的承诺是「同一个 commit 永远是同一份字节」，tag push 的
#     `VERSION` 与分支 push 不同（tag 名 vs sha），重出会把 sha tag 改写掉。
#
#   ./scripts/image-tags.sh <image> <ref> <sha>   打印要推的 tag，一行一个
#   ./scripts/image-tags.sh --self-test           内置 fixture 校验上面两行口径
#
# GITHUB_REF / GITHUB_SHA 直接透传即可：workflow 用的就是这两个变量。
set -eu

emit() {
	image=$1
	ref=$2
	sha=$3
	case "$ref" in
	refs/heads/*)
		printf '%s:sha-%s\n' "$image" "$(printf '%s' "$sha" | cut -c1-7)"
		if [ "$ref" = refs/heads/main ]; then
			printf '%s:latest\n' "$image"
		fi
		;;
	refs/tags/v*)
		printf '%s:%s\n' "$image" "${ref#refs/tags/}"
		;;
	esac
}

self_test() {
	fail=0
	img=ghcr.io/justforlxz/drama-hub
	sha=4f1c2ab9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f401

	check() {
		desc=$1
		want=$2
		got=$3
		if [ "$want" = "$got" ]; then
			printf 'OK   %s\n' "$desc"
		else
			printf 'FAIL %s\n  期望: [%s]\n  实际: [%s]\n' "$desc" "$want" "$got" >&2
			fail=1
		fi
	}

	check "main push：sha + latest" \
		"$img:sha-4f1c2ab
$img:latest" "$(emit "$img" refs/heads/main "$sha")"

	check "普通分支 push：只有 sha，latest 不动" \
		"$img:sha-4f1c2ab" "$(emit "$img" refs/heads/feat/image-ci "$sha")"

	check "v* tag push：只有同名 tag，不重出 sha、不动 latest" \
		"$img:v0.1.0" "$(emit "$img" refs/tags/v0.1.0 "$sha")"

	check "非 v* tag：口径里不触发发布，输出为空" \
		"" "$(emit "$img" refs/tags/nightly "$sha")"

	if [ "$fail" -ne 0 ]; then
		echo "image-tags: 自测失败" >&2
		exit 1
	fi
	echo "image-tags: 自测通过"
}

if [ "${1:-}" = "--self-test" ]; then
	self_test
	exit 0
fi

if [ "$#" -ne 3 ]; then
	echo "用法: $0 <image> <ref> <sha>   或   $0 --self-test" >&2
	exit 2
fi

emit "$1" "$2" "$3"
