#!/bin/sh
# 依赖方向检查：内部 import 仅允许 rank(importer) > rank(importee)。
# 同级、反向、未登记包一律失败。POSIX sh（lint 容器内 busybox /bin/sh，无 bash）。
#
#   ./scripts/check-deps.sh              常规检查
#   ./scripts/check-deps.sh --self-test  内置 fixture 走同一校验逻辑，证明检查非恒真
set -eu

# rank 越大越靠上。本表是唯一真值：新增包必须在此注册，否则检查失败。
RANKS="cmd/drama-hub 60
internal/web 50
internal/webdav 40
internal/stream 40
internal/segment 40
internal/sync 30
internal/vfs 30
internal/httpx 30
internal/plugin/hongguo 20
internal/cache 20
internal/plugin 10
internal/store 10"

mod=github.com/justforlxz/drama-hub
prefix="$mod/"

# go list 格式串：三类 import 全采。.Imports 非测试文件；.TestImports 同包测试
# （package store）；.XTestImports 外部测试包（package store_test）。漏掉任一类
# 都可能让测试绕过层级检查。生产与 --self-test 共用此串，避免两处漂移。
GOLIST_FMT='{{.ImportPath}}{{range .Imports}} {{.}}{{end}}{{range .TestImports}} {{.}}{{end}}{{range .XTestImports}} {{.}}{{end}}'

# rank_of <pkg> → 打印 rank；未登记则空。
rank_of() {
    _name=$1
    while read -r name rank; do
        if [ "$name" = "$_name" ]; then
            printf '%s\n' "$rank"
            return 0
        fi
    done <<EOF
$RANKS
EOF
}

# classify_edge <importer> <importee> —— 违规行追加到 $OUT。
# 外部模块（非本仓库前缀）忽略；未登记、反向、同级均记为违规。
classify_edge() {
    importer=$1
    dep=$2
    case "$dep" in
        "$prefix"*) ;;
        *) return 0 ;;
    esac
    dep_rel=${dep#"$prefix"}
    r_imp=$(rank_of "$importer")
    r_ie=$(rank_of "$dep_rel")
    if [ -z "$r_ie" ]; then
        printf '未登记包: %s（在 scripts/check-deps.sh 的 RANKS 表注册）\n' "$dep_rel" >>"$OUT"
        return 0
    fi
    if [ -z "$r_imp" ]; then
        printf '未登记包: %s（在 scripts/check-deps.sh 的 RANKS 表注册）\n' "$importer" >>"$OUT"
        return 0
    fi
    if [ "$r_imp" -lt "$r_ie" ]; then
        printf '反向依赖: %s → %s（rank %s → %s）\n' "$importer" "$dep_rel" "$r_imp" "$r_ie" >>"$OUT"
    elif [ "$r_imp" -eq "$r_ie" ]; then
        printf '同级依赖: %s → %s（rank %s = %s）\n' "$importer" "$dep_rel" "$r_imp" "$r_ie" >>"$OUT"
    fi
}

# 校验一行 go list 输出：完整 ImportPath 后跟若干完整 import 目标。
check_line() {
    line=$1
    [ -z "$line" ] && return 0
    set -- $line
    importer=$1
    shift
    case "$importer" in
        "$prefix"*) importer_rel=${importer#"$prefix"} ;;
        *) return 0 ;;  # 非本仓库包（理论上不出现），跳过
    esac
    for dep in "$@"; do
        classify_edge "$importer_rel" "$dep"
    done
}

# 内置 fixture：合法边 2、反向 2、同级 1、未登记 0、外部 1 —— 恰 3 条违规。
# 输入格式与 go list 实际输出一致（完整 import path）。
self_test() {
    # 先钉住字段选择：GOLIST_FMT 必须同时含 .Imports / .TestImports / .XTestImports。
    # 漏 .XTestImports 会让外部测试包（package x_test）绕过层级检查（Finding 1）。
    case "$GOLIST_FMT" in
        *'.Imports'*) ;;
        *) echo "check-deps: 自测失败（格式串漏 .Imports）" >&2; exit 1 ;;
    esac
    case "$GOLIST_FMT" in
        *'.TestImports'*) ;;
        *) echo "check-deps: 自测失败（格式串漏 .TestImports）" >&2; exit 1 ;;
    esac
    case "$GOLIST_FMT" in
        *'.XTestImports'*) ;;
        *) echo "check-deps: 自测失败（格式串漏 .XTestImports，外部测试包可绕过）" >&2; exit 1 ;;
    esac
    OUT=$(mktemp)
    trap 'rm -f "$OUT"' EXIT INT TERM HUP
    m=github.com/justforlxz/drama-hub
    check_line "$m/internal/web $m/internal/store"
    check_line "$m/internal/plugin/hongguo $m/internal/plugin"
    check_line "$m/internal/store $m/internal/web"
    check_line "$m/internal/vfs $m/internal/sync"
    # 注册 httpx(rank 30) 后，这条边由「未登记」变「反向」（cache rank 20 → httpx 30），
    # 违规数仍为 3，自测语义不变。
    check_line "$m/internal/cache $m/internal/httpx"
    check_line "$m/internal/store github.com/other/module"
    got=$(wc -l <"$OUT" | tr -d ' ')
    if [ "$got" -ne 3 ]; then
        echo "check-deps: 自测失败（预期 3 条违规，实际 $got）" >&2
        cat "$OUT" >&2
        exit 1
    fi
    echo "check-deps: 自测通过"
    exit 0
}

if [ "${1:-}" = "--self-test" ]; then
    self_test
fi

OUT=$(mktemp)
list=$(mktemp)
trap 'rm -f "$OUT" "$list"' EXIT INT TERM HUP

# 产出登记用的包路径清单：go list 失败即失败。格式串见 GOLIST_FMT。
go list -f "$GOLIST_FMT" ./... >"$list"

# 每个登记包必须真实存在，防止包被删后检查静默通过。
while read -r name _rank; do
    if ! go list "$mod/$name" >/dev/null 2>&1; then
        printf '登记包不存在: %s（RANKS 表与仓库不符）\n' "$name" >>"$OUT"
    fi
done <<EOF
$RANKS
EOF

while read -r line; do
    check_line "$line"
done <"$list"

if [ -s "$OUT" ]; then
    cat "$OUT"
    echo "check-deps: FAIL"
    exit 1
fi
count=$(wc -l <"$list" | tr -d ' ')
echo "check-deps: OK（${count} 个包，import 方向全部合法）"
