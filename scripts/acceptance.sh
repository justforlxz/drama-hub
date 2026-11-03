#!/usr/bin/env bash
# 端到端验收脚本（MKACG-625）：部署 → 配置 → 同步 → 目录 → 数据 → 播放 → 合并集 → 持久 → 换链。
#
#   bash scripts/acceptance.sh            # 跑全部 A–I
#   bash scripts/acceptance.sh F G        # 只跑指定段
#
# 每项输出一行：PASS / FAIL / SKIP + 项号 + 结论 + 证据。任何 FAIL 让脚本退出码为 1；
# SKIP 表示该项在当前条件下无法判定（缺前置、上游不可达、容器不可控），**不是通过**。
#
# 这个脚本只做判定，不改代码、不改判据：判定口径逐条对应 MKACG-625 正文的 A–I，
# 判据不满足就报红，不为了「变绿」放宽断言。
#
# ------------------------------------------------------------------ 环境变量
#   BASE            服务地址，默认 http://127.0.0.1:8080
#   ADMIN_TOKEN     DRAMA_HUB_ADMIN_TOKEN（写操作令牌，必填）
#   WEBDAV_USER     新的 WebDAV 用户名，默认 emby
#   WEBDAV_PASS     新的 WebDAV 密码，默认由脚本生成
#   OLD_WEBDAV_PASS B 段「旧凭据必须 401」用的旧密码（缺省则跳过该子项）
#   SERIES_ID       上游剧 id（纯数字），C 段同步它
#   ENGINE          podman | docker，默认 podman
#   DH_CONTAINER    服务容器名（H 重启 / F 故障注入需要；缺省则相关子项 SKIP）
#   PG_CONTAINER    数据库容器名（A/B/C/D/E 查库需要；缺省则查库子项 SKIP）
#   DB_USER/DB_NAME 默认 drama / drama
#   CACHE_HOST_DIR  宿主机上的缓存目录（H 段比对用；缺省用容器内 du）
#   FFMPEG/FFPROBE  ffmpeg / ffprobe 可执行文件，默认 PATH 里的
#   RCLONE          rclone 可执行文件，默认 PATH 里的
#   PLAY_SECONDS    F 段实播秒数，默认 60（预览只有 10.07 秒，播过 60 秒即证明不是预览）
#   MERGE_CHUNK_ALT G 段「改 N」用的目标值，默认 5（验完自动改回原值）
# ------------------------------------------------------------------ 输出约定
# 所有外部命令都带 timeout，脚本不会挂死。
set -uo pipefail

BASE=${BASE:-http://127.0.0.1:8080}
ADMIN_TOKEN=${ADMIN_TOKEN:-${DRAMA_HUB_ADMIN_TOKEN:-}}
WEBDAV_USER=${WEBDAV_USER:-emby}
WEBDAV_PASS=${WEBDAV_PASS:-acc-$(date +%s)}
OLD_WEBDAV_PASS=${OLD_WEBDAV_PASS:-}
SERIES_ID=${SERIES_ID:-}
ENGINE=${ENGINE:-podman}
DH_CONTAINER=${DH_CONTAINER:-}
PG_CONTAINER=${PG_CONTAINER:-}
DB_USER=${DB_USER:-drama}
DB_NAME=${DB_NAME:-drama}
CACHE_HOST_DIR=${CACHE_HOST_DIR:-}
FFMPEG=${FFMPEG:-ffmpeg}
FFPROBE=${FFPROBE:-ffprobe}
RCLONE=${RCLONE:-rclone}
PLAY_SECONDS=${PLAY_SECONDS:-60}
MERGE_CHUNK_ALT=${MERGE_CHUNK_ALT:-5}

WORK=${WORK:-$(mktemp -d)}
trap 'rm -rf "$WORK"' EXIT
RCLONE_CONFIG="$WORK/rclone.conf"
export RCLONE_CONFIG

FAILED=0
PASSED=0
SKIPPED=0

pass() { PASSED=$((PASSED + 1)); printf 'PASS  %-4s %s\n' "$1" "${*:2}"; }
fail() { FAILED=$((FAILED + 1)); printf 'FAIL  %-4s %s\n' "$1" "${*:2}"; }
skip() { SKIPPED=$((SKIPPED + 1)); printf 'SKIP  %-4s %s\n' "$1" "${*:2}"; }
info() { printf '      %-4s %s\n' "" "$*"; }
section() { printf '\n== %s ==\n' "$*"; }

need_bin() {
  for b in "$@"; do
    command -v "$b" >/dev/null 2>&1 || { echo "缺少命令: $b" >&2; exit 2; }
  done
}

# ---------------------------------------------------------------- 小工具
http_code() { # http_code <curl args...>
  timeout 60 curl -sS -o /dev/null -w '%{http_code}' "$@" 2>/dev/null || true
}
api() { # api <method> <path> [body] —— 带管理令牌；stdout 是状态码，响应体落 $WORK/api.body
  local method=$1 path=$2 body=${3:-}
  if [ -n "$body" ]; then
    timeout 120 curl -sS -o "$WORK/api.body" -w '%{http_code}' -X "$method" \
      -H "X-Admin-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d "$body" "$BASE$path"
  else
    timeout 120 curl -sS -o "$WORK/api.body" -w '%{http_code}' -X "$method" \
      -H "X-Admin-Token: $ADMIN_TOKEN" "$BASE$path"
  fi
}
propfind_code() { # propfind_code <user> <pass>
  local auth=()
  [ -n "$1" ] && auth=(-u "$1:$2")
  timeout 60 curl -sS -o /dev/null -w '%{http_code}' "${auth[@]}" \
    -X PROPFIND -H 'Depth: 1' "$BASE/webdav/" 2>/dev/null || true
}
json_get() { # json_get <field>  (从 stdin 的 JSON 里取顶层/嵌套字段)
  python3 -c 'import json,sys
d=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    d=d[k]
print(d if not isinstance(d,bool) else str(d).lower())' "$1" 2>/dev/null
}

db_ready() { [ -n "$PG_CONTAINER" ]; }
dbq() { # dbq <SQL> —— 一行一值（tAc 输出）
  timeout 60 "$ENGINE" exec "$PG_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -tAc "$1" 2>/dev/null | tr -d '\r'
}

# outward_items <drama_id> —— 按当前 merge_chunk_size 现算对外剧集，每行
# "ep_no|kind|uid"。库里没有 emby_item 表（ADR-0014），所以验收自己算一遍：
# 这正好是一份与被测代码独立的实现，用来对照目录与 .nfo 的集号。
outward_items() {
  dbq "WITH k AS (
         SELECT COALESCE((SELECT (value_json::json #>> '{}')::int
                            FROM settings WHERE key='merge_chunk_size'), 10) AS n),
       e AS (SELECT id, ep_no,
                    (row_number() OVER (ORDER BY ep_no) - 1) / (SELECT n FROM k) AS blk
               FROM episode WHERE drama_id=$1)
       SELECT (blk + 1) || '|' ||
              CASE WHEN count(*) = 1 THEN 'single' ELSE 'merged' END || '|' ||
              CASE WHEN count(*) = 1 THEN 'e' || min(id)
                   ELSE 'm$1_' || min(ep_no) || '_' || max(ep_no) END
         FROM e GROUP BY blk ORDER BY blk"
}

# outward_of_uid <drama_id> <uid> —— 该 uid 在当前分块下的 "ep_no|kind"（没有则空）。
outward_of_uid() {
  outward_items "$1" | awk -F'|' -v u="$2" '$3==u { print $1"|"$2 }'
}

# outward_runtime <drama_id> <uid> —— 该对外剧集的 runtime_sec（单集取本集时长、
# 合并集取块内成员之和，与 store.embyItems 同一口径）。
outward_runtime() {
  case "$2" in
    e*) dbq "SELECT duration_sec FROM episode WHERE id=${2#e}" ;;
    m*) dbq "SELECT coalesce(sum(duration_sec),0) FROM episode
              WHERE drama_id=$1 AND ep_no BETWEEN
                split_part(substr('$2', 2), '_', 2)::int AND split_part(substr('$2', 2), '_', 3)::int" ;;
    *) printf '0' ;;
  esac
}

