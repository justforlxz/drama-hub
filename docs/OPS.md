# OPS — 运维手册（安装 / 升级 / 回滚）

适用对象：在一台本机或小机器上部署 drama-hub 的人。命令都在仓库根目录执行。

事实边界：本手册只写**当前 `main` 上真实存在**的能力。当前版本单集出流与合并集
出流都还不能真正播出字节（原因与本轮验收证据见 `docs/ACCEPTANCE.md`「2026-10-30 记录」
与本文 §7），因此本手册的「安装完成」判据是**服务起得来、WebDAV 可读、管理面可用**，
不是「能播」。

## 0. 服务形态

- 一个 Go 二进制，三个面：只读 WebDAV（`/dav/`）、出流（`/s/`）、管理界面（`/` + `/api/`）。
- 存储只有 SQLite 一个文件（WAL）。没有第二个运行时、没有前端构建、没有外部缓存服务。
- 外部依赖三个：`ffmpeg`/`ffprobe`（切片与探测）、`rclone`（缓存代理 + 挂载）、Emby（唯一播放客户端）。
- 日志走 stdout，一条一个 JSON 对象（`method` / `path` / `status` / `duration_ms`，token 段已掩码）。

## 1. 前置

| 项 | 要求 | 说明 |
|---|---|---|
| 容器引擎 | podman 或 docker | 推荐路径。ffmpeg 在镜像里，版本由 `env.lock` 锁死 |
| Go | go1.27.x | 只在「本机二进制」路径需要，见 §2.1 |
| ffmpeg / ffprobe | 与 `env.lock` 一致（当前 6.1.2） | 只在「本机二进制」路径需要；版本不同切片行为会不同 |
| rclone | ≥ v1.60（实测 v1.75.2） | WebDAV 访问、缓存代理、Emby 挂载 |
| Emby | 任意近期版本 | 库路径指向 rclone 的挂载点 |

版本一致性用 `./scripts/env-check.sh` 校验（会起容器读实际版本并与 `env.lock` 比对）。

## 2. 安装

### 2.1 取产物

推荐（容器，ffmpeg 版本与测试、线上一致）：

```bash
make image                     # = podman build --target runtime → localhost/drama-hub:dev
```

本机二进制（只适合快速迭代，正式部署不要用，理由见 `docs/TEST-STANDARDS.md`）：

```bash
make build                     # → bin/drama-hub
```

### 2.2 数据目录

数据库路径由 `DRAMA_HUB_DB` 决定（默认 `./data/drama.db`；镜像内固定 `/data/drama.db`）。
目录不存在会自动创建。WAL 模式会在同目录生成 `drama.db-wal`、`drama.db-shm`，备份方式见 §5。

```bash
mkdir -p data
```

### 2.3 启动

容器（`make run` 等价于这条，`PORT` 可改宿主端口）：

```bash
podman run -d --name drama-hub --restart=unless-stopped \
  -p 8080:8080 --userns=keep-id --security-opt label=disable \
  -v "$PWD/data:/data" localhost/drama-hub:dev
```

本机二进制：

```bash
DRAMA_HUB_DB=./data/drama.db DRAMA_HUB_ADDR=:8080 ./bin/drama-hub
```

启动读的环境变量**只有三项**，其余全部走 `settings` 表：

| 变量 | 默认 | 说明 |
|---|---|---|
| `DRAMA_HUB_DB` | `./data/drama.db` | SQLite 文件路径 |
| `DRAMA_HUB_ADDR` | `:8080` | 监听地址 |
| `DRAMA_HUB_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

就绪判据：

```bash
curl -s localhost:8080/healthz          # {"status":"ok"}
```

首次启动自动建库并迁移到最新 `user_version`，重复启动幂等。收到 SIGTERM/SIGINT 时
优雅关闭（10 秒上限）。

### 2.4 引导配置（必做）

**当前版本没有配置写入 API**：管理面只读配置（`/status` 页面与 `/api/status` 只展示），
四个关键项必须写库一次。写库走 `sqlite3`，`value_json` 是 JSON —— 字符串必须带双引号。

```bash
SECRET=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 40)
sqlite3 data/drama.db "
INSERT INTO settings(key,value_json,updated_at) VALUES
 ('webdav_user','\"dramahub\"','$(date -u +%FT%TZ)'),
 ('webdav_pass','\"改成一个口令\"','$(date -u +%FT%TZ)'),
 ('stream_secret','\"$SECRET\"','$(date -u +%FT%TZ)'),
 ('strm_prefix','\"http://127.0.0.1:8080\"','$(date -u +%FT%TZ)')
ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at;"
```

| key | 必填 | 取值 | 作用 |
|---|---|---|---|
| `webdav_user` / `webdav_pass` | 是 | 非空 | WebDAV Basic 认证；缺任一项 `PROPFIND` 恒 401 |
| `stream_secret` | 是 | ≥ 32 字节 | 出流 HMAC 签名密钥（ADR-0003）；缺则 `/s/` 500 |
| `strm_prefix` | 否（默认空） | 绝对 URL | strm 里 URL 的前缀。**接缓存代理时填代理地址**（§2.5），不接代理就填服务自身 |

可选参数（同表、同写法，默认值即 `docs/DEV-STANDARDS.md` 参数表的默认值）：
`seg_target_sec`（int，4–30）、`seg_prefetch`（int，0–10）、`pin_days`（int，0–90）。
校验失败的值不落库，读回时按 400/500 报错 —— 直接写库绕过了这条校验，写错会在请求路径上暴露。

校验：

```bash
curl -sS -o /dev/null -w '%{http_code}\n' -X PROPFIND -H 'Depth: 1' \
  -u dramahub:口令 http://127.0.0.1:8080/dav/            # 207
