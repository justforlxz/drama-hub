# drama-hub

短剧媒体管理器。把红果短剧的数据整理成 Emby 能直接识别的剧集库。

- 只读虚拟 WebDAV：按需渲染 `.strm` / `.nfo` / 封面，任何 WebDAV 客户端都能直接读
- 出流服务：单集 MP4 透传（支持 Range），合并集自造 HLS 播放列表，按分片回源
- 合并集：N 集合成 1 条剧集，时间轴精确到关键帧，**不落物化文件**
- 缓存：分片级 LRU，上限与保留期后台可调
- Jellyfin 媒体库：一个分类一个库（库根 = `<媒体库根>/<分类>`），配好 API key 后由服务
  自动创建与维护；分类开关决定哪些库保留（ADR-0012）

> **服务边界**：本项目只提供 HTTP —— 只读 WebDAV（元数据）与 `/video`（播放字节）。
> 挂载 / 客户端接入由使用方自选（rclone、RaiDrive、davfs2、系统文件管理器、浏览器均可），
> **不属于本项目的交付物**；镜像里不含任何挂载客户端，把服务跑起来也不需要它。
> 想让 Emby / Jellyfin 读到目录时再看「客户端接入（示例）」。

## 快速开始

```bash
make build                    # 产出 ./bin/drama-hub
# PostgreSQL 是必选外部依赖（ADR-0009）：先准备好库，再用连接串启动
DRAMA_HUB_DB_URL='postgres://drama:drama@127.0.0.1:5432/drama?sslmode=disable' \
DRAMA_HUB_ADDR=:8080 ./bin/drama-hub
```

启动时自动迁移到最新 schema（库本身要先存在，服务不建库）。`stream_secret` 与 `cache_dir` 由环境变量**只在键缺失时**写入
一次；WebDAV 账号密码不走 env，用后台配置（`PATCH /api/settings` 或 `/settings` 页的表单）
写入：

```bash
DRAMA_HUB_STREAM_SECRET=<≥32 字节> DRAMA_HUB_CACHE_DIR=/data/cache \
DRAMA_HUB_ADMIN_TOKEN=<自己定一个长随机串> ./bin/drama-hub
```

`DRAMA_HUB_ADMIN_TOKEN` 是**写操作的令牌**：`POST` / `DELETE` / `PATCH` 必须带
`X-Admin-Token` 请求头，否则一律 401（没设这个 env 时所有写操作都不可用，页面会直说）。
读操作（页面、`GET /api/*`、WebDAV、播放）不需要它。新部署的完整路径就是：起容器 →
用令牌写 WebDAV 账号密码 → 可用：

```bash
curl -X PATCH -H "X-Admin-Token: $DRAMA_HUB_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"settings":{"webdav_user":"emby","webdav_pass":"<密码>","strm_prefix":"http://192.168.1.10:8080"}}' \
  http://127.0.0.1:8080/api/settings
```

已有值不会被 env 覆盖；之后改值都走这条接口（或在 `/settings` 页上改，每项标着生效时机，
见 `docs/DEV-STANDARDS.md`「配置」）。`strm_prefix` 留空时 `.strm` 里的地址按访问的 `Host`
推导成绝对 URL，默认安装即可直接播放。

## HTTP 接口

未实现的端点返回 **501 Not Implemented**（已规划未实现），不是 404；未装配的
路径返回 404。

> **2.0 口径**：WebDAV 挂在 `/webdav/`（`/dav/` 不留兼容别名），目录形状是
> `/{分类}/{剧名} ({年份})/Season 01/…`；播放面只有 `/video` 两个口子，
> `.strm` 里写的是 `{strm_prefix}/video/{id}.m3u8`（单集与合并集同一形态）。
> 1.0 的 `/s/{token}/*` 已退役、不留兼容层。契约真值：`docs/PLAYBACK-CONTRACT.md`；
> 目录形状：`docs/DIRECTORY-CONTRACT.md`。