rclone_setup() {
  timeout 60 "$RCLONE" config create dramahub webdav url "$BASE/webdav" vendor other \
    user "$WEBDAV_USER" pass "$WEBDAV_PASS" --non-interactive >/dev/null 2>&1
}

# strm_id_for_uid 从已刷新的清单里找某个 uid 的 .strm id（没有则空）。
strm_id_for_uid() { # strm_id_for_uid <uid>
  local want=$1 line id
  for line in $(season_files "$(find_show_dir)" strm | sed 's/ /%20/g'); do
    line=${line//%20/ }
    id=$(strm_id "$line")
    [ -n "$id" ] && [ "${id%%-*}" = "$want" ] && { echo "$id"; return 0; }
  done
  return 1
}

# refresh_list 重列一遍虚拟目录树（改 N 后目录立刻变，清单必须重取）
refresh_list() {
  rclone_setup
  timeout 300 "$RCLONE" lsf --files-only -R dramahub:/ >"$WORK/list.txt" 2>/dev/null
}

# ------------------------------------------------------------------ A 部署
stage_A() {
  section "A 部署"
  local code
  code=$(http_code "$BASE/healthz")
  if [ "$code" = "200" ]; then
    pass A1 "/healthz 200 — $(timeout 30 curl -sS "$BASE/healthz" | head -c 80)"
  else
    fail A1 "/healthz 期望 200，实得 $code"
  fi

  if [ -z "$DH_CONTAINER" ]; then
    skip A2 "日志检查：未给 DH_CONTAINER"
    return
  fi
  # 只看「最后一次启动」之后的日志：podman/docker 的 logs 跨重启累积，历史里的旧错误
  # 不属于本次部署。
  local logs
  logs=$("$ENGINE" logs "$DH_CONTAINER" 2>&1 | awk '/"msg":"starting drama-hub"/{buf=""} {buf=buf $0 "\n"} END{printf "%s", buf}')
  local bad
  bad=$(printf '%s\n' "$logs" | grep -cE '"level":"(ERROR|WARN)".*(panic|配置缺失|未设置 DRAMA_HUB|数据库不可用)')
  local panics
  panics=$(printf '%s\n' "$logs" | grep -ci 'panic')
  if [ "$bad" -eq 0 ] && [ "$panics" -eq 0 ]; then
    pass A2 "日志无 panic、无配置缺失告警 — $(printf '%s\n' "$logs" | head -1 | head -c 120)"
  else
    fail A2 "日志出现 $bad 条错误/告警、$panics 条 panic — $(printf '%s\n' "$logs" | grep -E 'panic|配置缺失|未设置 DRAMA_HUB|数据库不可用' | head -3)"
  fi
}

# ------------------------------------------------------------------ B 配置
stage_B() {
  section "B 配置（写操作令牌 / WebDAV 凭据热生效）"
  local code
  code=$(http_code -X PATCH -H 'Content-Type: application/json' \
    -d '{"settings":{"webdav_user":"x"}}' "$BASE/api/settings")
  if [ "$code" = "401" ]; then
    pass B1 "无令牌写配置 → 401"
  else
    fail B1 "无令牌写配置期望 401，实得 $code"
  fi

  if [ -z "$ADMIN_TOKEN" ]; then
    skip B2 "带令牌写配置：未给 ADMIN_TOKEN"
  else
    local body out
    body=$(printf '{"settings":{"webdav_user":"%s","webdav_pass":"%s"}}' "$WEBDAV_USER" "$WEBDAV_PASS")
    code=$(api PATCH /api/settings "$body")
    out=$(cat "$WORK/api.body")
    if [ "$code" = "200" ]; then
      pass B2 "带 admin_token 写 webdav_user/webdav_pass → 200 — $out"
    else
      fail B2 "带令牌写配置期望 200，实得 $code — $out"
    fi

    code=$(propfind_code "$WEBDAV_USER" "$WEBDAV_PASS")
    if [ "$code" = "207" ]; then
      pass B3 "PROPFIND 新凭据 → 207"
    else
      fail B3 "PROPFIND 新凭据期望 207，实得 $code"
    fi
    code=$(propfind_code "" "")
    if [ "$code" = "401" ]; then
      pass B4 "PROPFIND 空凭据 → 401"
    else
      fail B4 "PROPFIND 空凭据期望 401，实得 $code"
    fi
    if [ -n "$OLD_WEBDAV_PASS" ]; then
      code=$(propfind_code "$WEBDAV_USER" "$OLD_WEBDAV_PASS")
      if [ "$code" = "401" ]; then
        pass B5 "PROPFIND 旧凭据 → 401（不重启即生效）"
      else
        fail B5 "PROPFIND 旧凭据期望 401，实得 $code"
      fi
    else
      skip B5 "旧凭据 401：未给 OLD_WEBDAV_PASS"
    fi
  fi
}

# ------------------------------------------------------------------ C 同步
DRAMA_ID=""
SYNC_JSON=""
stage_C() {
  section "C 同步一部真实上游的剧"
  if [ -z "$SERIES_ID" ]; then
    skip C1 "未给 SERIES_ID"
    return
  fi
  local code
  code=$(api POST /api/dramas "{\"series_id\":\"$SERIES_ID\"}")
  SYNC_JSON=$(cat "$WORK/api.body")
  case "$code" in
    200 | 201) pass C1 "POST /api/dramas {series_id:$SERIES_ID} → $code" ;;
    *) fail C1 "POST /api/dramas 期望 200/201，实得 $code — $(echo "$SYNC_JSON" | head -c 200)" ;;
  esac
  DRAMA_ID=$(echo "$SYNC_JSON" | python3 -c 'import json,sys
d=json.load(sys.stdin)["dramas"][0]
print(d["id"])' 2>/dev/null)
  DRAMA_JSON=$(echo "$SYNC_JSON" | python3 -c 'import json,sys
print(json.dumps(json.load(sys.stdin)["dramas"][0],ensure_ascii=False))' 2>/dev/null)
  [ -n "$DRAMA_ID" ] && info "" "drama_id=$DRAMA_ID $(echo "$DRAMA_JSON" | head -c 200)"

  if ! db_ready; then
    skip C2 "集数比对：未给 PG_CONTAINER"
    return
  fi
  local declared rows lo hi distinct
  declared=$(echo "$SYNC_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["dramas"][0]["episode_total"])' 2>/dev/null)
  rows=$(dbq "SELECT count(*) FROM episode WHERE drama_id=$DRAMA_ID")
  distinct=$(dbq "SELECT count(DISTINCT ep_no) FROM episode WHERE drama_id=$DRAMA_ID")
  lo=$(dbq "SELECT min(ep_no) FROM episode WHERE drama_id=$DRAMA_ID")
  hi=$(dbq "SELECT max(ep_no) FROM episode WHERE drama_id=$DRAMA_ID")
  if [ "$rows" = "$declared" ] && [ "$distinct" = "$declared" ] && [ "$lo" = "1" ] && [ "$hi" = "$declared" ]; then
    pass C2 "库中 episode 行数 = 上游声明集数 = $declared（distinct=$distinct，ep_no 1..$hi 连续，无重复）"
  else
    fail C2 "episode 行数/集号不符：declared=$declared rows=$rows distinct=$distinct ep_no=$lo..$hi"
  fi
}

# ------------------------------------------------------------------ D 目录
stage_D() {
  section "D 目录（rclone 从 /webdav 列）"
  rclone_setup
  local list
  list="$WORK/list.txt"
  if ! timeout 300 "$RCLONE" lsf --files-only -R dramahub:/ >"$list" 2>"$WORK/rclone.err"; then
    fail D1 "rclone lsf -R 失败 — $(head -3 "$WORK/rclone.err")"
    return
  fi
  local n
  n=$(wc -l <"$list")
  pass D1 "rclone lsf --files-only -R 列出 $n 个文件"

  local top
  top=$(timeout 60 "$RCLONE" lsf --dirs-only dramahub:/ 2>/dev/null)
  if printf '%s\n' "$top" | grep -q '^全部/$'; then
    fail D2 "顶层出现 全部/ 目录"
  else
    pass D2 "顶层无 全部/ — $(printf '%s\n' "$top" | tr '\n' ' ' | head -c 120)"
  fi

  # 形状：<分类>/<剧>/Season 01/<文件>（分片层已下线，见 docs/DIRECTORY-CONTRACT.md v5）
  local shape
  shape='^[^/]+/[^/]+/(tvshow\.nfo|Season 01/[^/]+\.(strm|nfo))$'
  badshape=$(grep -vcE "$shape" "$list" || true)
  if [ "$badshape" -eq 0 ]; then
    pass D3 "全部文件形状为 <分类>/<剧>/{tvshow.nfo,Season 01/*.{strm,nfo}}"
  else
    fail D3 "$badshape 个文件形状不符 — $(grep -vE "$shape" "$list" | head -3)"
  fi
  # 分片层已下线：任何两层十六进制段出现即失败（分片目录叫 00–7f）。
  if grep -qE '^[^/]+/[0-9a-f]{2}/' "$list"; then
    fail D3b "路径里仍有两位十六进制段（分片层没删干净）— $(grep -E '^[^/]+/[0-9a-f]{2}/' "$list" | head -3)"
  else
    pass D3b "路径里没有两位十六进制段（分片层已下线）"
  fi

  if ! db_ready; then
    skip D4 "文件数与库比对：未给 PG_CONTAINER"
    return
  fi
  local want got genres empty
  # 文件数 = 2×对外集数 + 每剧 1 个 tvshow.nfo；对外集数按当前 N 现算（ADR-0014）
  want=$(dbq "WITH k AS (
                SELECT COALESCE((SELECT (value_json::json #>> '{}')::int
                                   FROM settings WHERE key='merge_chunk_size'), 10) AS n),
              e AS (SELECT drama_id,
                           (row_number() OVER (PARTITION BY drama_id ORDER BY ep_no) - 1)
                             / (SELECT n FROM k) AS blk
                      FROM episode)
              SELECT 2 * count(*) + (SELECT count(*) FROM drama)
                FROM (SELECT DISTINCT drama_id, blk FROM e) t")
  got=$n
  if [ "$got" = "$want" ]; then
    pass D4 "文件数 = 现算的对外集数×2 + 每剧 tvshow.nfo = $got"
  else
    fail D4 "文件数 $got ≠ 现算的对外集数×2 + 剧数 = $want"
  fi
  genres=$(dbq "SELECT count(DISTINCT genre) FROM drama WHERE genre<>''")
  empty=$(dbq "SELECT count(*) FROM drama WHERE genre=''")
  if [ "$empty" = "0" ]; then
    skip D5 "兜底分类：本次同步的剧 genre 均非空，无法观测「其他」兜底"
  else
    if printf '%s\n' "$top" | grep -q '^其他/$'; then
      pass D5 "genre 为空的 $empty 部剧落在兜底目录 其他/"
    else
      fail D5 "有 $empty 部剧 genre 为空，但顶层没有 其他/"
    fi
  fi
  info "" "分类数 $genres"
}

# ------------------------------------------------------------------ E 数据正确性
find_show_dir() { # find_show_dir —— 本次验收的剧目录（C 段未跑时取库里第一部）
  if [ -z "$DRAMA_ID" ] && [ -n "$PG_CONTAINER" ]; then
    DRAMA_ID=$(dbq "SELECT id FROM drama ORDER BY id LIMIT 1")
  fi
  [ -n "$DRAMA_ID" ] || return 1
  grep -E "^[^/]+/[^/]+/tvshow\.nfo$" "$WORK/list.txt" | head -1 | sed 's#/tvshow\.nfo$##'
}

season_files() { # season_files <show dir> <strm|nfo> —— 该剧季目录下的文件（固定串匹配，
  # 剧名里有 ( ) [ ] 这类字符，走 grep -E 会被当成正则）
  grep -F "$1/Season 01/" "$WORK/list.txt" | grep -E "\.$2$"
}
strm_id() { # strm_id <path> —— 取 .strm 里的 id
  timeout 60 "$RCLONE" cat "dramahub:/$1" 2>/dev/null | head -1 |
    sed -nE 's#^.*/video/([^/]+)\.m3u8$#\1#p'
}

stage_E() {
  section "E 数据正确性（逐项核对）"
  if [ ! -s "$WORK/list.txt" ]; then
    fail E1 "没有目录清单（D 段未跑通），无法核对"
    return
  fi
  local show
  show=$(find_show_dir)
  if [ -z "$show" ]; then
    fail E1 "找不到剧目录（<分类>/<剧>/tvshow.nfo）"
    return
  fi
  info "" "剧目录: $show"

  # E1 tvshow.nfo：合法 XML + 标题/年份/简介与库一致
  timeout 60 "$RCLONE" cat "dramahub:/$show/tvshow.nfo" >"$WORK/tvshow.nfo" 2>/dev/null
  local e1
  e1=$(python3 - "$WORK/tvshow.nfo" <<'PY' 2>&1
import sys, xml.etree.ElementTree as ET
try:
    r = ET.parse(sys.argv[1]).getroot()
except Exception as e:
    print("非法 XML: %s" % e); sys.exit(0)
if r.tag != "tvshow":
    print("根元素是 %s，不是 tvshow" % r.tag); sys.exit(0)
print("OK|%s|%s|%s" % (r.findtext("title"), r.findtext("year"), (r.findtext("plot") or "")[:20]))
PY
)
  if ! db_ready; then
    case "$e1" in
      OK\|*) pass E1 "tvshow.nfo 合法 XML（根 tvshow）— $e1" ;;
      *) fail E1 "tvshow.nfo 解析失败 — $e1" ;;
    esac
  else
    local db_title db_year db_plot
    db_title=$(dbq "SELECT title FROM drama WHERE id=$DRAMA_ID")
    db_year=$(dbq "SELECT year FROM drama WHERE id=$DRAMA_ID")
    db_plot=$(dbq "SELECT left(plot,20) FROM drama WHERE id=$DRAMA_ID")
    if [ "${e1%%|*}" = "OK" ] &&
      [ "$(echo "$e1" | cut -d'|' -f2)" = "$db_title" ] &&
      [ "$(echo "$e1" | cut -d'|' -f3)" = "$db_year" ] &&
      [ "$(echo "$e1" | cut -d'|' -f4)" = "$db_plot" ]; then
      pass E1 "tvshow.nfo 合法 XML，title/year/plot 与库一致 — $e1"
    else
      fail E1 "tvshow.nfo 与库不一致 — nfo=$e1 / db=title:$db_title year:$db_year plot:$db_plot"
    fi
    # 口径说明：tvshow.nfo 只有 title/year/plot（docs/DIRECTORY-CONTRACT.md 冻结的内容）；
    # 「季/集数」落在目录与现算的对外集号上，见 E1b。
    local seasons eps
    seasons=$(grep -cF "$show/Season 01/" "$WORK/list.txt")
    eps=$(outward_items "$DRAMA_ID" | wc -l | tr -d ' ')
    if [ "$seasons" -eq $((2 * eps)) ]; then
      pass E1b "季目录 Season 01 文件数 = 2×对外集数（$eps 集，按当前 N 现算）"
    else
      fail E1b "Season 01 文件数 $seasons ≠ 2×$eps"
    fi
  fi

  # E2/E4：逐集 nfo 的 season/episode 与现算的对外集号一致；strm id 可 GET
  local n=0 bad_nfo=0 bad_strm=0 bad_get=0 code200=0 code502=0 first=1
  local line ep
  for line in $(season_files "$show" strm | sed 's/ /%20/g'); do
    line=${line//%20/ }
    ep=$(strm_id "$line")
    if [ -z "$ep" ]; then
      bad_strm=$((bad_strm + 1))
      continue
    fi
    if ! printf '%s\n' "$ep" | grep -qE '^(e[0-9]+|m[0-9]+_[0-9]+_[0-9]+)-[0-9a-f]{12}$'; then
      bad_strm=$((bad_strm + 1))
    fi
    local code
    code=$(http_code "$BASE/video/$ep.m3u8")
    case "$code" in
      200) code200=$((code200 + 1)) ;;
      502) code502=$((code502 + 1)) ;; # 契约允许的「这一集解析不到片源」，不是死链
      *) bad_get=$((bad_get + 1)) ;;
    esac
    n=$((n + 1))
    [ "$first" -eq 1 ] && info "" "示例 .strm id=$ep → /video/$ep.m3u8 $code"
    first=0
  done
  if [ "$bad_strm" -eq 0 ] && [ "$n" -gt 0 ]; then
    pass E2 "全部 $n 条 .strm 匹配 ^/video/[em][0-9]+-[0-9a-f]{12}\\.m3u8$"
  else
    fail E2 "$bad_strm 条 .strm 形态不符（共 $n 条）"
  fi
  # 「该 URL 可直接 GET」= 指向本服务且按契约应答；404/401/连接失败才是死链。
  # 502 是 PLAYBACK-CONTRACT 里「解析不到片源」的既定应答，单独计数并写进结论。
  if [ "$bad_get" -eq 0 ] && [ "$n" -gt 0 ]; then
    pass E3 "全部 $n 条 .strm URL 可直接 GET（$code200 条 200，$code502 条 502=契约的「解析不到片源」）"
  else
    fail E3 "$bad_get/$n 条 .strm URL GET 既非 200 也非 502（$code200 条 200，$code502 条 502）"
  fi

  if ! db_ready; then
    skip E4 "集 .nfo 与现算的对外集号一致：未给 PG_CONTAINER"
    skip E5 "runtime 与实测时长：未给 PG_CONTAINER"
    return
  fi

  # E4 集号一致性：id → 现算的对外集号 / season，与同目录 .nfo 的 <episode>/<season> 比对
  local nfo_bad=0 checked=0
  for line in $(season_files "$show" nfo | sed 's/ /%20/g'); do
    line=${line//%20/ }
    local id want_season want_ep got_season got_ep
    id=$(strm_id "$(echo "$line" | sed 's/\.nfo$/.strm/')")
    [ -z "$id" ] && continue
    want_ep=$(outward_of_uid "$DRAMA_ID" "${id%%-*}" | cut -d'|' -f1)
    want_season=1   # 2.0 只有单季（对外剧集的 Season 恒为 1）
    [ -z "$want_ep" ] && continue
    timeout 60 "$RCLONE" cat "dramahub:/$line" >"$WORK/ep.nfo" 2>/dev/null
    read -r got_season got_ep <<EOF
$(python3 - "$WORK/ep.nfo" <<'PY'
import sys, xml.etree.ElementTree as ET
try:
    r = ET.parse(sys.argv[1]).getroot()
    print(r.findtext("season") or "?", r.findtext("episode") or "?")
except Exception:
    print("?", "?")
PY
)
EOF
    checked=$((checked + 1))
    if [ "$got_season" != "$want_season" ] || [ "$got_ep" != "$want_ep" ]; then
      nfo_bad=$((nfo_bad + 1))
      [ "$nfo_bad" -le 3 ] && info "" "集号不符: $line nfo=$got_season/$got_ep db=$want_season/$want_ep"
    fi
  done
  if [ "$nfo_bad" -eq 0 ] && [ "$checked" -gt 0 ]; then
    pass E4 "$checked 集 .nfo 的 season/episode 与现算的对外集号逐一相符"
  else
    fail E4 "$nfo_bad/$checked 集 .nfo 集号不符"
  fi

  # E5 runtime（分钟）与实测时长相符；未建索引的集允许 0，但必须在记录里注明
  local idx rt_bad=0 rt_checked=0 rt_zero=0
  idx=$(dbq "SELECT count(*) FROM segment WHERE episode_id IN (SELECT id FROM episode WHERE drama_id=$DRAMA_ID)")
  for line in $(season_files "$show" strm | sed 's/ /%20/g'); do
    line=${line//%20/ }
    local id uid rt dur want
    id=$(strm_id "$line")
    [ -z "$id" ] && continue
    uid=${id%%-*}
    want=$(outward_of_uid "$DRAMA_ID" "$uid" | cut -d'|' -f1)
    [ -z "$want" ] && continue
    timeout 60 "$RCLONE" cat "dramahub:/$line" >/dev/null 2>&1 # 保证文件可读
    rt=$(outward_runtime "$DRAMA_ID" "$uid")
    if [ "$rt" = "0" ]; then
      rt_zero=$((rt_zero + 1))
      continue
    fi
    dur=$(timeout 180 "$FFPROBE" -v error -show_entries format=duration -of csv=p=0 "$BASE/video/$id.m3u8" 2>/dev/null | head -1)
    rt_checked=$((rt_checked + 1))
    if [ -z "$dur" ]; then
      rt_bad=$((rt_bad + 1))
      info "" "集 $want 取实测时长失败（runtime=$rt）"
      continue
    fi
    # 按秒比对，容差 1 秒：runtime_sec 是 ffprobe 时长四舍五入到秒（segment/cutter.go
    # 的 math.Round），拿「分钟截断」的结果互比会在 119.7–120.0 这种边界上假红
    # （实测：ep13 duration_sec=120、ffprobe=119.734，两边都取 floor(/60) 得 2 与 1）。
    # 判据是「runtime 与实测时长相符」，所以就在秒上判，不绕分钟。
    if ! python3 -c "import sys;sys.exit(0 if abs(float('$dur')-$rt)<=1 else 1)"; then
      rt_bad=$((rt_bad + 1))
      [ "$rt_bad" -le 3 ] && info "" "集 $want runtime=${rt}s ≠ 实测 ${dur}s"
    fi
  done
  if [ "$rt_bad" -eq 0 ] && [ "$rt_checked" -gt 0 ]; then
    pass E5 "$rt_checked 集 runtime 与 ffprobe 实测一致（另有 $rt_zero 集尚未建索引，runtime=0）"
  elif [ "$rt_checked" -eq 0 ]; then
    skip E5 "runtime 比对：所有集 runtime=0（索引未建立）"
  else
    fail E5 "$rt_bad/$rt_checked 集 runtime 与实测不符（$rt_zero 集未建索引）"
  fi
}

# ------------------------------------------------------------------ F 播放
playlist_ok() { # playlist_ok <id> —— 合法 VOD playlist 打 200 + 时长
  local id=$1
  local code
  code=$(http_code "$BASE/video/$id.m3u8")
  if [ "$code" != "200" ]; then
    echo "code=$code"; return 1
  fi
  timeout 60 curl -sS "$BASE/video/$id.m3u8" >"$WORK/play.m3u8" 2>/dev/null
  grep -q '^#EXTM3U' "$WORK/play.m3u8" || { echo "缺 #EXTM3U"; return 1; }
  grep -q '^#EXT-X-ENDLIST' "$WORK/play.m3u8" || { echo "缺 #EXT-X-ENDLIST"; return 1; }
  local dur
  dur=$(timeout 120 "$FFPROBE" -v error -show_entries format=duration -of csv=p=0 "$BASE/video/$id.m3u8" 2>/dev/null | head -1)
  echo "duration=${dur:-?}"
}

segments_ok() { # segments_ok <id> —— playlist 里每个分片 200、是自包含 fMP4 且真有视频流
  # 「每集一片」（ADR-0011）：分片数 = 成员集数，所以分片清单从 playlist 里取，不写死 3 个。
  local id=$1 bad=0 n=0 uri code file magic
  timeout 60 curl -sS "$BASE/video/$id.m3u8" 2>/dev/null | grep -E '^/video/' >"$WORK/uris.txt"
  if [ ! -s "$WORK/uris.txt" ]; then echo "playlist 里没有分片"; return 1; fi
  while IFS= read -r uri; do
    n=$((n + 1))
    code=$(http_code "$BASE$uri")
    if [ "$code" != "200" ]; then
      echo "$uri code=$code"; return 1
    fi
    file="$WORK/seg$n.m4s"
    timeout 300 curl -sS -o "$file" "$BASE$uri" 2>/dev/null
    # 自包含：片头必须是 ftyp（top-level box 的 type 在偏移 4..7）。
    magic=$(dd if="$file" bs=1 skip=4 count=4 2>/dev/null)
    if [ "$magic" != "ftyp" ]; then
      bad=$((bad + 1))
      continue
    fi
    # 单看一片就能解析出视频流（不依赖前一片，也没有 EXT-X-MAP）。
    if ! timeout 60 "$FFPROBE" -v error -select_streams v:0 -show_entries stream=codec_name -of csv=p=0 "$file" 2>/dev/null | grep -q .; then
      bad=$((bad + 1))
    fi
  done <"$WORK/uris.txt"
  if [ "$bad" -eq 0 ]; then echo "$n/$n 分片是自包含 fMP4 且含视频流"; else echo "$bad/$n 分片有问题"; return 1; fi
}

play_seconds() { # play_seconds <id> —— 真的拉到 N 秒内容：看 ffmpeg 的 out_time，
  # 不是「退出码 0」就够（10 秒的 playlist 也会正常结束，exec 退出码同样是 0）。
  local id=$1
  timeout $((PLAY_SECONDS + 180)) "$FFMPEG" -v error -nostdin -rw_timeout 20000000 \
    -progress pipe:1 -nostats -i "$BASE/video/$id.m3u8" -t "$PLAY_SECONDS" -f null - \
    >"$WORK/play.progress" 2>"$WORK/play.err"
  local rc=$?
  tail -2 "$WORK/play.err" | tr '\n' ' ' >"$WORK/play.tip"
  [ $rc -eq 0 ] || return $rc
  local us
  us=$(grep -oE '^out_time_us=[0-9]+' "$WORK/play.progress" | tail -1 | cut -d= -f2)
  [ -n "$us" ] || us=0
  local got=$((us / 1000000))
  echo "已播放 ${got}s"
  # 允许 0.5 秒的容器/flush 误差
  [ "$(python3 -c "print(1 if $us >= ($PLAY_SECONDS-0.5)*1000000 else 0)")" = "1" ] || return 1
  return 0
}

item_uid_for_ep() { # item_uid_for_ep <ep_no> —— 从清单里找对外集号的 strm id
  local show=$1 want=$2 line id
  for line in $(season_files "$show" strm | sed 's/ /%20/g'); do
    line=${line//%20/ }
    id=$(strm_id "$line")
    if [ -n "$id" ] && db_ready; then
      local ep
      ep=$(outward_of_uid "$DRAMA_ID" "${id%%-*}" | cut -d'|' -f1)
      [ "$ep" = "$want" ] && { echo "$id"; return 0; }
    fi
  done
  return 1
}

# （无 live_warnings：F11 直接读故障注入容器的日志，见 stage_F）

stage_F() {
  section "F 播放（核心判据：完整集、≥${PLAY_SECONDS} 秒）"
  if [ ! -s "$WORK/list.txt" ]; then
    fail F0 "没有目录清单，无法取 .strm"
    return
  fi
  local show
  show=$(find_show_dir)
  [ -z "$show" ] && { fail F0 "找不到剧目录"; return; }

  local id1 out
  id1=$(item_uid_for_ep "$show" 1)
  if [ -z "$id1" ]; then
    fail F1 "取不到第 1 集的 .strm id"
  else
    info "" "第 1 集 id=$id1"
    if out=$(playlist_ok "$id1"); then
      pass F1 "第 1 集 /video/$id1.m3u8 → 200 合法 VOD playlist — $out"
      local dur=${out#duration=}
      if [ "$(python3 -c "print(1 if float('${dur:-0}')>=60 else 0)")" = "1" ]; then
        pass F2 "第 1 集 playlist 时长 ${dur}s ≥ 60s（不是 10 秒预览）"
      else
        fail F2 "第 1 集 playlist 时长 ${dur}s < 60s：拿到的是预览，不是完整片源"
      fi
    else
      fail F1 "第 1 集 playlist — $out"
      fail F2 "第 1 集时长 — 同上（playlist 不可用）"
    fi
    if out=$(segments_ok "$id1"); then
      pass F3 "第 1 集各分片均 200、自包含 fMP4 且有视频流 — $out"
    else
      fail F3 "第 1 集分片 — $out"
    fi
    if play_seconds "$id1"; then
      pass F4 "第 1 集实际播放 ${PLAY_SECONDS}s 不中断"
    else
      fail F4 "第 1 集播放 ${PLAY_SECONDS}s 失败 — $(cat "$WORK/play.tip")"
    fi
  fi

  # 第 4 集及以后（网页无片源，只能来自 App 通道）
  local id4=""
  local n
  n=$(season_files "$show" strm | wc -l)
  if [ "$n" -ge 4 ]; then
    id4=$(item_uid_for_ep "$show" 4)
  fi
  if [ -z "$id4" ]; then
    fail F5 "第 4 集及以后：取不到第 4 集 id，无法验证 App 通道"
  else
    info "" "第 4 集 id=$id4"
    if out=$(playlist_ok "$id4"); then
      pass F5 "第 4 集 /video/$id4.m3u8 → 200 合法 VOD playlist — $out"
      local dur4=${out#duration=}
      if [ "$(python3 -c "print(1 if float('${dur4:-0}')>=60 else 0)")" = "1" ]; then
        pass F6 "第 4 集 playlist 时长 ${dur4}s ≥ 60s"
      else
        fail F6 "第 4 集 playlist 时长 ${dur4}s < 60s"
      fi
      if out=$(segments_ok "$id4"); then
        pass F7 "第 4 集各分片均 200、自包含 fMP4 且有视频流 — $out"
      else
        fail F7 "第 4 集分片 — $out"
      fi
      if play_seconds "$id4"; then
        pass F8 "第 4 集实际播放 ${PLAY_SECONDS}s 不中断（App 通道）"
      else
        fail F8 "第 4 集播放 ${PLAY_SECONDS}s 失败 — $(cat "$WORK/play.tip")"
      fi
      # 时长不许是 10 秒预览
      if [ "$(python3 -c "print(1 if abs(float('${dur4:-0}')-10.07)>1 else 0)")" = "1" ]; then
        pass F9 "第 4 集时长 ${dur4}s ≠ 10.07s 预览"
      else
        fail F9 "第 4 集时长 ${dur4}s 等于预览长度"
      fi
    else
      fail F5 "第 4 集 playlist — $out"
      fail F6 "第 4 集时长 — 同上"
      fail F7 "第 4 集分片 — 同上"
      fail F8 "第 4 集播放 — 同上"
    fi
  fi

  # F10 故障注入：起一个「设备注册域名指向 127.0.0.1」的同镜像临时容器（新进程、
  # 进程内设备身份缓存为空），App 通道必然失败 → 必须 502 + 日志原因，而不是
  # 200 + 10 秒预览。不能改运行中容器的 /etc/hosts：podman/docker 在 restart 时会
  # 重建 /etc/hosts（实测）。
  if [ -z "$DH_CONTAINER" ]; then
    skip F10 "App 通道故障注入：未给 DH_CONTAINER"
  else
    local victim="" vid="" code="" warn=""
    if [ -z "$id4" ]; then
      skip F10 "故障注入：没有可用的第 4 集及以后 id"
    else
      local img port=18081 envs=()
      img=$("$ENGINE" inspect --format '{{.ImageName}}' "$DH_CONTAINER" 2>/dev/null)
      while IFS= read -r e; do [ -n "$e" ] && envs+=(-e "$e"); done < <("$ENGINE" inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$DH_CONTAINER" 2>/dev/null)
      "$ENGINE" rm -f dh625-appfail >/dev/null 2>&1
      info "" "故障注入：临时容器 dh625-appfail（设备注册域名 → 127.0.0.1），试第 4–6 集"
      if timeout 180 "$ENGINE" run -d --name dh625-appfail --network "${DH_NETWORK:-dh625}" \
        -p "$port:8080" --add-host log5-applog-lf.fqnovel.com:127.0.0.1 \
        "${envs[@]}" "$img" >/dev/null 2>&1; then
        for _ in $(seq 1 30); do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 1; done
        # 网页通道有时先回 ErrSchema（拿不到 VideoObject），那种集不会走到 App；
        # 逐个试第 4–6 集，取第一个「502 且日志里确实有 App 通道失败」的。
        local cand vid_of_cand
        for cand in 4 5 6; do
          victim=$(item_uid_for_ep "$show" "$cand") || victim=""
          [ -z "$victim" ] && continue
          vid_of_cand=$(dbq "SELECT vid FROM episode WHERE drama_id=$DRAMA_ID AND ep_no=$cand")
          code=$(http_code "http://127.0.0.1:$port/video/$victim.m3u8")
          warn=$("$ENGINE" logs dh625-appfail 2>&1 | grep -F "$vid_of_cand" | grep -E 'App 通道取址失败' | tail -1)
          [ "$code" = "502" ] && [ -n "$warn" ] && { vid=$vid_of_cand; break; }
        done
        if [ "$code" = "502" ]; then
          pass F10 "App 通道不可用时 /video（第 $cand 集 id=$victim）→ 502（不是 200 + 预览）"
          if [ -n "$warn" ]; then
            pass F11 "日志有一条 WarnContext 说明原因 — $(printf '%s' "$warn" | head -c 220)"
          else
            fail F11 "日志里看不到 App 通道失败原因（第 4–6 集都没有 App 失败日志）"
          fi
        else
          fail F10 "App 通道不可用时 /video 期望 502，实得 $code（200 说明回落了预览）"
          fail F11 "日志原因 — 与 F10 同因"
        fi
        "$ENGINE" rm -f dh625-appfail >/dev/null 2>&1
      else
        skip F10 "故障注入容器启动失败（看 $ENGINE run 的输出）"
        skip F11 "故障注入容器启动失败"
      fi
    fi
  fi
}

# ------------------------------------------------------------------ G 合并集
# 判据（ADR-0010）：分块是自动的，不需要也不允许人工建组。
#   G1 对外集数 = ceil(集数 / N)
#   G2 每块 ep_no 连续、uid 形态正确（single e<id> / merged m<drama>_<a>_<b>）
#   G3 合并块的 playlist 集边界有 DISCONTINUITY、seq 从 0 连续
#   G4 合并块 playlist 的总时长 ≈ 块内成员 duration_sec 之和（误差 ≤2s）
#   G5 改 N 后**不同步任何剧**：对外集数立刻变 ceil(集数 / 新 N)（ADR-0014）
#   G6 旧合并 id 的 .m3u8 变 404（签名仍合法，不返回别人的内容）
stage_G() {
  section "G 合并集（自动分块 + 虚拟 HLS 聚合）"
  if [ -z "$DRAMA_ID" ] || ! db_ready; then
    skip G1 "合并集：需要 C 段同步成功与 PG_CONTAINER"
    return
  fi

  local eps chunk
  eps=$(dbq "SELECT count(*) FROM episode WHERE drama_id=$DRAMA_ID")
  chunk=$(dbq "SELECT value_json FROM settings WHERE key='merge_chunk_size'" | tr -d '"')
  [ -z "$chunk" ] && chunk=10
  if ! printf '%s' "$eps" | grep -qE '^[0-9]+$' || ! printf '%s' "$chunk" | grep -qE '^[0-9]+$'; then
    skip G1 "取不到集数或 merge_chunk_size（DRAMA_ID=$DRAMA_ID）"
    return
  fi
  if [ "$eps" -eq 0 ]; then
    skip G1 "这部剧 0 集：分块没有可判定的对象"
    return
  fi
  info "" "剧 id=$DRAMA_ID 集数=$eps N=$chunk"

  # G1 对外集数 = ceil(集数 / N)
  local want_items got_items
  want_items=$(( (eps + chunk - 1) / chunk ))
  got_items=$(outward_items "$DRAMA_ID" | wc -l | tr -d ' ')
  if [ "$got_items" = "$want_items" ]; then
    pass G1 "对外集数 = ceil($eps/$chunk) = $want_items（现算与目录条目数一致）"
  else
    fail G1 "对外集数 = $got_items, 期望 ceil($eps/$chunk) = $want_items"
  fi

  # G2 块号连续、kind 与 uid 形态一致
  local bad_no=0 bad_uid=0 expected_no=0 ep_no kind uid
  while IFS='|' read -r ep_no kind uid; do
    [ -z "$ep_no" ] && continue
    expected_no=$((expected_no + 1))
    [ "$ep_no" = "$expected_no" ] || bad_no=$((bad_no + 1))
    case "$kind" in
      single) printf '%s' "$uid" | grep -qE '^e[0-9]+$' || bad_uid=$((bad_uid + 1)) ;;
      merged) printf '%s' "$uid" | grep -qE "^m${DRAMA_ID}_[0-9]+_[0-9]+$" || bad_uid=$((bad_uid + 1)) ;;
      *) bad_uid=$((bad_uid + 1)) ;;
    esac
  done <<EOF
$(outward_items "$DRAMA_ID")
EOF
  if [ "$bad_no" -eq 0 ] && [ "$bad_uid" -eq 0 ]; then
    pass G2 "$got_items 块：对外集号 1..$got_items 连续，uid 形态全部正确"
  else
    fail G2 "块号不连续 $bad_no 条、uid 形态错误 $bad_uid 条"
  fi

  # 找一个合并块（块内 ≥2 集）：G3/G4 只对它有意义。N=1 或集数不足时跳过。
  local muid=""
  muid=$(outward_items "$DRAMA_ID" | awk -F'|' '$2=="merged" { print $3; exit }')
  if [ -z "$muid" ]; then
    skip G3 "没有合并块（N=1 或集数 ≤1），无法验 DISCONTINUITY"
    skip G4 "没有合并块，无法验总时长"
  else
    local mid from to wantdur m_ep_no
    m_ep_no=$(outward_of_uid "$DRAMA_ID" "$muid" | cut -d'|' -f1)
    refresh_list
    mid=$(item_uid_for_ep "$(find_show_dir)" "$m_ep_no")
    from=$(printf '%s' "$muid" | cut -d_ -f2)
    to=$(printf '%s' "$muid" | cut -d_ -f3)
    if [ -z "$mid" ]; then
      fail G3 "清单里找不到合并块 $muid 的 .strm（现算出该块但目录没渲染出来？）"
      fail G4 "合并块 $muid 的 playlist 取不到 id"
    else
      local code body
      code=$(http_code "$BASE/video/$mid.m3u8")
      if [ "$code" != "200" ]; then
        fail G3 "合并块 playlist 期望 200，实得 $code"
        fail G4 "合并块 playlist 不可用"
      else
        body=$(timeout 60 curl -sS "$BASE/video/$mid.m3u8" 2>/dev/null)
        local want_disc got_disc
        # 成员数 - 1（集号可能缺号，不能用 to - from 当边界数）
        want_disc=$(dbq "SELECT count(*) - 1 FROM episode WHERE drama_id=$DRAMA_ID AND ep_no BETWEEN $from AND $to")
        got_disc=$(printf '%s\n' "$body" | grep -c '#EXT-X-DISCONTINUITY')
        if [ "$got_disc" = "$want_disc" ]; then
          pass G3 "合并块 第${from}-${to}集：DISCONTINUITY $got_disc 处（= 成员集数 - 1）"
        else
          fail G3 "合并块 第${from}-${to}集：DISCONTINUITY $got_disc 处，期望 $want_disc"
        fi
        local seqs
        seqs=$(printf '%s\n' "$body" | grep -oE '/video/[^/]+/[0-9]+\.m4s' | sed -E 's#.*/([0-9]+)\.m4s$#\1#' | tr '\n' ' ')
        if [ -n "$seqs" ] && [ "$(printf '%s' "$seqs" | awk '{for(i=1;i<=NF;i++) if($i!=i-1) bad=1; END{print bad+0}')" = "0" ]; then
          info "" "分片 seq 从 0 连续：$(echo $seqs | awk '{print $1".."$NF}')"
        else
          fail G3 "分片 seq 不连续 — $seqs"
        fi

        wantdur=$(dbq "SELECT coalesce(sum(duration_sec),0) FROM episode WHERE drama_id=$DRAMA_ID AND ep_no BETWEEN $from AND $to")
        local gotdur
        gotdur=$(timeout 180 "$FFPROBE" -v error -show_entries format=duration -of csv=p=0 "$BASE/video/$mid.m3u8" 2>/dev/null | head -1)
        if [ -n "$gotdur" ] && [ -n "$wantdur" ] && python3 -c "import sys;sys.exit(0 if abs(float('$gotdur')-float('$wantdur'))<=2 else 1)" 2>/dev/null; then
          pass G4 "合并块总时长 ${gotdur}s ≈ Σ成员 ${wantdur}s（误差 ≤2s）"
        else
          # 成员集从未播放过时 duration_sec 仍为 0（首播回填），这种情况不能判失败。
          if [ "$wantdur" = "0" ]; then
            skip G4 "成员集 duration_sec 全为 0（还没首播回填），无法比对总时长"
          else
            fail G4 "合并块总时长 ${gotdur:-?}s ≠ Σ成员 ${wantdur}s"
          fi
        fi
        # G7：整条 playlist 交给 ffmpeg 系播放端不能丢帧。集边界的 PTS 回退会让解码器
        # 丢帧，而「ΣEXTINF ≈ Σ时长」这条判据看不出来（包全读到、帧被丢）。
        local counts nframes npackets
        counts=$(timeout 240 "$FFPROBE" -v error -select_streams v:0 -count_frames -count_packets \
          -show_entries stream=nb_read_frames,nb_read_packets -of csv=p=0 "$BASE/video/$mid.m3u8" 2>/dev/null | head -1)
        nframes=${counts%%,*}
        npackets=${counts##*,}
        if [ -n "$nframes" ] && [ "$nframes" = "$npackets" ]; then
          pass G7 "合并块整条 playlist 解码帧数 = 包数 = $nframes（集边界时间戳单调，不丢帧）"
        else
          fail G7 "合并块整条 playlist frames=$nframes ≠ packets=$npackets（集边界 PTS 回退，ffmpeg 系播放端会丢画面）"
        fi
      fi
    fi
  fi

  # G5/G6 改 N 生效（ADR-0014）：改完**不触发任何同步**，目录与 playlist 立刻是新分块。
  # 这是本段最重要的一条 —— 以前要等下一次重建，现在不需要。
  if [ "$MERGE_CHUNK_ALT" = "$chunk" ]; then
    skip G5 "MERGE_CHUNK_ALT=$MERGE_CHUNK_ALT 与原 N 相同，改 N 不可判定"
    skip G6 "同 G5"
    return
  fi
  local want_alt got_alt old_code old_id="" dir_files_alt
  if [ -n "$muid" ]; then
    old_id=$(strm_id_for_uid "$muid")
  fi
  local set_code
  set_code=$(api PATCH "/api/settings" "{\"settings\":{\"merge_chunk_size\":\"$MERGE_CHUNK_ALT\"}}")
  if [ "$set_code" != "200" ]; then
    fail G5 "PATCH merge_chunk_size=$MERGE_CHUNK_ALT 期望 200，实得 $set_code — $(head -c 200 "$WORK/api.body")"
    fail G6 "改 N 失败，G6 无法判定"
    return
  fi
  # 不同步、不重建、不重启：只重列目录。
  refresh_list
  want_alt=$(( (eps + MERGE_CHUNK_ALT - 1) / MERGE_CHUNK_ALT ))
  got_alt=$(outward_items "$DRAMA_ID" | wc -l | tr -d ' ')
  dir_files_alt=$(grep -cF "$(find_show_dir)/Season 01/" "$WORK/list.txt" 2>/dev/null || echo 0)
  if [ "$got_alt" = "$want_alt" ] && [ "$dir_files_alt" = "$((2 * want_alt))" ]; then
    pass G5 "N=$MERGE_CHUNK_ALT 未做任何同步：对外集数 = ceil($eps/$MERGE_CHUNK_ALT) = $want_alt，Season 01 立刻是 $dir_files_alt 个文件"
  else
    fail G5 "N=$MERGE_CHUNK_ALT 未做任何同步：对外集数 $got_alt（期望 $want_alt）、Season 01 文件 $dir_files_alt（期望 $((2 * want_alt))）"
  fi
  # 旧 uid 已不在派生表里 → 旧 id 走 404（签名仍合法，不能变 401、不能返回别人的内容）
  if [ -n "${old_id:-}" ]; then
    old_code=$(http_code "$BASE/video/$old_id.m3u8")
    if [ "$old_code" = "404" ]; then
      pass G6 "改 N 后旧合并 id 的 playlist → 404"
    else
      fail G6 "改 N 后旧合并 id 期望 404，实得 $old_code"
    fi
  else
    skip G6 "没有可复用的旧合并 id（原 N 下没有合并块）"
  fi

  # 改回原 N（同样不同步），避免影响后续段与重跑。
  api PATCH "/api/settings" "{\"settings\":{\"merge_chunk_size\":\"$chunk\"}}" >/dev/null
  refresh_list
  local restored
  restored=$(outward_items "$DRAMA_ID" | wc -l | tr -d ' ')
  if [ "$restored" = "$want_items" ]; then
    info "" "已把 merge_chunk_size 改回 $chunk（对外集数 $restored）"
  else
    info "" "警告：改回 N=$chunk 后对外集数 = $restored，期望 $want_items（看板与后续段可能受影响）"
  fi
}

# ------------------------------------------------------------------ H 持久
stage_H() {
  section "H 持久（重启后数据、缓存、同一 id 仍可播）"
  if [ -z "$DH_CONTAINER" ]; then
    skip H1 "重启验证：未给 DH_CONTAINER"
    return
  fi
  local show id1 before after_before after_after
  refresh_list
  show=$(find_show_dir)
  id1=$(item_uid_for_ep "$show" 1) || id1=""
  if [ -z "$id1" ]; then
    skip H1 "取不到第 1 集 id"
    return
  fi
  before=$id1
  if [ -n "$CACHE_HOST_DIR" ]; then
    after_before=$(du -sm "$CACHE_HOST_DIR" 2>/dev/null | awk '{print $1}')
  else
    after_before=$("$ENGINE" exec "$DH_CONTAINER" sh -c 'du -sm /data/cache 2>/dev/null' | awk '{print $1}')
  fi
  "$ENGINE" restart "$DH_CONTAINER" >/dev/null 2>&1
  for _ in $(seq 1 30); do curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break; sleep 1; done
  if [ -n "$CACHE_HOST_DIR" ]; then
    after_after=$(du -sm "$CACHE_HOST_DIR" 2>/dev/null | awk '{print $1}')
  else
    after_after=$("$ENGINE" exec "$DH_CONTAINER" sh -c 'du -sm /data/cache 2>/dev/null' | awk '{print $1}')
  fi
  local after
  after=$(item_uid_for_ep "$(find_show_dir)" 1) || after=""
  # 判据是「重启后数据与缓存都在、同一 id 仍可播」。播不出来要区分「持久性坏了」
  # 与「上游当下不可用」：后者不是 H 的判据，但也不许当通过——所以仍旧判红，
  # 只是把「id 变没变 / 缓存还在不在 / 上游为什么失败」分开写清（实测碰到过
  # App 端点连续 25 分钟只回 200+空体，重启前后数据缓存全对但播不出来）。
  local code
  code=$(http_code "$BASE/video/$after.m3u8")
  if [ "$before" != "$after" ] || [ -z "$after" ]; then
    fail H1 "重启后同一集 id 变了：前 $before / 后 $after"
    return
  fi
  if [ "$code" = "200" ]; then
    pass H1 "重启前后同一集 id 不变（$before）且 playlist 200；缓存 ${after_before:-?}MB → ${after_after:-?}MB"
    return
  fi
  # 数据与缓存仍在、id 也没变，只是播不出来：把失败原因摊开，交给 F/上游判。
  local why
  why=$("$ENGINE" logs --since 5m "$DH_CONTAINER" 2>&1 |
    grep -E '"msg":"(App 通道取址失败|App 取流|上游请求失败)"' | tail -1 | head -c 200)
  fail H1 "重启后同一集 id 未变（$before）、缓存 ${after_before:-?}MB → ${after_after:-?}MB，但 playlist=$code — $why"
}

# ------------------------------------------------------------------ I 换链
stage_I() {
  section "I 换链（直链失效后重新解析仍出流）"
  if [ -z "$DH_CONTAINER" ]; then
    skip I1 "换链验证：未给 DH_CONTAINER"
    return
  fi
  local show id1
  refresh_list
  show=$(find_show_dir)
  id1=$(item_uid_for_ep "$show" 1) || id1=""
  [ -z "$id1" ] && { skip I1 "取不到第 1 集 id"; return; }
  # 直链不落库（PLAYBACK-CONTRACT）：每次请求都现解析。清掉该集分片缓存后重取，
  # 若仍能出流，说明走了「重新解析 + 重新切片」，没有沿用任何存量链接或字节。
  local before after
  before=$("$ENGINE" exec "$DH_CONTAINER" sh -c 'find /data/cache/seg -type f 2>/dev/null | wc -l' | tr -d ' ')
  "$ENGINE" exec -u 0 "$DH_CONTAINER" sh -c 'rm -f /data/cache/seg/* 2>/dev/null' >/dev/null 2>&1
  local code
  code=$(http_code "$BASE/video/$id1/0.m4s")
  after=$("$ENGINE" exec "$DH_CONTAINER" sh -c 'find /data/cache/seg -type f 2>/dev/null | wc -l' | tr -d ' ')
  if [ "$code" = "200" ] && [ "$after" -gt 0 ]; then
    pass I1 "清空分片缓存后重新取分片 → 200 且缓存重新落盘（缓存文件 $before → $after）；直链不落库，每次请求现解析"
  else
    fail I1 "清空分片缓存后重取分片失败：code=$code 缓存文件 $before → $after"
  fi
}

# ------------------------------------------------------------------ 主流程
STAGES=("$@")
[ ${#STAGES[@]} -eq 0 ] && STAGES=(A B C D E F G H I)

need_bin curl python3
for s in "${STAGES[@]}"; do
  case "$s" in
    A | B | C | D | E)
      :
      ;;
    F | G | H | I)
      need_bin "$FFMPEG" "$FFPROBE" "$RCLONE"
      ;;
  esac
done

echo "base=$BASE engine=$ENGINE dh=${DH_CONTAINER:-<none>} pg=${PG_CONTAINER:-<none>} series=${SERIES_ID:-<none>}"
for s in "${STAGES[@]}"; do
  "stage_$s"
done

printf '\n== 汇总 ==\nPASS %d  FAIL %d  SKIP %d\n' "$PASSED" "$FAILED" "$SKIPPED"
[ "$FAILED" -eq 0 ] || exit 1
