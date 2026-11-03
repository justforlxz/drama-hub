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
  'TRUNCATE drama, episode, segment RESTART IDENTITY CASCADE;'
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
rclone lsf --max-depth 4 dramahub:/       # <分类>/<剧>/Season 01/{.strm,.nfo}
rclone lsf --files-only -R dramahub:/ | wc -l    # = 对外集数×2 + 每剧 1 个 tvshow.nfo
rclone mount dramahub:/ /mnt/dramahub --dir-cache-time 30s
```

Emby / Jellyfin 的库**必须按分类目录分挂**：一个「电视节目」库，媒体文件夹加
`/mnt/dramahub/<分类>`（每个分类一个，真实 `genre` 词表 68 个）。库根直接挂
`/mnt/dramahub` 会把分类目录当 Series（1436 部剧只识别出 59 部），见
`docs/DIRECTORY-CONTRACT.md`「被推翻的做法」。

`PROPFIND` 的 `Depth` 只认 `0` / `1` / `infinity`（数值深度 ≥2 由 `x/net/webdav` 回 400），
rclone 与 Emby 本来都是逐层 `Depth: 1` 扫，不是缺陷。**注意省略 `Depth` 头等于
`infinity`（递归整棵树）**：21.6 万剧的分类目录实测 369 MB / 116 s / 服务端 RSS +501 MB，
所以手工 `curl` 排障必须显式带 `-H 'Depth: 1'`（`docs/DIRECTORY-CONTRACT.md`「单目录子项上限」）。

## 5b. 接 Jellyfin：一分类一媒体库（ADR-0012）

Jellyfin 只把**库根的直接子目录**当剧集。库根挂在挂载根上时，分类目录会变成 Series、
剧目录退化成普通目录（本部署实测：59 个分类成了 59 部 Series、1436 个剧目录失效）。
所以库根必须收窄到 `<挂载根>/<分类>`，由本服务一个分类建一个库。

三步，缺一步库就是空的：

1. **网络可达**：本服务容器要能连到 Jellyfin 的 HTTP 面。两者不在同一个 docker 网络时
   `jellyfin_url` 只能填两者都可达的地址（本部署实测 `http://jellyfin:8096` 在
   drama-hub 容器内解析失败，`https://jellyfin.<域名>` 可用）；要么把 drama-hub 接进
   Jellyfin 所在的网络。
2. **建 API key**：Jellyfin 控制台 → 管理 → API 密钥 → 新建。12.1 只认
   `Authorization: MediaBrowser Token="…"`（`X-Emby-Token` 已停用），本服务自动用它。
3. **库根填 Jellyfin 容器内的路径**（看挂载，不看宿主）：本部署是
   `/data/video/dramahub`（宿主 `/vol1/1000/Media/dramahub`）。填成宿主路径或本服务的
   挂载点时，Jellyfin 建出来的库是空的。

```bash
TOKEN=<DRAMA_HUB_ADMIN_TOKEN>
curl -sS -X PATCH -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' -d '{
  "settings": {
    "jellyfin_url": "https://jellyfin.example.com",
    "jellyfin_api_key": "<Jellyfin API key>",
    "jellyfin_library_root": "/data/video/dramahub"
  }}' http://127.0.0.1:8080/api/settings

# 手动跑一轮（同步轮末也会自动跑一次，幂等）
curl -sS -o /dev/null -w '%{http_code}\n' -X POST -H "X-Admin-Token: $TOKEN" \
  http://127.0.0.1:8080/api/jellyfin/libraries      # 202 已派出 / 409 上一轮还在跑 / 400 没配齐

# 结果：库数、上次新建、孤儿库、错误原因
curl -sS http://127.0.0.1:8080/api/status | python3 -c \
  'import json,sys; print(json.dumps(json.load(sys.stdin)["jellyfin"], ensure_ascii=False, indent=2))'

# Jellyfin 侧直接核对（不需要 scanner 跑完）
curl -sS -H "Authorization: MediaBrowser Token=\"<Jellyfin API key>\"" \
  'http://<jellyfin>/Library/VirtualFolders' | python3 -c \
  'import json,sys; [print(l["Name"], l["Locations"], l["CollectionType"]) for l in json.load(sys.stdin)]'
```

- **幂等**：再点一次是 0 新增、0 写请求（`created` 回到 0）。
- **只增不删**：分类消失只列在 `orphans[]`，不删库。确认要删时显式带 `prune`：
  `curl -X POST -H "X-Admin-Token: $TOKEN" -d '{"prune":true}' …/api/jellyfin/libraries`。
  只删「名字带前缀 + 路径在库根下一层 + 分类已不存在」的库。
- **失败隔离**：Jellyfin 停机时同步、WebDAV、播放都不受影响，原因只出现在
  `/api/status` 的 `jellyfin.last_error` 与日志里。
- **API key 不外泄**：`/api/status` 只给 `api_key_set`，日志与错误文本里没有 key。
- **`failed[]` 是路径问题**：分类列在 `jellyfin.failed` 里 = Jellyfin 拒绝建这个库，实测
  12.1 的原文是 `The specified path does not exist: <path>`。三个常见原因：目录还没被
  rclone 刷出来（`--dir-cache-time`）、分类只存在于数据库而挂载上没有对应目录、
  `jellyfin_library_root` 填错了一层。修好后下次同步会自动补上，不用清状态。
