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

## 3. 首次启动后立刻做两件事

### 3.1 先关掉自动调度（只验收一部剧时）

**开箱的调度器会在启动后立刻跑一轮：枚举 sitemap（≈30 请求）+ 把 60 万部新剧逐个灌库。**
首次全量约 30 小时，而且累计约 1 万次 `/player` 之后上游会对 `/player` 全量回 `444`
（限流，约十几分钟后恢复，见 `docs/SCHEDULER-MODEL.md`）。

只想同步一两部剧来验收时，先把它关掉：

```bash
curl -sS -X PATCH -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"settings":{"plugin.web.enabled":"false","refresh_batch":"0"}}' \
  http://127.0.0.1:8080/api/settings
# 字符串 "false" 是接口约定：settings 值一律按字符串写，响应里回成布尔 false
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

## 已知问题（本基线）

本基线（`main @ de843f6`）的 A–E、H、I 通过、**F/G 未通过**：
`/video` 会把 10 秒预览当完整片源交付（ep1），App 通道取到的完整片源因出站
`Referer` 被媒体 CDN 403。逐项证据与打回记录见 MKACG-625。修好后按第 7 节从 A 重跑。