| 端点 | 方法 | 状态 | 说明 |
|---|---|---|---|
| `/healthz` | GET（HEAD 隐式） | 200 `{"status":"ok"}` | 存活探测 |
| `/webdav/` 及全部子路径 | 任意 | 207/200/308/401/403/404/405/415/500 | 只读 WebDAV：PROPFIND 207（带 getcontentlength；**`Depth: infinity` 与省略 `Depth` 一律 `403` + `<D:propfind-finite-depth/>`**，见 RFC 4918 §9.1 与 `docs/adr/0017`——客户端收到后改回递归 `Depth: 1`；数值深度 ≥2 仍由 `x/net/webdav` 直接 400。拦截前的实测代价见 `docs/DIRECTORY-CONTRACT.md`「单目录子项上限」）、**集合的 GET/HEAD → 200 `text/html` 目录页**（浏览器/文件管理器列目录用；单页 ≤ settings `webdav_page_size`（默认 1000），超出显示「共 M 项，已显示前 N」，`.strm` 的链接指向 `/video/…` 而不是 DAV 自身路径，见 `docs/DIRECTORY-CONTRACT.md`「集合 GET」）、GET strm/nfo 200；树是 `/{分类}/{剧名} ({年份})/Season 01/<集>.{strm,nfo}`；Basic 认证（settings `webdav_user`/`webdav_pass`），无认证 401；认证后 `/webdav`（无尾斜杠）308 到 `/webdav/`（跳转在认证之后，凭据不会丢）；405 一律带 `Allow`，OPTIONS 的 `Allow` 含 `GET, HEAD`（集合也能列目录）；写意图被拒的状态码由协议层决定（`PUT` 404、`MOVE`/`COPY` 403、`DELETE`/无 body 的 `MKCOL` 405、带 body 的 `MKCOL` 415、`PROPPATCH` 500，逐格前置条件见 `docs/DEV-STANDARDS.md`「只读」） |
| `/video/{id}.m3u8` | GET / HEAD | 200/401/403/404/501/502/503 | 自造 VOD playlist：单集与合并集**同一路径形态**，集边界带 `#EXT-X-DISCONTINUITY` |
| `/video/{id}/{seq}.m4s` | GET / HEAD | 200/206/401/403/404/501/502/503 | 整集 fMP4 分片：本地缓存命中即回，未命中整集转封装并落缓存 |
| `/s/{token}/…`、`/dav/…` | 任意 | 404 | 已退役（不留兼容层） |
| `/` | GET | 200 | 管理界面：概览 = 健康带（同步计数、播放源调度、Jellyfin 媒体库现状与问题） |
| `/library` | GET | 200 | 管理界面：剧库（台账 / 海报墙、搜索与筛选、补录） |
| `/library/{id}` | GET | 200 / 404 | 管理界面：剧详情（封面、WebDAV 路径、Jellyfin 归属、集分块与播放地址）；id 非法或不存在 404 |
| `/categories` | GET | 200 | 管理界面：分类（剧数 / 集数、Jellyfin 库状态、启用开关） |
| `/settings` | GET | 200 | 管理界面：设置（访问 / 同步与刷新 / 合并 / 缓存 / Jellyfin 共 12 个可改项，一个保存按钮） |
| `/static/app.css`、`/static/app.js`、`/static/paging.js` | GET | 200 | 内嵌样式与脚本（随二进制，无构建步骤） |
| `/api/dramas` | GET / POST | 200 / 200·201 | 剧目列表（含 `genre` / `plot` / `has_cover` 与计数）/ 按上游剧 id 同步入库：**新建 201、已存在（只更新）200** |
| `/api/dramas/{id}/episodes` | GET | 200 / 400 / 404 | 剧详情页的集分块（`merge_chunk_size` + 每块的 `play_id` / `play_url` / 成员集）；id 非法 400、剧不存在 404 |
| `/api/dramas/{id}/cover` | GET | 200 / 404 | 海报字节（`image/*` + 精确 `Content-Length`）；没有海报或上游失败 404 |
| `/api/status` | GET | 200 | 同步计数、运行配置现状（含各键的现读值；密钥只报「是否已设置」）、各源调度状态、`jellyfin` 块（媒体库同步现状：配置、上次结果、运行中进度、孤儿库；API key 只报 `api_key_set`）、`admin_token_set`（写操作能不能用） |
| `/api/playback` | GET | 200 | 正在播放（只读、纯内存会话）：`sessions` 为当前几条流，每条带 uid、状态（`state`：`playing` 出过分片 / `probing` 只取过播放列表）、剧名、集号或合并块范围、已请求到第几片、播放位置/总时长（总时长取刚生成的播放时间轴，与 m3u8 的 `#EXTINF` 总和同口径）、已出字节、客户端 IP 与 UA、首末活跃时间；`streams` 只算正在播放的会话数、`probing` 是只取播放列表的探测会话数、`clients`/`bytes` 也按播放中统计（探测不计入），没有播放时 `sessions` 为 `[]`。会话超过 settings `play_session_ttl_sec` 无活动即回收，重启清空；同步轮次不计入 |
| `/api/jellyfin/libraries` | GET / POST | 200 / 202 / 400 / 401 / 409 | GET = 一轮媒体库同步的现状与进度（`running` / `phase_label` / `processed` / `total` / `started_at`；收尾后 `created` / `unchanged` / `removed` / `duration_ms` / `delete_failed`），页面每秒轮询它、不碰 drama 表；POST = 手动跑一轮（无 body 参数，删不删库由分类开关决定）；400 = 三项配置没配齐（点名缺哪个键），409 = 上一轮还在跑（接着 GET 看那一轮进度） |
| `/api/jellyfin/categories` | GET / PATCH | 200 / 400 / 401 | GET = 全部分类 + 开关状态（`{"categories":[{"name","enabled"}]}`）；PATCH = 开关一个分类（`{"name":"都市","enabled":false}`），只写库、不碰 Jellyfin |
| `/api/settings` | PATCH | 200 / 400 / 401 | 按 key 批量写配置：`{"settings":{"<key>":"<值>"}}` → 归一化后的值；未知键 / 非法值 400 且整批不落库；写操作要 `X-Admin-Token` |
| 其它未登记路径 | 任意 | 404 | 由 web 的 mux 兜底 |

