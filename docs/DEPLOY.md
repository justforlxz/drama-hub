# DEPLOY — 从零到能播的最短路径

这份文档只讲一件事：**拿到镜像之后，用最少几步把它跑起来、同步一部剧、让 `/video` 出流、
被 rclone / Emby 读到**。每一步都能照抄执行。

验收口径与逐项结论见 MKACG-625；自动化判定用 `scripts/acceptance.sh`（见文末）。

## 前置

- `podman`（或 `docker`）可用的 Linux 主机。
- PostgreSQL 16（容器即可；本服务**不建库**，只连库并自动迁移）。
- 出站能到 `hongguoduanju.com`（元数据）与 `*.fqnovel.com` / `*.qznovelvod.com`
  （App 通道取完整片源）。
- 宿主机上用于验收的工具：`curl`、`rclone`、`ffmpeg`、`ffprobe`、`python3`。
  服务自己的 `ffmpeg` 在镜像里，不需要宿主机装。

## 1. 环境变量（只给这四个）

| 变量 | 必填 | 说明 |
|---|---|---|
| `DRAMA_HUB_DB_URL` | **是** | `postgres://user:pass@host:5432/db?sslmode=disable`。没给 = 启动直接失败（ADR-0009） |
| `DRAMA_HUB_STREAM_SECRET` | **是** | ≥ 32 字节。空卷首次启动写进 `settings.stream_secret`；之后重复启动不覆盖库里已有值 |
| `DRAMA_HUB_ADMIN_TOKEN` | **是** | 写操作令牌（`POST`/`PATCH`/`DELETE` 必带 `X-Admin-Token`）。不设 = 所有写操作 401 |
| `DRAMA_HUB_CACHE_DIR` | 建议 | 容器内缓存根目录，镜像默认 `/data/cache`（挂 `/data` 卷即持久） |

可选（进程自身）：`DRAMA_HUB_ADDR`（默认 `:8080`）、`DRAMA_HUB_LOG_LEVEL`（默认 `info`）、
`DRAMA_HUB_DB_MAX_OPEN_CONNS` / `DRAMA_HUB_DB_MAX_IDLE_CONNS` / `DRAMA_HUB_DB_CONN_MAX_LIFETIME_SEC`。

拼错的 `DRAMA_HUB_*` 会被忽略并打一条 warn；启动日志里能看见支持的键。

## 2. 起服务

```bash
podman pull ghcr.io/justforlxz/drama-hub:latest     # 私有包需先 podman login ghcr.io
podman network create drama-hub
mkdir -p pgdata data
podman run -d --name drama-hub-pg --network drama-hub \
  -e POSTGRES_USER=drama -e POSTGRES_PASSWORD=drama -e POSTGRES_DB=drama \
  -v "$PWD/pgdata:/var/lib/postgresql/data:Z" \
  docker.io/library/postgres:16 -c fsync=off
until podman exec drama-hub-pg pg_isready -U drama -d drama; do sleep 1; done

podman run -d --name drama-hub --network drama-hub -p 8080:8080 \
  --userns=keep-id --security-opt label=disable -v "$PWD/data:/data:Z" \
  -e DRAMA_HUB_DB_URL='postgres://drama:drama@drama-hub-pg:5432/drama?sslmode=disable' \
  -e DRAMA_HUB_STREAM_SECRET="$(head -c 48 /dev/urandom | base64 | tr -d '\n')" \
  -e DRAMA_HUB_ADMIN_TOKEN="$(head -c 24 /dev/urandom | base64 | tr -d '\n')" \
  -e DRAMA_HUB_CACHE_DIR=/data/cache \
  ghcr.io/justforlxz/drama-hub:latest
curl -sS localhost:8080/healthz          # {"status":"ok"}
podman logs drama-hub | head -3          # version / commit = 实际构建值
```

`--userns=keep-id` 不能省：rootless podman 下容器内 uid 1000 会映射到 subuid，
写不进属主是宿主用户的 `./data`（实测 `permission denied`）。

## 3. 启动后立刻要处理的两件事

### 3.1 关于自动调度：第一轮无法用 API 拦下

**开箱的调度器在进程起来约 0.15 秒内就派出第一轮**（`source_state` 为空 → `next_due = now`），
本轮实测时间线：启动 `02:36:17.97` → 首个 sitemap 请求 `02:36:18.11` → 首个 `/player`
`02:36:49.09`。所以「启动之后先 PATCH 关掉调度」**拦不住这一轮**：PATCH 只能阻止*下一轮*派发，
已经在跑的那一轮按 `refresh_timeout_sec`（默认 900 秒）跑完才收尾。实测：PATCH 在启动后约
3 分钟生效（`/api/status` 里 `enabled:false, running:true`），本轮仍把库灌到 **3528 部剧 /
3535 次 `/player`** 才于 `02:51:18` 结束。