curl -s localhost:8080/api/status | jq '.config'          # *_set 全为 true
```

### 2.5 缓存代理与 Emby

缓存淘汰由 rclone 负责，本服务不实现（ADR-0004）。`/api/status` 的 `cache` 段给出建议参数
（`--vfs-cache-mode full --vfs-cache-max-size 200G --vfs-cache-max-age 720h`）。

**WebDAV 与 Emby 挂载（已实测可用）**：

```bash
rclone config create dramahub webdav url http://127.0.0.1:8080/dav vendor other \
  user dramahub pass 口令
rclone mount dramahub:/ /mnt/dramahub --vfs-cache-mode full --dir-cache-time 30s
rclone lsf --max-depth 3 dramahub:/     # 剧目录 → Season 01 → strm/nfo
```

Emby 库路径填 `/mnt/dramahub`，类型「电视节目」。

**播放字节的缓存代理（尚未验证）**：`strm_prefix` 的语义是「播放字节经此前缀转发」，
所以接代理时它要指向代理地址（例如 `http://127.0.0.1:8081`），代理再把
`/s/{token}/e/{uid}` 原样转发给本服务。候选接法是给服务本身建一个 `http` 类型远端再
`serve http`：

```bash
rclone config create dramahub-cdn http url http://127.0.0.1:8080/
rclone serve http dramahub-cdn: --addr 127.0.0.1:8081 \
  --vfs-cache-mode full --vfs-cache-max-size 200G --vfs-cache-max-age 720h
```

这条**在 2026-10-30 的验收里没有跑通**（`rclone lsl dramahub-cdn:/healthz` 能取到文件，
但 `serve http` 下所有路径都返回 `File not found`：`http` 远端不支持目录，而服务根
`HEAD /` 返回 200，rclone 把根判成「一个文件」）。缓存代理的最终接法待 P4 的字节来源
问题一起定（见 §7 与 `docs/ACCEPTANCE.md` 的 P4 行）。当前先按直连部署：`strm_prefix`
填服务自身地址、`/api/status` 的 `cache.mode` 会显示 `direct`。

### 2.6 首次同步

管理界面 `/` 粘贴上游剧 id 或 `https://hongguoduanju.com/player/<id>` 链接；或：

```bash
curl -sS -X POST -H 'Content-Type: application/json' \
  -d '{"series_id":"7503745441910017049"}' http://127.0.0.1:8080/api/dramas
```

同步成功后 `rclone lsf --max-depth 3 dramahub:/` 就能看到剧目录。合并集在 `/merges` 里编排
（点选顺序即时间轴顺序），建组后派生表重建、对外集号连续。

## 3. 升级

```bash
# 1) 先备份（必须，见 §5）
sqlite3 data/drama.db ".backup 'backup/drama-$(date +%F-%H%M).db'"

# 2) 停服务
podman stop drama-hub

# 3) 换产物（数据卷不动）
git fetch && git checkout <目标版本>
make image                     # 或 podman load 一个新镜像

# 4) 起服务；迁移在启动时自动向前执行
podman start drama-hub
```

升级后逐条验证：

```bash
podman logs --tail 20 drama-hub                                   # 无 ERROR，看到 listening
curl -s localhost:8080/healthz                                    # {"status":"ok"}
curl -sS -o /dev/null -w '%{http_code}\n' -X PROPFIND -H 'Depth: 1' \
  -u dramahub:口令 http://127.0.0.1:8080/dav/                     # 207
rclone lsf --max-depth 3 dramahub:/ | head                        # 目录结构与升级前一致
sqlite3 data/drama.db 'PRAGMA user_version;'                      # 与新版迁移数一致
curl -s localhost:8080/api/status | jq '.totals'                  # 计数正常
```

注意：

- 迁移只向前、不回退。新版本引入的**必填** settings 项缺失时，请求路径上会报
  `暂不可用`/`必要配置项缺失`，按 §2.4 补一行即可。
- 升级不改 `data/` 内容；strm 若依赖 `strm_prefix`，升级后确认前缀没被旧配置带偏。

## 4. 回滚