对外 id 是 HMAC 常量签名（ADR-0003）：`id = uid + "-" + hex(HMAC-SHA256(secret,
uid))[:12]`，`uid` 是 `e<episode.id>`（单集）或 `m<dramaID>_<epFrom>_<epTo>`（合并集），secret 取
`settings.stream_secret`。一个 id 同时给 playlist 与分片用，无过期、无 token 表、不落库；
签名错 → 401，`uid` 不是当前派生规则下的播放单元 → 404，解析不到直链或上游失败 → 502；
该集需要登录态（App 通道与匿名站点通道都不通、`hongguo_cookie` 未配或已失效）→ 403，
只提供拿不到 key 的加密流 → 501。三种都不是「源坏了」的同一种说法：403 补 Cookie 即可，
501 是能力边界。

**直链只在请求时解析**：`episode.play_url` / `play_url_at` 已由 `0004` 迁移删除（直链是
短时凭据，落库就是过期链接），每次播放请求现调插件 `Resolve`，换链后下一次请求自然拿到
新链。

**第一次播放某个 uid 时**，`internal/segment` 为每个成员集同步 `ffprobe` 一次取
`format=duration` 并回填 `episode.duration_sec`（只填 0），30 秒硬上限、超时 503 +
`Retry-After`；**不做批量预建**（库里 2847 万集）。分片是**一整集**：`ffmpeg -c copy
-movflags +frag_keyframe+empty_moov+default_base_moof -f mp4` 转成自包含 fMP4，
单片返回 `video/mp4` 并带 `Cache-Control: public, max-age=86400`；
ffmpeg 失败 → 502 且不写缓存。详见 `docs/adr/0011-episode-per-segment-fmp4.md`。

分片缓存归本服务（ADR-0007）：键 `sha256("e<episode.id>" | channel)` —— **seq 不进键**，
同一集在单集 / 合并集 / 重新分组后共用一份，落 `<cache_dir>/seg/<hash>.m4s`。命中零外呼、
并发同键只生成一次（single-flight），按 `cache_max_mb` / `cache_lru_days` 淘汰
（`docs/CACHE-MODEL.md`）。