只想验收一两部剧，有两条可用的路：

**A. 首启前把调度键预置进库**（干净、可重复；本轮验收用的就是这条）

```bash
# 库刚建好、服务还没起时执行一次
podman exec -i <pg容器> psql -U drama -d drama <<'SQL'
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY, value_json TEXT NOT NULL, updated_at TEXT NOT NULL);
INSERT INTO settings(key,value_json,updated_at) VALUES
  ('plugin.web.enabled','false','2026-01-01T00:00:00Z'),
  ('refresh_batch','0','2026-01-01T00:00:00Z')
ON CONFLICT (key) DO UPDATE SET value_json=EXCLUDED.value_json;
SQL
```

设置表与 `internal/store/settings.go` 的 `settingsDDL` 逐字一致，服务起来后迁移会
`CREATE TABLE IF NOT EXISTS` 幂等共存，不会覆盖这两行。之后启动即 `enabled:false`，
`/api/status` 里 `running:false`，库保持为空。

**B. 让它跑完一轮再清库**（不想动库结构时）

第一轮受 `refresh_timeout_sec` 约束（默认 900 秒），跑完后不会再派新轮（已停用）。
等 `/api/status` 的 `running` 变 `false`，再按需清库：

```bash
podman exec -i <pg容器> psql -U drama -d drama -c \
  'TRUNCATE drama, episode, emby_item, merge_group, merge_item, segment RESTART IDENTITY CASCADE;'
```

跑起来之后（第二轮起）关调度就是普通的 PATCH，立即生效：

```bash
curl -sS -X PATCH -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"settings":{"plugin.web.enabled":"false","refresh_batch":"0"}}' \
  http://127.0.0.1:8080/api/settings
# 字符串 "false" 是接口约定：settings 值一律按字符串写，响应里回成布尔 false
# 生效范围是「下一轮派发」；正在跑的一轮不会被中断（会跑到 refresh_timeout_sec 收尾）
```

`plugin.web.enabled=false` **只管调度**：手动同步（`POST /api/dramas`）与播放（`/video`）
仍然可用。想让调度器正常工作就不要关，交给它按 `refresh_interval_sec` 一轮轮跑。

### 3.2 写 WebDAV 账号密码

WebDAV 凭据**不走 env**（它们保护的就是「读元数据」这一面，写进 settings 才能热改）：

```bash
curl -sS -X PATCH -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"settings":{"webdav_user":"emby","webdav_pass":"<密码>"}}' \
  http://127.0.0.1:8080/api/settings
```

写完立刻生效，不用重启（PROPFIND 旧凭据立刻 401）。
`strm_prefix` 留空即可：`.strm` 里的地址按访问的 `Host` 推导成绝对 URL；
若服务在反代后面，写 `{"strm_prefix":"http://<对外地址>:8080"}`。

## 4. 同步一部剧

```bash
SID=7503745441910017049        # 上游剧 id：/player/<id> 里的那串数字
curl -sS -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"series_id\":\"$SID\"}" http://127.0.0.1:8080/api/dramas
# 首次 201 Created；同一部剧再发一次只更新 → 200 OK
curl -sS http://127.0.0.1:8080/api/dramas | python3 -m json.tool | head -20
```

`episode_total` 是上游声明的集数，`episodes` 是库里落库的集数，两者应相等。

## 5. 接到 rclone / Emby

```bash
rclone config create dramahub webdav url http://127.0.0.1:8080/webdav vendor other \
  user emby pass '<密码>'
rclone lsf --max-depth 5 dramahub:/       # <分类>/<分片>/<剧>/Season 01/{.strm,.nfo}
rclone lsf --files-only -R dramahub:/ | wc -l    # = emby_item×2 + 每剧 1 个 tvshow.nfo
rclone mount dramahub:/ /mnt/dramahub --dir-cache-time 30s
```

Emby 库路径指到 `/mnt/dramahub`，类型「电视节目」。
`PROPFIND` 只支持 `Depth: 0/1`（数值深度 ≥2 由 `x/net/webdav` 回 400），rclone 与 Emby
本来都是逐层扫，不是缺陷。

## 6. 播放面自检

```bash
# 取任意一条 .strm 里的 URL
STRM=$(rclone cat "dramahub:/<分类>/<分片>/<剧>/Season 01/<剧> - S01E01 - 第1集.strm")
curl -sS "$STRM" | head -12         # #EXTM3U … #EXT-X-ENDLIST；单集就是一条 playlist
ID=$(echo "$STRM" | sed -E 's#.*/video/([^/]+)\.m3u8#\1#')
curl -sS -o /tmp/0.ts -w '%{http_code}\n' "http://127.0.0.1:8080/video/$ID/0.ts"
ffprobe -v error -show_streams /tmp/0.ts | head -20
ffmpeg -v error -i "$STRM" -t 60 -f null - && echo "播了 60 秒"
```