- **库内是剧目录而不是分片目录**：这依赖**去掉分片层**（MKACG-651）先合入。分片层还在时
  库根的直接子目录是 `00`–`7f`，Jellyfin 会把分片目录认成 Series、真剧目录退化成普通
  目录 —— 本服务建库正确也照样如此（12.1 实测复现）。所以 651 必须先合并。

## 6. 播放面自检

```bash
# 取任意一条 .strm 里的 URL
STRM=$(rclone cat "dramahub:/<分类>/<剧>/Season 01/<剧> - S01E01 - 第1集.strm")
curl -sS "$STRM" | head -12         # #EXTM3U … #EXT-X-ENDLIST；单集就是一条 playlist
ID=$(echo "$STRM" | sed -E 's#.*/video/([^/]+)\.m3u8#\1#')
curl -sS -o /tmp/0.m4s -w '%{http_code}\n' "http://127.0.0.1:8080/video/$ID/0.m4s"
# 每集一片的 fMP4：片头是 ftyp，且单看这一片就能 ffprobe 出视频流与完整时长
head -c 8 /tmp/0.m4s | tail -c 4
ffprobe -v error -show_streams -show_entries format=duration /tmp/0.m4s | head -20
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

## 升级须知（含 0005 / 0006 / 0007 / 0008 的版本）

- `0005` 删掉手工编排的 `merge_group` / `merge_item`，并清掉 `emby_item` 里新代码读不动
  的旧合并行（uid 形如 `m<组 id>`）。
- `0008` 直接删掉 `emby_item` 表（ADR-0014）：对外剧集改成读时现算，库里只留原始集。
  升级后不需要任何重建动作 —— 下一次目录列举/播放就是按当前 `merge_chunk_size` 算出来的
  结果，旧合并 id 在该 N 下照旧有效（uid 形态未变）。
- 合并是自动的：改 `merge_chunk_size`（默认 10）**下一次访问即生效**，对外集号重排、
  与该 N 不一致的旧 id 立即 404。
- `0006` 把 `segment` 表收窄成「每集的播放身份」`(episode_id, channel)`，并清空整张表
  （切点/时长/关键帧/编码签名/mode 五个概念都消失）；`artifact` 去掉 `index` kind，
  `settings` 里「分片目标时长」那一项已删除、不再是合法配置项。**旧缓存不迁移**：`.ts` 分片因为
  键与后缀都变了而永不命中，由 `cache_lru_days` 自然淘汰；升级后每个从没播过的 `uid`
  首次播放会多一次 `ffprobe`（取时长），并且首播必须读完整个源（一集一片的既定代价）。
- `0007` 删掉 `drama.shard` 列与 `idx_drama_genre_shard_title`，改走 `idx_drama_genre_title`，
  **WebDAV 路径全变**（少一层 `<分片>/`）。**Emby / Jellyfin 的库必须重建重扫**，并把库根
  改成「每个分类目录一个 media folder」——库根挂在 `/webdav` 时分类目录会被当成 Series
  （实测 1436 部剧只认出 59 部，`docs/DIRECTORY-CONTRACT.md`「被推翻的做法」）。
  `DROP COLUMN` 不可回滚。

## 常见失败

| 现象 | 原因 / 处置 |
|---|---|
| 启动即退出，日志 `数据库不可用` | `DRAMA_HUB_DB_URL` 没给/连不上；库必须先存在（服务不建库）。容器里访问宿主库用容器网络里的服务名，不要写 `127.0.0.1` |
| `/api/*` 写操作 401 | 没带 `X-Admin-Token`，或没设 `DRAMA_HUB_ADMIN_TOKEN`（fail closed） |
| `PROPFIND` 401 | `webdav_user/webdav_pass` 还没写（第 3.2 步），或凭据错 |
| 缓存目录 `permission denied` | 忘了 `--userns=keep-id` |
| 同步/播放日志里大量 `upstream_status:444` | 上游限流（累计约 1 万次 `/player` 后触发），等十几分钟；不要写重试循环 |
| `/video/{id}.m3u8` 502 | 该集解析不到片源：受保护剧的网页只有预览，完整片源走 App 通道；App 被拒/不可用时按契约回 502（日志一条 `WarnContext` 说明 `uid`/`ep_no`/`channel`/`reason`） |
| `/video/{id}.m3u8` 503 + `Retry-After` | 首次播放要探测时长，30 秒硬上限；稍后重试 |
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
| `rclone lsf -R` 文件数 ≠ `对外集数×2 + 剧数` | 若首轮仍在灌库，清单与库是两个时刻的快照。等 `/api/status` 的 `running=false` 再比 |
| App 通道连续几分钟全是 `/video` 502，日志 `App 取流 … "bytes":0` + `reason:"ErrSchema"` | 上游端点 `200 + 空响应体`。实测连续 25 分钟后自行恢复（同期同一 vid 之前是 18 KB 正常体）。按契约回 502 且不回落预览是正确行为；重试即可，不要当代码缺陷 |

`docs/SCHEDULER-MODEL.md` 把「首启立刻跑一轮」写成既定行为；本文件的 3.1 只讲怎么在验收时
绕开它，不改调度语义。