合并集（`uid = m<dramaID>_<epFrom>_<epTo>`）是虚拟 HLS 分片聚合：**每集一片**，playlist
在集边界插 `#EXT-X-DISCONTINUITY`，分片 URL 是 `/video/{id}/{seq}.m4s`，`seq` 就是块内
成员下标；每片自带 `ftyp+moov`，所以集边界的时间戳重置由 DISCONTINUITY 表达，不需要平移
PTS（ADR-0011）。

**合并是自动的**（ADR-0010）：每部剧的集按 `settings.merge_chunk_size`（默认 10）切连续
块，第 i 块占对外集号 i，块内 1 集走单集规则、≥2 集是合并集。库里只存原始集，分块在
每次读取时按当前 N 现算：改这个值后**不需要同步、不需要重建**，下一次列举 / 播放就是新
分块，旧 id 随即 404（ADR-0014）；不需要人工编排。

## 管理界面

四页 + 一个对象详情页，纯 Go 模板 + 原生 JS，随二进制嵌入（没有 `package.json`、没有构建步骤）：

| 页面 | 能干的事 |
|---|---|
| `/` 概览 | 健康带：摘要条（剧/集/对外集计数与最近同步时间）、「正在播放」（当前几条流、剧名与集/块、进度、已出字节、客户端，没有播放时明确写「当前没有播放」，每 4 秒且仅在页面可见时刷新）、每个播放源一张卡片（状态徽标、上次运行/成功/下次计划、上次错误、立即刷新）、Jellyfin 卡片（状态徽标、同步进度与上次结果、孤儿/冲突/被拒分类、同步媒体库） |
| `/library` 剧库 | 工具条：搜索（剧名 / 上游 id）、分类下拉、状态筛选（无封面 / 集数不齐）、视图切换（台账 / 海报墙）、补录；台账九列（封面 / 标题 / 年份 / 分类 / 集数 / 上游 id / 上次同步 / Jellyfin 归属 / 操作），窄屏按行塌成卡片；前端 50 条/页 |
| `/library/{id}` 剧详情 | 封面、元信息、简介、WebDAV 路径（可复制）、Jellyfin 归属、单剧刷新；集按当前 `merge_chunk_size` 分块，每块给「第 A-B 集」、成员集与时长、播放地址与复制按钮 |
| `/categories` 分类 | 分类名、剧数 / 集数、Jellyfin 库状态、启用开关（一行一个分类）；关掉的分类在下一轮同步时删库，剧与文件不动 |
| `/settings` 设置 | 一个表单、一个「保存」：访问（WebDAV 账号密码 / strm 前缀）、同步与刷新、合并与对外集号、缓存、Jellyfin，每项带生效时机徽标与 settings key 提示；底部可折叠「只读运行参数」（缓存目录 / 每轮存量 / 单轮预算 / 密钥与凭据是否已设置） |

页面里的每个操作都直接落到上面的 `/api/` 端点，渲染结果与 API 返回一致；补录走
`POST /api/dramas`（201 新建、200 已在库只更新）。写操作（刷新 / 同步 / 改配置）要先在
顶栏点「写操作 · 未解锁」打开令牌弹窗填 `DRAMA_HUB_ADMIN_TOKEN`，令牌只存在本标签页
（`sessionStorage`）；任一写请求 401 会自动弹出该弹窗。

界面只有一层 `:root` token（颜色 / 间距 / 字号 / 圆角），深色只覆盖颜色；所有页面都是 0 张
`<table>`（卡片 + `dl`），剧库台账用 ARIA `table` 语义 + CSS Grid（ADR-0017），窄屏按行
塌成卡片、触控目标 ≥44px；不发请求的纯样式改动不会引入构建步骤。

缓存由本服务自己负责（ADR-0007）：海报与分片落 `<cache_dir>`，按
`cache_max_mb` / `cache_lru_days` 淘汰，规则见 `docs/CACHE-MODEL.md`；播放字节一律经本
服务的 `/video/…`，客户端只读 WebDAV，不需要任何缓存模式（示例里的 rclone 也不必再开
`--vfs-cache-mode full`）。
（`/api/status` 原先那份 rclone 建议参数属 ADR-0004 遗留，已在 MKACG-616 删除：
页面只展示真正参与运行的配置，缓存目录/上限/保留天数都在设置页里。）