**判「完整片源 vs 10 秒预览」看时长，不看状态码**：官网 SSR 对受保护剧只给
`10.07 秒 / 360p` 的 SEO 预览，完整片源只能从 App 通道来。playlist 里
`#EXTINF` 加起来 ≈10 秒就是拿到了预览（不应发生）；正常单集是几十秒到几分钟。

## 7. 跑验收脚本

`scripts/acceptance.sh` 是 A–I 的机器判定，逐项输出 `PASS/FAIL/SKIP`：

```bash
BASE=http://127.0.0.1:8080 \
ADMIN_TOKEN="$TOKEN" \
WEBDAV_USER=emby WEBDAV_PASS='<密码>' OLD_WEBDAV_PASS='<上一个密码，可选>' \
SERIES_ID=7503745441910017049 \
PG_CONTAINER=drama-hub-pg DH_CONTAINER=drama-hub \
bash scripts/acceptance.sh              # 或只跑某段：bash scripts/acceptance.sh F G
```

- `SKIP` 表示「当前条件下无法判定」（没给容器名、上游不可达、无法观测兜底分类），
  **不是通过**。
- 任何 `FAIL` 让脚本退出码为 1；`FAIL` 就是判据不满足，不要改判据让它变绿。
- F 段默认实播 60 秒（`PLAY_SECONDS`），并用 ffmpeg 的 `out_time` 证明**真的拉了 60 秒内容**
  ——只看退出码 0 不行：10 秒的 playlist 也会正常结束。

## 常见失败

| 现象 | 原因 / 处置 |
|---|---|
| 启动即退出，日志 `数据库不可用` | `DRAMA_HUB_DB_URL` 没给/连不上；库必须先存在（服务不建库）。容器里访问宿主库用容器网络里的服务名，不要写 `127.0.0.1` |
| `/api/*` 写操作 401 | 没带 `X-Admin-Token`，或没设 `DRAMA_HUB_ADMIN_TOKEN`（fail closed） |
| `PROPFIND` 401 | `webdav_user/webdav_pass` 还没写（第 3.2 步），或凭据错 |
| 缓存目录 `permission denied` | 忘了 `--userns=keep-id` |
| 同步/播放日志里大量 `upstream_status:444` | 上游限流（累计约 1 万次 `/player` 后触发），等十几分钟；不要写重试循环 |
| `/video/{id}.m3u8` 502 | 该集解析不到片源：受保护剧的网页只有预览，完整片源走 App 通道；App 被拒/不可用时按契约回 502（日志一条 `WarnContext` 说明 `uid`/`ep_no`/`channel`/`reason`） |
| `/video/{id}.m3u8` 503 + `Retry-After` | 首次播放要建关键帧索引，30 秒硬上限；稍后重试 |
| URL 变了 / 换了 `stream_secret` 后全部 401 | 出流 id 是 HMAC 签名（ADR-0003）：换密钥等于所有旧 URL 失效 |
| 日志出现 `package_manager` 之外的「未知的 DRAMA_HUB_* 环境变量」 | 变量名拼错，值被忽略 |

## 已知问题（本基线 `main @ ab51419`）

上一版基线（`de843f6`）的 F/G 红已在 637/638/639/640 修掉；本基线全量重跑 A–I 为
**37 PASS / 0 FAIL / 1 SKIP**（SKIP 是 D5：本轮同步的剧 `genre` 非空，观测不到兜底目录；
另用一条 `genre=''` 的剧手工确认过落到 `其他/`）。逐项证据见 MKACG-625。

三处**运维事项，不是判据**，但会让人误判成败：

| 现象 | 事实 |
|---|---|
| 首启后 PATCH 关调度，库还在涨 | 第一轮在启动后约 0.15 秒已派出，PATCH 只拦下一轮；本轮会跑到 `refresh_timeout_sec`（默认 900 秒）。见 3.1 |
| `rclone lsf -R` 文件数 ≠ `emby_item×2 + 剧数` | 若首轮仍在灌库，清单与库是两个时刻的快照。等 `/api/status` 的 `running=false` 再比 |
| App 通道连续几分钟全是 `/video` 502，日志 `App 取流 … "bytes":0` + `reason:"ErrSchema"` | 上游端点 `200 + 空响应体`。实测连续 25 分钟后自行恢复（同期同一 vid 之前是 18 KB 正常体）。按契约回 502 且不回落预览是正确行为；重试即可，不要当代码缺陷 |

`docs/SCHEDULER-MODEL.md` 把「首启立刻跑一轮」写成既定行为；本文件的 3.1 只讲怎么在验收时
绕开它，不改调度语义。
