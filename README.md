# drama-hub

短剧媒体管理器。把红果短剧的数据整理成 Emby 能直接识别的剧集库。

- 只读虚拟 WebDAV：按需渲染 `.strm` / `.nfo` / 封面，Emby 通过 `rclone mount` 扫描
- 出流服务：单集 MP4 透传（支持 Range），合并集自造 HLS 播放列表，按分片回源
- 合并集：N 集合成 1 条剧集，时间轴精确到关键帧，**不落物化文件**
- 缓存：分片级 LRU，上限与保留期后台可调

## 快速开始

```bash
make build                    # 产出 ./bin/drama-hub
DRAMA_HUB_DB=./data/drama.db DRAMA_HUB_ADDR=:8080 ./bin/drama-hub
```

首次启动自动建库与迁移。

## HTTP 接口

未实现的端点返回 **501 Not Implemented**（已规划未实现），不是 404；未装配的
路径返回 404。

> **2.0 变更中**：`/dav/` 改挂 `/webdav/`；出流统一为 `/video/{id}.m3u8` +
> `/video/{id}/{seq}.ts`，`/s/*` 退役；`.strm` 里写的是 `/video/{id}.m3u8`。
> 下表仍是 1.0 的形态，以 `docs/PLAYBACK-CONTRACT.md` 为准。

| 端点 | 方法 | 状态 | 说明 |
|---|---|---|---|
| `/healthz` | GET（HEAD 隐式） | 200 `{"status":"ok"}` | 存活探测 |
| `/dav/` 及全部子路径 | 任意 | 207/200/401/404/405 | 只读 WebDAV：PROPFIND 207（带 getcontentlength）、GET strm/nfo 200；Basic 认证（settings `webdav_user`/`webdav_pass`），无认证 401 |
| `/s/{token}/e/{uid}` | GET / HEAD | 200/206/416（单集）<br/>200（合并集） | 单集 MP4 出流：HMAC 签名 + Range 透传；**uid 是合并集句柄（`m…`）时返回自造 m3u8** |
| `/s/{token}/e/{uid}/master.m3u8` | GET | 200/404 | 合并集 playlist 的标准别名 |
| `/s/{token}/seg/{uid}/{seq}.ts` | GET | 200/404/502/503 | 分片：现转封装 MPEG-TS（单集按片内序、合并集按跨集时间轴），索引未就绪 503 |
| `/s/` 其余子路径 | 任意 | 501 | 已规划未实现 |
| `/`、`/merges`、`/status` | GET | 200 | 管理界面三页：剧目 / 合并编排 / 同步与缓存看板 |
| `/static/app.css`、`/static/app.js` | GET | 200 | 内嵌样式与脚本（随二进制，无构建步骤） |
| `/api/dramas` | GET / POST | 200 / 201 | 剧目列表（含计数）/ 按上游剧 id 同步入库 |
| `/api/dramas/{id}` | DELETE | 204 | 删剧目（级联删集、合并组、对外集号） |
| `/api/dramas/{id}/episodes` | GET | 200 | 该剧全部集，供合并编排挑选 |
| `/api/dramas/{id}/merge-groups` | GET / POST | 200 / 201 | 合并组列表 / 建组（`episode_ids` 顺序即时间轴顺序） |
| `/api/merge-groups/{id}` | DELETE | 204 | 删组并重建对外集号 |
| `/api/status` | GET | 200 | 同步计数、运行配置现状、缓存代理建议参数 |
| 其它未登记路径 | 任意 | 404 | 由 web 的 mux 兜底 |

出流 token 是 HMAC 常量签名（ADR-0003）：`token = base64url( HMAC-SHA256(secret,
"stream:"+uid) )`，secret 取 `settings.stream_secret`。错误签名 → 401；uid 非法或
不存在 → 404；`play_url` 为空或上游失败 → 502。`Range` 原样透传到上游，206/416
由上游判定后透传回。

分片端点同样按该 token 鉴权。分片索引由 `internal/segment` 用 `ffprobe
-skip_frame nokey` 预扫源、把切点对齐到关键帧后写入 `segment` 表（未索引 → 503 +
`Retry-After`）；切片走 `ffmpeg -ss <切点> -t <片长> -c copy -f mpegts`，单片
返回 `video/mp2t` 并带 `Cache-Control: public, max-age=86400`。ffmpeg 失败 → 502，
失败结果不落库、不缓存。