```bash
open http://127.0.0.1:8080/            # 状态页（顶栏解锁写操作后才能同步/改配置）
curl -i -X POST -H 'Content-Type: application/json' \
  -H "X-Admin-Token: $DRAMA_HUB_ADMIN_TOKEN" \
  -d '{"series_id":"7503745441910017049"}' http://127.0.0.1:8080/api/dramas
# 首次同步该剧 → 201 Created；再发一次（库里只更新、不新增）→ 200 OK
```

```bash
curl -i http://127.0.0.1:8080/healthz   # 200 {"status":"ok"}
curl -u <webdav_user>:<webdav_pass> -X PROPFIND -H 'Depth: 1' http://127.0.0.1:8080/webdav/  # 207 + getcontentlength
# Depth 只认 0 / 1 / infinity；数值深度 ≥2 → 400。`Depth: infinity` 与省略 Depth 会被直接拒成
# 403 + <D:propfind-finite-depth/>（不再递归整棵树，ADR-0017），所以手工请求写 Depth: 1。
```

每个请求输出一条结构化访问日志（`method` / `path` / `status` / `duration_ms`，见
`internal/httpx`）；只记路径、不记 query，也不记带签名的上游直链。
错误响应统一为 `{"code":"<kind>","message":"..."}`（404/400/401/403/501/502/503，其余 500）。

需要的运行环境：`ffmpeg`、`ffprobe` 在 `PATH` 里。

## 客户端接入（示例）

**示例，不是依赖**：本服务只提供 HTTP（只读 WebDAV + `/video`），镜像里没有 rclone，也
不需要装它才能把服务跑起来。下面用 rclone 演示「给 Emby / Jellyfin 一个本地目录」这一步，
换成 RaiDrive、davfs2、文件管理器「连接服务器」，或直接用浏览器打开 `/webdav/` 的目录页
（集合 GET 返回可浏览的 HTML 列表）同样成立。

```bash
rclone config create dramahub webdav url http://127.0.0.1:8080/webdav vendor other \
  user <webdav_user> pass <webdav_pass>
rclone mount dramahub:/ /mnt/dramahub --dir-cache-time 30s
rclone lsf --max-depth 4 dramahub:/     # 分类 → 剧 → Season 01 → strm/nfo
```

Emby / Jellyfin 的库**按分类目录分挂**：一个「电视节目」库，媒体文件夹逐个加
`/mnt/dramahub/<分类>`（真实 `genre` 词表 68 个）。库根直接挂 `/mnt/dramahub` 会把分类
目录当 Series（1436 部剧只识别出 59 部，`jellyfin.db` 实测）。

分类目录一次 `PROPFIND` 要列出该分类的全部剧（`PROPFIND` 没有分页），实测 10 万子项
2.6 s / 54 MB 响应体、21.6 万子项 3.5 s / 117 MB，判据与越线项的升级路径见
`docs/DIRECTORY-CONTRACT.md`「单目录子项上限」。

## 接到 Jellyfin（一分类一库）

Emby 能逐层往下扫，Jellyfin 不能：它只把**库根的直接子目录**当剧集。库根挂在 DAV 根上时，
分类会被认成 Series、剧目录退化成普通目录。所以用一分类一库，由本服务自动建：

1. 在 Jellyfin 里建一个 API key（12.1 只认 `Authorization: MediaBrowser Token="…"`）；
2. `PATCH /api/settings` 写 `jellyfin_url` / `jellyfin_api_key` / `jellyfin_library_root`
   （库根填 **Jellyfin 容器内**看到的挂载根）；
3. `POST /api/jellyfin/libraries` 跑一轮，或等同步轮末自动跑（幂等）。