```bash
podman stop drama-hub
# 1) 换回旧镜像/二进制
podman run -d --name drama-hub-old ... <旧镜像 tag>
# 2) 还原升级前的库（迁移不可逆，除非确认本次升级没有执行新迁移）
podman stop drama-hub-old
rm -f data/drama.db-wal data/drama.db-shm
cp backup/drama-<升级前时间>.db data/drama.db
podman start drama-hub-old
```

回滚后验证同 §3 四条。`stream_secret` 未变时，回滚前渲染出的 strm URL 仍然有效
（token 是无状态 HMAC，无过期、无 token 表，ADR-0003）；换过 secret 就重新渲染。

## 5. 备份与恢复

```bash
# 备份（WAL 模式必须用 .backup，不能只 cp 主文件）
mkdir -p backup
sqlite3 data/drama.db ".backup 'backup/drama-$(date +%F-%H%M).db'"

# 恢复（服务必须停）
podman stop drama-hub
rm -f data/drama.db-wal data/drama.db-shm
cp backup/drama-<时间>.db data/drama.db
podman start drama-hub
```

备份内容只有数据库（剧、集、合并编排、派生表、settings）。strm/nfo 是虚拟渲染的，
不需要备份；封面与关键帧这类 artifact 缓存丢了只是重新生成。

## 6. 日常操作

| 想做什么 | 入口 |
|---|---|
| 加/删剧目 | `/`（剧目页）；`POST /api/dramas`、`DELETE /api/dramas/{id}` |
| 合并集编排 | `/merges`；`POST/DELETE /api/dramas/{id}/merge-groups`、`/api/merge-groups/{id}` |
| 看同步与缓存现状、配置是否齐全 | `/status`；`/api/status` |
| 改配置 | 走 §2.4 的 SQL（当前无写入 API） |
| 看访问日志 | `podman logs -f drama-hub`（JSON，一行一个请求） |

删剧目级联删集、合并组、对外集号（`ON DELETE CASCADE`），不可撤销。

## 7. 已知限制（当前 `main`，2026-10-30 验收结论）

这些不是配置问题，是**还没实现或还没决策**的能力。逐条都有验收证据，见 `docs/ACCEPTANCE.md`。

| # | 限制 | 影响 |
|---|---|---|
| 1 | 同步不写 `episode.play_url`；解析真直链的 `provider.FetchDirectURL` 没有生产调用方；上游官网只给首集约 10 秒的预览片（实测 750,269 字节 / 10.07 s），第 2 集起页面里没有 `contentUrl` | 单集出流 `/s/{token}/e/e<id>` 恒 **502**；P6（App 回退）未实现，整集字节来源仍待决策 |
| 2 | 分片索引 `segment.Cutter.Index` 没有生产调用方（无端点、无 CLI、管理面也没有触发按钮） | `segment` 表恒空 → 合并集 playlist 与分片端点恒 **503 + Retry-After**，合并集播不了 |
| 3 | 虚拟文件树只渲染 `.strm` / `.nfo`，不渲染封面图 | Emby 里没有封面；集 `.nfo` 的 `runtime` 是 0（同步不采集时长），Emby 显示 0 分钟 |
| 4 | `settings` 只有读路径，没有写入 API | 配置必须按 §2.4 直写库；写错值绕过范围校验 |
| 5 | 缓存代理接法未验证：`http` 远端 + `rclone serve http` 实测所有路径 404（远端通、VFS 把根判成文件） | P4 的三条（缓存增长 / 无回源 / 回源兜底）连「代理在链路里」的前提都没有；`cache.mode` 只能是 `direct`（见 §2.5） |

## 8. 排障

| 现象 | 先查什么 |
|---|---|
| `PROPFIND /dav/` 恒 401 | `settings` 里 `webdav_user` / `webdav_pass` 是否有行；库直接写的是否漏了外层双引号（`value_json` 解码失败） |
| `/s/...` 恒 500 | 缺 `stream_secret`（≥ 32 字节）。响应体与日志都不会回显 secret |
| `/s/...` 恒 401 | 客户端用的 token 与当前 `stream_secret` 不匹配（换过 secret 就要重新渲染 strm）；token 是 43 字符无 `=` 的 base64url |
| `/s/.../e/e<id>` 502 | 该集 `play_url` 为空（当前所有集都为空，见 §7 第 1 条）；或上游 4xx/5xx/无 `Content-Length` |
| `/s/.../seg/...` 或合并集 playlist 503 | 该集还没索引（当前没有任何触发点，见 §7 第 2 条） |
| `/dav/` 下 404 | 路径不在虚拟树里；先 `rclone lsf --max-depth 3` 对照层级 |
| rclone mount 里文件读不出长度 | `PROPFIND` 的 `getcontentlength` 与 GET 的 `Content-Length` 都来自同一次渲染；对不上先看服务日志是否有 5xx |
| 切片报 502 / 日志里 ffmpeg stderr | ffmpeg 版本与 `env.lock` 不一致；切片有 30s/60s 硬超时，源站慢会命中超时 |
| 升级后某些请求 500 | 看 `PRAGMA user_version` 与新版迁移是否对齐；回滚按 §4 |