合并集（`uid = m<group_id>`）是虚拟 HLS 分片聚合：strm 指向合并集时返回自造
playlist（`#EXT-X-VERSION:4` + `#EXT-X-DISCONTINUITY` 在集边界），分片 URL 指回
`/s/{token}/seg/{uid}/{seq}.ts`，seq 是跨集连续时间轴；切片按 `seq → (集号, 集内秒)`
解析，`-output_ts_offset` 保证跨集时间戳连续。合并编排（N 集并 1）在管理界面
`/merges` 里做，重建 `emby_item` 派生表。

## 管理界面

三页，纯 Go 模板 + 原生 JS，随二进制嵌入（没有 `package.json`、没有构建步骤）：

| 页面 | 能干的事 |
|---|---|
| `/` 剧目 | 粘贴上游剧 id 或 `https://hongguoduanju.com/player/<id>` 链接同步入库；删剧目（连带集、合并组、对外集号） |
| `/merges` 合并编排 | 选剧 → 按想要的播放顺序点集 → 建组（点击顺序即时间轴顺序）→ 删组；已入组的集不可重复选 |
| `/status` 同步与缓存 | 剧目/集/可播/对外集/合并组计数与最近同步时间、运行配置现状（密钥只显示是否已设置）、缓存代理建议参数 |

页面里的每个操作都直接落到上面的 `/api/` 端点，渲染结果与 API 返回一致。缓存面只做
展示：视频缓存由缓存代理淘汰（ADR-0004），看板给出 `--vfs-cache-max-size 200G
--vfs-cache-max-age 720h` 这份建议值，不参与本服务运行逻辑。

```bash
open http://127.0.0.1:8080/            # 剧目
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"series_id":"7503745441910017049"}' http://127.0.0.1:8080/api/dramas
```

```bash
curl -i http://127.0.0.1:8080/healthz   # 200 {"status":"ok"}
curl -u <webdav_user>:<webdav_pass> -X PROPFIND -H 'Depth: 1' http://127.0.0.1:8080/dav/  # 207 + getcontentlength
```

每个请求输出一条结构化访问日志（`method` / `path` / `status` / `duration_ms`，见
`internal/httpx`）；出流路径里的签名 token 段被掩码为 `{token}`，query 不进日志。
错误响应统一为 `{"code":"<kind>","message":"..."}`（404/400/401/502/503，其余 500）。

需要的运行环境：`ffmpeg`、`ffprobe` 在 `PATH` 里。

## 接到 Emby

```bash
rclone config create dramahub webdav url http://127.0.0.1:8080/dav vendor other \
  user <webdav_user> pass <webdav_pass>
rclone mount dramahub:/ /mnt/dramahub --vfs-cache-mode full --dir-cache-time 30s
rclone lsf --max-depth 3 dramahub:/     # 剧目录 → Season 01 → strm/nfo
```

Emby 库路径指向 `/mnt/dramahub`（DAV 根即剧集库根），类型选「电视节目」。

## 开发环境

生产与测试共用同一个镜像，ffmpeg 版本由镜像固定（见 `env.lock`）：

```bash
make env-check                  # 校验容器内 go/ffmpeg 版本与 env.lock 一致
make e2e-image                  # 构建测试镜像（runtime + Go 工具链）
make test-e2e                   # 容器内跑集成测试，--network=none
make run-container              # 容器内起服务，挂 ./data，映射 :8080
```

本机 `make build` / `make test` 只用于快速迭代：开发机 ffmpeg 与容器 ffmpeg 版本不同，
本机结果不能作为验收依据。升级基础镜像 = 改 `Containerfile` 的 digest + `env.lock` + 跑 `make env-check`。

podman 6 会把短名当远程仓库去解析，因此镜像名一律用全限定名 `localhost/drama-hub`，运行命令带 `--pull=never`。
宿主机 8080 被占用时用 `make run-container PORT=18081`。

## 文档

- `AGENTS.md` — 硬约束与目录约定（改代码前先读）
- `docs/DEV-STANDARDS.md` — 依赖、错误处理、配置、日志规范
- `docs/TEST-STANDARDS.md` — 测试分层、必须覆盖的协议断言
- `docs/ACCEPTANCE.md` — 真机验收清单
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

## 状态

实现中，分阶段推进：P1 数据与管理面 → P2 WebDAV+NFO → P3 单集出流 → P4 缓存 → P5 合并集 → P6 App 回退 → P7 缓存观测。M7（合并集）已实现：playlist 生成、跨集时间轴、合并编排与派生表重建齐备。R1（管理界面三页）已实现：剧目增删、合并编排、同步与缓存看板都接真实 API。真机验收（P2–P5）待跑。

设计基线见 MKACG-580 附件 `DESIGN-drama-hub.md`。