点「同步媒体库」之后页面每秒轮询 `GET /api/jellyfin/libraries`，把这一轮显示成一行：当前阶段
（读取现有库 / 列分类 / 比对 / 创建 / 删除已关闭分类的库 / 触发扫描）、已处理几项、开始与已用时间；收尾后
那一行变成「上次同步：时间 · 新建 / 更新 0 / 已存在 / 删除 / 失败 · 耗时」，刷新页面读回同一份
状态。运行中再点一次返回 409（已在运行），页面会接着显示正在跑的那一轮。本服务不改已存在的库，
所以「更新」恒为 0，「已存在」就是这一轮幂等命中的条数。

分类开关在 `/categories` 页，一行一个分类、默认全开：关掉的分类会在下一轮
同步时删掉对应的库（拨开关本身不发请求；点「同步媒体库」则当场删），重新打开后下一轮会重建。
删除范围只限本服务托管的库（带前缀 + 位于库根下一层），用户手建的库永不删；删除失败会进
`delete_failed` 与 `last_error`，下一轮自动重试。分类从树里整个消失时仍然只列成「孤儿库」、
不删。Jellyfin 不可达只影响这一段，同步/WebDAV/播放照常。步骤与核对命令见
`docs/DEPLOY.md`「5b」，理由与 API 事实见 `docs/adr/0012-jellyfin-libraries.md`。

## 容器镜像

`main` 的每次 push 与 `v*` tag 会构建镜像，ci 全绿才推到
`ghcr.io/justforlxz/drama-hub`；功能分支 / PR 的 push 只跑 `ci.yml`，不构建镜像
（口径见 `docs/adr/0008-image-publish.md`）。

```bash
podman pull ghcr.io/justforlxz/drama-hub:latest
podman network create drama-hub
podman run -d --name drama-hub-pg --network drama-hub \
  -e POSTGRES_USER=drama -e POSTGRES_PASSWORD=drama -e POSTGRES_DB=drama \
  -v "$PWD/pgdata:/var/lib/postgresql/data:Z" postgres:16
podman run -d --name drama-hub --network drama-hub -p 8080:8080 \
  -e DRAMA_HUB_DB_URL='postgres://drama:drama@drama-hub-pg:5432/drama?sslmode=disable' \
  --userns=keep-id -v "$PWD/data:/data:Z" \
  ghcr.io/justforlxz/drama-hub:latest
curl -s localhost:8080/healthz                        # 200 {"status":"ok"}
podman logs drama-hub | head -1                        # version / commit 与构建参数一致
```

| tag | 什么时候更新 |
|---|---|
| `sha-<7位>` | `main` 每次 push；不可变——同一个 commit 永远同一份字节 |
| `latest` | **只在 `main`**；`v*` tag 构建不动它 |
| `vX.Y.Z` | 推 `v*` tag 时 |

- 两个平台在同一个 manifest list 里：`linux/amd64`、`linux/arm64`（`docker manifest inspect` 除这两个平台外还有 buildx 的 provenance 清单，`unknown/unknown`，属正常）。
- **门禁**：lint 与容器测试都过才发布（`needs: [lint, test]`）；ci 红就是没有任何 tag 被推。
- 构建参数 `VERSION` / `COMMIT` 经 `-ldflags` 打进二进制，起服务的日志第一行即为实际值。
- 镜像内 `USER app`（uid 1000）、`VOLUME ["/data"]`、`EXPOSE 8080`，默认
  `DRAMA_HUB_ADDR=:8080`。`DRAMA_HUB_DB_URL` 必填（镜像里不设：连接串含凭据），
  没给就启动失败并打印支持的 env。挂 `/data` 持久化的是缓存（海报/分片/索引），
  剧集数据在 PostgreSQL 里。
- **绑宿主缓存目录要带 `--userns=keep-id`（与 `make run` 一致）**：rootless podman 会把容器内的
  uid 1000 映射到 subuid，写不进属主是宿主用户的目录（实测报 `permission denied`）。
  缓存目录不可用时服务仍能起（会 warn），但播放与封面会每次回源。
- 起服务后调度器会立刻按默认间隔抓一次源，日志里有 `上游请求` 属正常。
- ghcr 包默认私有：拉取前先 `podman login ghcr.io -u <user> -p <带 read:packages 的 PAT>`。
- 首次配置入口：`DRAMA_HUB_STREAM_SECRET` / `DRAMA_HUB_CACHE_DIR` 由 env 在键缺失时种下
  （MKACG-616）；WebDAV 账号密码用 `DRAMA_HUB_ADMIN_TOKEN` 调 `PATCH /api/settings` 写
  （MKACG-622），见「快速开始」。

自己构建同一份镜像是 `make image`（`ENGINE=podman`，默认只出本机平台）。

## 开发环境

生产与测试共用同一个镜像，ffmpeg 版本由镜像固定（见 `env.lock`）：

```bash
make env-check                  # 校验容器内 go/ffmpeg 版本与 env.lock 一致
make e2e-image                  # 构建测试镜像（runtime + Go 工具链）
make test                       # 两级：unit（--network=none）+ integration（起 postgres:16）
make run                        # 容器内起服务，挂 ./data，映射 :8080
./scripts/ci.sh                 # 全量门禁：lint + 两级测试 + 版本锁
```

本机 `make build` / `make lint` 只用于快速迭代：开发机 ffmpeg 与容器 ffmpeg 版本不同，
本机结果不能作为验收依据（运行与测试只有容器这一条路径）。升级基础镜像 = 改 `Containerfile` 的 digest + `env.lock` + 跑 `make env-check`。

podman 6 会把短名当远程仓库去解析，因此镜像名一律用全限定名 `localhost/drama-hub`，运行命令带 `--pull=never`。
宿主机 8080 被占用时用 `make run PORT=18081`。

## 文档

- `AGENTS.md` — 硬约束与目录约定（改代码前先读）
- `docs/DEV-STANDARDS.md` — 依赖、错误处理、配置、日志规范
- `docs/TEST-STANDARDS.md` — 测试分层、必须覆盖的协议断言
- `docs/ACCEPTANCE.md` — 真机验收清单
- `docs/DEPLOY.md` — 从零到能播的最短路径（env 清单、配置顺序、常见失败）
- `docs/adr/` — 架构决策记录
- **2.0（框架 + 播放源插件化）设计定稿件**：
  - `docs/PLUGIN-CONTRACT.md` — 插件接口、错误语义、注册与配置
  - `docs/SCHEDULER-MODEL.md` — 刷新调度、并发、超时、失败隔离、手动触发
  - `docs/DATA-MODEL.md` — 字段来源、`0002`–`0004` 迁移草案、增删改语义
  - `docs/DIRECTORY-CONTRACT.md` — `/<类型>/` 目录树、同名冲突、Emby 影响
  - `docs/PLAYBACK-CONTRACT.md` — `/webdav` 与 `/video/{id}.m3u8`、`.strm` 形态
  - `docs/CACHE-MODEL.md` — 海报与分片本地缓存、淘汰、并发
  - `docs/L1-CORPUS-SCOPE.md` — 「获取全部的剧集」口径与红果实测数字
  - `docs/adr/0006-framework-plugin-boundary.md` — 框架 / 插件边界决策
  - `docs/adr/0007-own-hls-and-local-cache.md` — 播放统一走自造 HLS、缓存收回本服务
  - `docs/adr/0010-auto-merge-chunking.md` — 合并集自动分块、手工编排移除
  - `docs/adr/0012-jellyfin-libraries.md` — Jellyfin 一分类一库、自管判定、分类开关即删除开关
  - `docs/adr/0013-drop-directory-shard-layer.md` — 去掉分片目录层、单目录子项上限实测
  - `docs/adr/0014-runtime-derived-emby-items.md` — 合并集只在运行时派生、删除 `emby_item` 表
  - `docs/adr/0008-image-publish.md` — 镜像发布口径：ghcr、双架构、ci 绿才发、`latest` 跟 main

## 状态

实现中，分阶段推进：P1 数据与管理面 → P2 WebDAV+NFO → P3 单集出流 → P4 缓存 → P5 合并集 → P6 App 回退 → P7 缓存观测。M7（合并集）已实现：playlist 生成、跨集时间轴、按 `merge_chunk_size` 的运行时自动分块齐备（ADR-0014）。R1（管理界面）已实现：同步与缓存看板接真实 API。真机验收（P2–P5）待跑。

设计基线见 MKACG-580 附件 `DESIGN-drama-hub.md`。
